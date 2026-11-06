package dbmigrate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"oneimg/backend/app"
	"oneimg/backend/config"
	"oneimg/backend/database"
	"oneimg/backend/models"
	"oneimg/backend/services"
	"oneimg/backend/utils/passkeys"
	"oneimg/backend/utils/secureconfig"
	"oneimg/internal/testdb"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func sourceFixture(t *testing.T) (string, *config.Config, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.db")
	cfg := &config.Config{DBType: "postgres", ConfigSecret: "migration-test-only-config-secret"}
	old := config.App
	config.App = cfg
	t.Cleanup(func() { config.App = old })
	db, err := database.Open(&config.Config{SqlitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.MigrateSchema(db.DB); err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("migration-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	avatar := "avatars/kept.webp"
	for _, user := range []models.User{
		{ID: 1, Role: models.RoleAdmin, Username: "Straße", Password: string(hash), AuthVersion: 7, Avatar: &avatar, Permission: models.Permission{Codes: models.AllPermissionCodes(), Buckets: []int{1, 900}}},
		{ID: 2, Role: models.RoleUser, Username: "restricted", Password: string(hash), AuthVersion: 11, PasswordChangeRequired: true, BootstrapCodeHash: &avatar},
	} {
		if err := db.DB.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	}
	encrypted, err := secureconfig.EncryptBucketConfigValues(map[string]any{"webdav_pass": "never-output-this", "webdav_url": "https://example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []models.Buckets{
		{Id: 1, Name: "local", Type: "default", Config: map[string]any{}, Usage: 12},
		{Id: 900, Name: "encrypted", Type: "webdav", Config: encrypted, Disabled: true, Capacity: 1000, Usage: 45},
	} {
		if err := db.DB.Create(&bucket).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DB.Create(&models.Settings{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Model(&models.Settings{}).Where("id=1").UpdateColumns(map[string]any{"save_webp": false, "thumbnail": false, "main_image_quality": 0, "file_name": "", "seo_title": ""}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Exec("UPDATE users SET permission = NULL WHERE id=2").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 10, 20, 30, 123456789, time.FixedZone("test", 8*3600))
	size := int64(12)
	for _, image := range []models.Image{
		{Id: 1 << 33, Url: "/uploads/original#?.png", Thumbnail: "/thumbnails/kept.webp", FileName: "Στραße.png", OriginalFileName: "e\u0301.png", FileSize: 10, StorageBytes: &size, CreatedAt: now},
		{Id: 9, Url: "/uploads/original#?.png", FileName: "historical orphan", UserId: 999, BucketId: 900, FileSize: 45, CreatedAt: now},
	} {
		if err := db.DB.Create(&image).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DB.Create(&models.Tags{Id: 300, Name: "tag"}).Error; err != nil {
		t.Fatal(err)
	}
	for _, link := range []models.ImageToTags{{ImageId: 1 << 33, TagId: 300}, {ImageId: 9, TagId: 999}} {
		if err := db.DB.Create(&link).Error; err != nil {
			t.Fatal(err)
		}
	}
	tokens := services.NewTokenService(db.DB, cfg.ConfigSecret)
	active, err := tokens.Create(1, services.CreateTokenInput{AuthVersion: 7, Name: "active", Scopes: []string{"images:read"}})
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := tokens.Create(1, services.CreateTokenInput{AuthVersion: 7, Name: "revoked", Scopes: []string{"images:read"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tokens.Revoke(1, revoked.Token.ID); err != nil {
		t.Fatal(err)
	}
	key, data, err := passkeys.EncodeCredential(&webauthn.Credential{ID: []byte("migration-key"), PublicKey: []byte("test-public-key")})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Create(&models.PasskeyCredential{UserID: 1, Name: "key", CredentialID: key, CredentialData: data}).Error; err != nil {
		t.Fatal(err)
	}
	sequence := db.DB.Exec("UPDATE sqlite_sequence SET seq = ? WHERE name = 'personal_access_tokens'", int64(1<<33)+99)
	if sequence.Error != nil || sequence.RowsAffected != 1 {
		t.Fatal("source high-water fixture missing", sequence.Error)
	}
	if err := db.DB.Exec("UPDATE images SET file_name_search = 'stale', original_filename_search='stale', content_hash_search='stale'").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Exec("UPDATE images SET original_filename=NULL,content_hash=NULL WHERE id=9").Error; err != nil {
		t.Fatal(err)
	}
	return path, cfg, active.Plain, revoked.Plain
}

func postgresTarget(t *testing.T) *gorm.DB {
	t.Helper()
	if _, ok := os.LookupEnv("ONEIMG_TEST_POSTGRES_URL"); !ok {
		t.Skip("PostgreSQL integration explicitly disabled")
	}
	db, cleanup, err := testdb.Open(filepath.Join(t.TempDir(), "unused.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return db.DB
}

func TestSourcePreflightReadOnlyAndInvalidData(t *testing.T) {
	path, cfg, _, _ := sourceFixture(t)
	source, err := openSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := source.DB.Exec("DELETE FROM users").Error; err == nil {
		t.Fatal("source was writable")
	}
	tx := sourceTransaction(source)
	defer tx.Rollback()
	specs, err := tables(tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkSource(tx, specs, Options{Output: &bytes.Buffer{}}, cfg); err != nil {
		t.Fatal(err)
	}
	for _, spec := range specs {
		rows, err := selectRows(tx, spec, true)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			if _, _, err := scanRow(rows, spec, true); err != nil {
				t.Fatal(err)
			}
		}
		_ = rows.Close()
	}
}

func TestSQLiteToPostgresFullMigration(t *testing.T) {
	target := postgresTarget(t)
	path, cfg, active, revoked := sourceFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	options := Options{Source: path, Output: &output, DryRun: true}
	if err := run(cfg, options, target); err != nil {
		t.Fatal(err)
	}
	empty, err := database.SchemaEmpty(target)
	if err != nil || !empty {
		t.Fatal("dry-run created objects", err)
	}
	options.DryRun = false
	if err := run(cfg, options, target); err != nil {
		t.Fatal(err)
	}
	options.VerifyOnly = true
	if err := run(cfg, options, target); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("source was modified", err)
	}
	if strings.Contains(output.String(), "never-output-this") || strings.Contains(output.String(), active) {
		t.Fatal("migration output exposed sensitive data")
	}
	if !strings.Contains(output.String(), "fields=") || strings.Contains(output.String(), "fields=0\n") {
		t.Fatal("microsecond normalization not reported", output.String())
	}
	if !strings.Contains(output.String(), "historical dangling") {
		t.Fatal("historical dangling references not reported")
	}
	tokens := services.NewTokenService(target, cfg.ConfigSecret)
	if _, user, err := tokens.Authenticate(active); err != nil || user.AuthVersion != 7 {
		t.Fatal("active PAT changed", err)
	}
	if _, _, err := tokens.Authenticate(revoked); !errors.Is(err, services.ErrInvalidToken) {
		t.Fatal("revoked PAT restored", err)
	}
	var admin models.User
	if err := target.First(&admin, 1).Error; err != nil || bcrypt.CompareHashAndPassword([]byte(admin.Password), []byte("migration-password")) != nil {
		t.Fatal("password hash changed", err)
	}
	var credential models.PasskeyCredential
	if err := target.First(&credential).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := passkeys.DecodeCredential(credential); err != nil {
		t.Fatal(err)
	}
	var settings models.Settings
	if err := target.First(&settings).Error; err != nil || settings.SaveWebp || settings.Thumbnail || settings.MainImageQuality != 0 || settings.FileName != "" {
		t.Fatal("zero values changed", err)
	}
	t.Chdir(t.TempDir())
	cfg.StateDir = t.TempDir()
	cfg.AppURL = "http://localhost:8080"
	cfg.ImageMaxPixels = 40000000
	cfg.ImageMaxDimension = 16384
	system, err := app.InitializeDatabase(cfg, &database.Database{DB: target})
	if err != nil {
		t.Fatal("imported database could not start", err)
	}
	if _, _, err := system.Services.Tokens.Authenticate(active); err != nil {
		t.Fatal("application rejected migrated active PAT", err)
	}
	if _, _, err := system.Services.Tokens.Authenticate(revoked); !errors.Is(err, services.ErrInvalidToken) {
		t.Fatal("application restored revoked PAT", err)
	}
	image := models.Image{FileName: "after"}
	if err := target.Create(&image).Error; err != nil || int64(image.Id) <= int64(1<<33) {
		t.Fatal("high-water sequence lost", image.Id, err)
	}
	newToken, err := system.Services.Tokens.Create(1, services.CreateTokenInput{AuthVersion: 7, Name: "after migration", Scopes: []string{"images:read"}})
	if err != nil || int64(newToken.Token.ID) <= int64(1<<33)+99 {
		t.Fatal("SQLite AUTOINCREMENT high-water lost", err)
	}
	options.VerifyOnly = false
	if err := run(cfg, options, target); err == nil {
		t.Fatal("nonempty target accepted")
	}
}

func TestMigrationRollsBackAndUncertainCommit(t *testing.T) {
	path, cfg, _, _ := sourceFixture(t)
	for _, stage := range []string{"copied:users", "copied:personal_access_tokens", "before-verify"} {
		t.Run(stage, func(t *testing.T) {
			target := postgresTarget(t)
			err := run(cfg, Options{Source: path, hook: func(current string, _ *gorm.DB) error {
				if current == stage {
					return errors.New("injected")
				}
				return nil
			}}, target)
			if err == nil {
				t.Fatal("injection ignored")
			}
			empty, err := database.SchemaEmpty(target)
			if err != nil || !empty {
				t.Fatal("failed import not rolled back", err)
			}
		})
	}
	for _, scenario := range []string{"duplicate key", "corrupt copied value"} {
		t.Run(scenario, func(t *testing.T) {
			target := postgresTarget(t)
			err := run(cfg, Options{Source: path, hook: func(stage string, tx *gorm.DB) error {
				if stage != "before-verify" {
					return nil
				}
				if scenario == "duplicate key" {
					return tx.Exec("INSERT INTO tags (id,name) VALUES (9999,'tag')").Error
				}
				return tx.Exec("UPDATE images SET thumbnail='different' WHERE id=9").Error
			}}, target)
			if err == nil {
				t.Fatal("injected failure accepted")
			}
			empty, err := database.SchemaEmpty(target)
			if err != nil || !empty {
				t.Fatal("injected failure did not roll back target", err)
			}
		})
	}
	t.Run("uncertain commit", func(t *testing.T) {
		target := postgresTarget(t)
		err := run(cfg, Options{Source: path, commit: func() error { return errors.New("connection lost") }}, target)
		var failure *Failure
		if !errors.As(err, &failure) || failure.Code != 4 {
			t.Fatal("not uncertain", err)
		}
		if err := run(cfg, Options{Source: path, VerifyOnly: true}, target); err != nil {
			t.Fatal("uncertain result could not be verified", err)
		}
	})
	t.Run("server termination during commit", func(t *testing.T) {
		target := postgresTarget(t)
		err := run(cfg, Options{Source: path, commit: func() error {
			return &pgconn.PgError{Code: "57P01", Severity: "FATAL"}
		}}, target)
		var failure *Failure
		if !errors.As(err, &failure) || failure.Code != 4 {
			t.Fatal("server termination incorrectly treated as a definite rollback", err)
		}
		if err := run(cfg, Options{Source: path, VerifyOnly: true}, target); err != nil {
			t.Fatal("server termination result could not be verified", err)
		}
	})
	t.Run("comparison failure", func(t *testing.T) {
		target := postgresTarget(t)
		if err := run(cfg, Options{Source: path}, target); err != nil {
			t.Fatal(err)
		}
		if err := target.Exec("UPDATE images SET url = 'changed' WHERE id=9").Error; err != nil {
			t.Fatal(err)
		}
		if err := run(cfg, Options{Source: path, VerifyOnly: true}, target); err == nil {
			t.Fatal("mismatch accepted")
		}
	})
}

func TestInvalidSourceStopsBeforeTargetDDL(t *testing.T) {
	for _, statement := range []string{
		"UPDATE buckets SET usage=-1 WHERE id=1",
		"UPDATE images SET created_at='not a time' WHERE id=9",
		"UPDATE tags SET name=printf('%060d',1)",
		"UPDATE users SET permission='invalid' WHERE id=2",
		`UPDATE users SET permission='{"codes":"invalid","buckets":[]}' WHERE id=2`,
		`UPDATE users SET permission='{"codes":[],"buckets":["invalid"]}' WHERE id=2`,
		"UPDATE images SET url=char(0) WHERE id=9",
		"UPDATE images SET url=cast(x'ff' as text) WHERE id=9",
		"ALTER TABLE images ADD COLUMN unknown_extension TEXT",
		"CREATE TABLE unknown_extension(id integer)",
		"CREATE VIEW unknown_extension AS SELECT id FROM users",
		"CREATE INDEX unknown_extension ON images(file_name)",
		"CREATE UNIQUE INDEX name_collision ON images(file_name)",
		"DROP INDEX idx_passkey_user_name",
		"ALTER TABLE oneimg_schema_migrations ADD COLUMN unknown_extension TEXT",
		"INSERT INTO oneimg_schema_migrations(id) VALUES ('future_version')",
		"DELETE FROM oneimg_schema_migrations",
	} {
		t.Run(statement, func(t *testing.T) {
			target := postgresTarget(t)
			path, cfg, _, _ := sourceFixture(t)
			db, err := database.Open(&config.Config{SqlitePath: path})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.DB.Exec(statement).Error; err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			if err := run(cfg, Options{Source: path}, target); err == nil {
				t.Fatal("invalid source accepted")
			}
			empty, err := database.SchemaEmpty(target)
			if err != nil || !empty {
				t.Fatal("invalid source created target objects", err)
			}
		})
	}
	t.Run("wrong key", func(t *testing.T) {
		target := postgresTarget(t)
		path, cfg, _, _ := sourceFixture(t)
		cfg.ConfigSecret = "wrong"
		if err := run(cfg, Options{Source: path}, target); err == nil {
			t.Fatal("wrong secret accepted")
		}
	})
	t.Run("WAL", func(t *testing.T) {
		path, _, _, _ := sourceFixture(t)
		if err := os.WriteFile(path+"-wal", []byte("unmerged"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := openSource(path); err == nil {
			t.Fatal("unmerged WAL accepted")
		}
	})
}

func TestJSONAndTimeValidation(t *testing.T) {
	for _, value := range []string{`{"x":"\u0000"}`, `{"x":"\ud800"}`, `{"x":"\udc00"}`} {
		if validJSONStrings([]byte(value)) {
			t.Fatal("invalid JSONB string accepted", value)
		}
	}
	if !validJSONStrings([]byte(`{"x":"\ud83d\ude00","literal":"\\u0000"}`)) {
		t.Fatal("valid JSON escapes rejected")
	}
	a := map[string]any{"permission": `{"n":1.0,"codes":[]}`}
	b := map[string]any{"permission": `{"codes":[],"n":1}`}
	specs, err := tables(testdb.New(t).DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range specs {
		if spec.name != "users" {
			continue
		}
		da, err := digest(spec, a)
		if err != nil {
			t.Fatal(err)
		}
		db, err := digest(spec, b)
		if err != nil || da != db {
			t.Fatal("JSONB semantic equality lost", err)
		}
		nullJSON, _ := digest(spec, map[string]any{"permission": "null"})
		nullSQL, _ := digest(spec, map[string]any{"permission": nil})
		if nullJSON == nullSQL {
			t.Fatal("SQL NULL was confused with JSON null")
		}
		number, _ := digest(spec, map[string]any{"permission": `{"value":1}`})
		object, _ := digest(spec, map[string]any{"permission": `{"value":{"Number":"1"}}`})
		if number == object {
			t.Fatal("JSON object was confused with a normalized number")
		}
	}
}

func TestOrphanReportIgnoresNullBucketPermissions(t *testing.T) {
	db, err := database.Open(&config.Config{SqlitePath: filepath.Join(t.TempDir(), "source.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.MigrateSchema(db.DB); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Create(&models.Buckets{Id: 1, Name: "local", Type: "default", Config: map[string]any{}}).Error; err != nil {
		t.Fatal(err)
	}
	for id, permission := range map[int]any{
		1: `{"codes":null,"buckets":null}`,
		2: `{"codes":[],"buckets":[]}`,
		3: `{"codes":[],"buckets":[null]}`,
		4: nil,
		5: `{"codes":[],"buckets":[1,404]}`,
	} {
		if err := db.DB.Create(&models.User{ID: id, Username: "user" + string(rune('0'+id)), Password: "hash"}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.DB.Exec("UPDATE users SET permission = ? WHERE id = ?", permission, id).Error; err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := reportOrphans(db.DB, &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "historical dangling references users.permission.buckets references=1 preserved\n" {
		t.Fatalf("unexpected dangling reference report: %q", got)
	}
}

func TestCLIRejectsInvalidConfigurationWithoutCreatingFiles(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("ONEIMG_ENV_FILE", filepath.Join(root, "absent", ".env"))
	t.Setenv("DB_TYPE", "unknown")
	for _, args := range [][]string{
		nil,
		{"--source", "/backup/oneimg.db", "--dry-run", "--verify-only"},
		{"--source", "/backup/oneimg.db", "--overwrite"},
		{"--source", "/backup/oneimg.db", "--dry-run"},
	} {
		if code := RunCLI(args); code != 2 {
			t.Fatal("invalid CLI invocation did not return configuration error", code)
		}
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 0 {
		t.Fatal("migration CLI created local configuration or state", err)
	}
}

func TestSourceWithUndeclaredImageIndexRejected(t *testing.T) {
	path, cfg, _, _ := sourceFixture(t)
	writer, err := database.Open(&config.Config{SqlitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.DB.Exec("CREATE INDEX idx_images_undeclared ON images (file_name)").Error; err != nil {
		t.Fatal(err)
	}
	if err := writer.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error; err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	source, err := openSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	tx := sourceTransaction(source)
	defer tx.Rollback()
	specs, err := tables(tx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = checkSource(tx, specs, Options{Output: &bytes.Buffer{}}, cfg)
	var failure *Failure
	if !errors.As(err, &failure) || failure.Code != 3 || !strings.Contains(failure.Stage, "unrecognized indexes") {
		t.Fatalf("undeclared index accepted: %v", err)
	}
}
