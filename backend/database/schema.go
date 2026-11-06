package database

import (
	"encoding/json"
	"errors"
	"fmt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"math"
	"oneimg/backend/models"
	"time"
)

const SchemaVersion = "postgres_support_v1"
const SearchVersion = "unicode_search_v1"
const MigrationLock int64 = 526537410

type SchemaMigration struct {
	ID        string `gorm:"primaryKey;type:text"`
	AppliedAt time.Time
}

func (SchemaMigration) TableName() string { return "oneimg_schema_migrations" }
func Models() []any {
	return []any{&models.Tags{}, &models.User{}, &models.PasskeyCredential{}, &models.PersonalAccessToken{}, &models.Image{}, &models.Settings{}, &models.ImageToTags{}, &models.Buckets{}}
}
func SchemaEmpty(tx *gorm.DB) (bool, error) {
	var count int64
	err := tx.Raw(`SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema() AND c.relkind IN ('r','p','v','m','S','f')`).Scan(&count).Error
	if err != nil {
		return false, errors.New("cannot inspect target schema")
	}
	var types int64
	err = tx.Raw(`SELECT COUNT(*) FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace
 WHERE n.nspname=current_schema() AND t.typtype IN ('e','d','c','r','m')`).Scan(&types).Error
	if err != nil {
		return false, errors.New("cannot inspect target types")
	}
	var routines int64
	if err := tx.Raw(`SELECT COUNT(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
WHERE n.nspname=current_schema()`).Scan(&routines).Error; err != nil {
		return false, errors.New("cannot inspect target routines")
	}
	return count == 0 && types == 0 && routines == 0, nil
}
func MigrateSchema(value *gorm.DB) error {
	return value.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", MigrationLock).Error; err != nil {
				return errors.New("cannot acquire schema migration lock")
			}
			empty, err := SchemaEmpty(tx)
			if err != nil {
				return err
			}
			if !empty {
				if !tx.Migrator().HasTable(&SchemaMigration{}) {
					return errors.New("unsupported existing PostgreSQL schema")
				}
				var count int64
				if err := tx.Model(&SchemaMigration{}).Where("id = ?", SchemaVersion).Count(&count).Error; err != nil || count != 1 {
					return errors.New("unsupported existing PostgreSQL schema version")
				}
			}
		}
		for _, name := range []string{"unique_idx", "idx_users_role"} {
			if tx.Migrator().HasIndex(&models.User{}, name) {
				if err := tx.Migrator().DropIndex(&models.User{}, name); err != nil {
					return errors.New("cannot drop legacy role index")
				}
			}
		}
		if err := tx.AutoMigrate(append(Models(), &SchemaMigration{})...); err != nil {
			return fmt.Errorf("table migration failed (%s)", ErrorCode(err))
		}
		index := `CREATE UNIQUE INDEX IF NOT EXISTS idx_passkey_user_name ON passkey_credentials (user_id, name COLLATE NOCASE)`
		if tx.Dialector.Name() == "postgres" {
			index = `CREATE UNIQUE INDEX IF NOT EXISTS idx_passkey_user_name ON passkey_credentials (user_id, (translate(name, 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz') COLLATE "C"))`
		}
		if err := tx.Exec(index).Error; err != nil {
			return errors.New("cannot create Passkey name index")
		}
		// Runs on every start: an older build written to this schema after a
		// rollback leaves derived keys missing or stale despite the marker.
		if err := syncSearchKeys(tx); err != nil {
			return errors.New("search backfill failed")
		}
		if err := MarkMigration(tx, SearchVersion); err != nil {
			return err
		}
		return MarkMigration(tx, SchemaVersion)
	})
}
func MarkMigration(tx *gorm.DB, id string) error {
	var count int64
	if err := tx.Model(&SchemaMigration{}).Where("id = ?", id).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	if err := tx.Create(&SchemaMigration{ID: id, AppliedAt: time.Now().UTC()}).Error; err != nil {
		return errors.New("cannot record schema migration")
	}
	return nil
}

// syncSearchKeys rewrites only rows whose derived keys differ from the current rule.
func syncSearchKeys(tx *gorm.DB) error {
	var users []models.User
	if err := tx.Select("id", "username", "username_search").FindInBatches(&users, 250, func(batch *gorm.DB, _ int) error {
		for _, user := range users {
			key := models.SearchKey(user.Username)
			if user.UsernameSearch == key {
				continue
			}
			if err := tx.Model(&models.User{}).Where("id = ?", user.ID).UpdateColumn("username_search", key).Error; err != nil {
				return err
			}
		}
		return nil
	}).Error; err != nil {
		return err
	}
	var images []models.Image
	return tx.Select("id", "file_name", "original_filename", "url", "content_hash", "file_name_search", "original_filename_search", "url_search", "content_hash_search").FindInBatches(&images, 250, func(batch *gorm.DB, _ int) error {
		for _, image := range images {
			values := map[string]any{}
			for column, item := range map[string]struct{ original, current string }{
				"file_name_search":         {image.FileName, image.FileNameSearch},
				"original_filename_search": {image.OriginalFileName, image.OriginalFileNameSearch},
				"url_search":               {image.Url, image.URLSearch},
				"content_hash_search":      {image.ContentHash, image.ContentHashSearch},
			} {
				if key := models.SearchKey(item.original); key != item.current {
					values[column] = key
				}
			}
			if len(values) == 0 {
				continue
			}
			if err := tx.Model(&models.Image{}).Where("id = ?", image.Id).UpdateColumns(values).Error; err != nil {
				return err
			}
		}
		return nil
	}).Error
}

// SyncSequences preserves both allocated and historical SQLite high-water IDs.
func SyncSequences(tx *gorm.DB, highWater map[string]int64) error {
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	for _, table := range []string{"users", "tags", "buckets", "images", "settings", "passkey_credentials", "personal_access_tokens"} {
		var sequence string
		if err := tx.Raw("SELECT COALESCE(pg_get_serial_sequence(?, 'id'), '')", table).Scan(&sequence).Error; err != nil || sequence == "" {
			return errors.New("cannot inspect ID sequence")
		}
		var maximum int64
		if err := tx.Table(table).Select("COALESCE(MAX(id), 0)").Scan(&maximum).Error; err != nil {
			return err
		}
		maximum = max(maximum, highWater[table])
		if maximum == math.MaxInt64 {
			return errors.New("ID sequence exhausted")
		}
		var state struct {
			LastValue int64
			IsCalled  bool
		}
		var schema, name string
		if err := tx.Raw("SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.oid=?::regclass", sequence).Row().Scan(&schema, &name); err != nil {
			return err
		}
		quoted := tx.Statement.Quote(schema) + "." + tx.Statement.Quote(name)
		if err := tx.Raw("SELECT last_value, is_called FROM " + quoted).Scan(&state).Error; err != nil {
			return err
		}
		if maximum < state.LastValue || (maximum == state.LastValue && state.IsCalled) || maximum == 0 {
			continue
		}
		if err := tx.Exec("SELECT setval(?::regclass, ?, true)", sequence, maximum).Error; err != nil {
			return err
		}
	}
	return nil
}
func SearchLike(db *gorm.DB, column string) string {
	if db.Dialector.Name() == "postgres" {
		return column + ` COLLATE "C" LIKE ? ESCAPE ''`
	}
	return column + " LIKE ?"
}

// BinaryOrder sorts text by code point on both backends, independent of the
// PostgreSQL database locale.
func BinaryOrder(db *gorm.DB, column string) string {
	if db.Dialector.Name() == "postgres" {
		return column + ` COLLATE "C"`
	}
	return column + " COLLATE BINARY"
}

// JSONSetKey replaces one top-level key of a JSON column in place, leaving the
// other keys as the database currently holds them. key must be a trusted
// identifier; value is JSON-encoded and bound.
func JSONSetKey(db *gorm.DB, column, key string, value any) (clause.Expr, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return clause.Expr{}, err
	}
	if db.Dialector.Name() == "postgres" {
		return gorm.Expr("jsonb_set("+column+", '{"+key+"}', ?::jsonb)", string(encoded)), nil
	}
	return gorm.Expr("json_set("+column+", '$."+key+"', json(?))", string(encoded)), nil
}

// PasskeyNameSQL retains SQLite's ASCII-only name comparison on both backends.
func PasskeyNameSQL(db *gorm.DB, column string) string {
	if db.Dialector.Name() == "postgres" {
		return "translate(" + column + `, 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', 'abcdefghijklmnopqrstuvwxyz') COLLATE "C"`
	}
	return "LOWER(" + column + ")"
}

// CheckSequences reads sequence state without consuming an ID.
func CheckSequences(tx *gorm.DB, highWater map[string]int64) error {
	for _, table := range []string{"users", "tags", "buckets", "images", "settings", "passkey_credentials", "personal_access_tokens"} {
		var sequence string
		if err := tx.Raw("SELECT COALESCE(pg_get_serial_sequence(?, 'id'), '')", table).Scan(&sequence).Error; err != nil || sequence == "" {
			return errors.New("missing ID sequence")
		}
		var maximum int64
		if err := tx.Table(table).Select("COALESCE(MAX(id), 0)").Scan(&maximum).Error; err != nil {
			return err
		}
		maximum = max(maximum, highWater[table])
		var schema, name string
		if err := tx.Raw("SELECT n.nspname, c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.oid=?::regclass", sequence).Row().Scan(&schema, &name); err != nil {
			return err
		}
		var state struct {
			LastValue int64
			IsCalled  bool
		}
		if err := tx.Raw("SELECT last_value, is_called FROM " + tx.Statement.Quote(schema) + "." + tx.Statement.Quote(name)).Scan(&state).Error; err != nil {
			return err
		}
		if maximum == math.MaxInt64 || state.LastValue == math.MaxInt64 || state.LastValue < maximum || (state.LastValue == maximum && !state.IsCalled) {
			return errors.New("ID sequence is behind data or exhausted")
		}
	}
	return nil
}
