package database_test

import (
	"reflect"
	"testing"

	"oneimg/backend/database"
	"oneimg/backend/models"
	"oneimg/internal/testdb"

	"gorm.io/gorm"
)

// Index names and column order are a contract: bucket maintenance relies on
// idx_images_bucket_id_id being (bucket_id, id).
var wantImageIndexes = map[string][]string{
	"idx_images_url":           {"url"},
	"idx_images_thumbnail":     {"thumbnail"},
	"idx_images_bucket_id_id":  {"bucket_id", "id"},
	"idx_images_user_created":  {"user_id", "created_at", "id"},
	"idx_images_created_at_id": {"created_at", "id"},
}

func TestImageIndexTagsParsed(t *testing.T) {
	statement := &gorm.Statement{DB: testdb.New(t).DB}
	if err := statement.Parse(&models.Image{}); err != nil {
		t.Fatal(err)
	}
	parsed := map[string][]string{}
	for _, index := range statement.Schema.ParseIndexes() {
		for _, field := range index.Fields {
			parsed[index.Name] = append(parsed[index.Name], field.DBName)
		}
	}
	for name, columns := range wantImageIndexes {
		if !reflect.DeepEqual(parsed[name], columns) {
			t.Fatalf("%s parsed as %v, want %v", name, parsed[name], columns)
		}
	}
}

func imageIndexColumns(t *testing.T, db *gorm.DB, name string) []string {
	t.Helper()
	var columns []string
	if db.Dialector.Name() == "sqlite" {
		if err := db.Raw("SELECT name FROM pragma_index_info(?) ORDER BY seqno", name).Scan(&columns).Error; err != nil {
			t.Fatal(err)
		}
		return columns
	}
	err := db.Raw(`SELECT a.attname FROM pg_index i
JOIN pg_class c ON c.oid = i.indexrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
WHERE c.relname = ? AND n.nspname = current_schema() AND i.indisvalid
ORDER BY k.ord`, name).Scan(&columns).Error
	if err != nil {
		t.Fatal(err)
	}
	return columns
}

func TestImageIndexesCreatedIdempotently(t *testing.T) {
	db := testdb.New(t).DB
	if err := db.Create(&models.Image{Url: "/uploads/old.png", FileName: "old.png"}).Error; err != nil {
		t.Fatal(err)
	}
	// Recreates the schema an older release left behind.
	for name := range wantImageIndexes {
		if err := db.Migrator().DropIndex(&models.Image{}, name); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := database.MigrateSchema(db); err != nil {
			t.Fatal("migration failed:", err)
		}
	}
	for name, columns := range wantImageIndexes {
		if !db.Migrator().HasIndex(&models.Image{}, name) {
			t.Fatalf("missing index %s", name)
		}
		if got := imageIndexColumns(t, db, name); !reflect.DeepEqual(got, columns) {
			t.Fatalf("%s has columns %v, want %v", name, got, columns)
		}
	}
}

func TestImageURLNotUnique(t *testing.T) {
	db := testdb.New(t).DB
	rows := []models.Image{{Url: "/uploads/shared.png", FileName: "a.png"}, {Url: "/uploads/shared.png", FileName: "b.png"}}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal("duplicate url rejected:", err)
	}
}
