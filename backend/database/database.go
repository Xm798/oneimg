package database

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/mattn/go-sqlite3"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"oneimg/backend/config"
)

type Database struct{ DB *gorm.DB }

var db *Database

func gormConfig() *gorm.Config {
	return &gorm.Config{
		SkipDefaultTransaction: true, DisableForeignKeyConstraintWhenMigrating: true,
		TranslateError: true, DisableAutomaticPing: true,
		// Driver errors and bound SQL can contain credentials and authentication data.
		Logger: logger.Discard,
	}
}
func NewDB(dialector gorm.Dialector) (*Database, error) {
	value, err := gorm.Open(dialector, gormConfig())
	if err != nil {
		return nil, errors.New("database open failed")
	}
	sqlDB, err := value.DB()
	if err != nil {
		return nil, errors.New("database connection unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, errors.New("database connection check failed")
	}
	return &Database{DB: value}, nil
}
func (d *Database) Close() error {
	connection, err := d.DB.DB()
	if err != nil {
		return err
	}
	return connection.Close()
}
func GetDB() *Database { return db }

// SetDB installs the process database for legacy media/settings helpers.
func SetDB(value *Database) { db = value }
func Open(cfg *config.Config) (*Database, error) {
	switch cfg.DatabaseType() {
	case "sqlite":
		if err := os.MkdirAll(filepath.Dir(cfg.SqlitePath), 0755); err != nil {
			return nil, errors.New("cannot create SQLite directory")
		}
		return NewDB(sqlite.Open(cfg.SqlitePath))
	case "postgres":
		return OpenPostgres(cfg, "public")
	default:
		return nil, errors.New("unsupported database type")
	}
}

// PostgreSQLConfig never includes the source URL in returned errors.
func PostgreSQLConfig(raw string) (*pgx.ConnConfig, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.Path == "" || u.Path == "/" {
		return nil, errors.New("DATABASE_URL must be a PostgreSQL URL with host and database")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL URL parameters")
	}
	for supplied := range query {
		for _, key := range []string{"host", "hostaddr", "port", "dbname", "user", "password", "options", "timezone", "search_path"} {
			if strings.EqualFold(supplied, key) {
				return nil, errors.New("connection identity and managed session settings must not override the PostgreSQL URL")
			}
		}
	}
	if strings.Contains(u.Hostname(), ",") {
		return nil, errors.New("a single PostgreSQL host is required")
	}
	if modes := query["sslmode"]; len(modes) > 1 {
		return nil, errors.New("sslmode must be specified once")
	}
	if query.Get("sslmode") == "" {
		query.Set("sslmode", "verify-full")
	}
	if query.Get("sslmode") != "verify-full" && query.Get("sslmode") != "disable" {
		return nil, errors.New("sslmode must be verify-full or disable")
	}
	if query.Get("connect_timeout") == "" {
		query.Set("connect_timeout", "5")
	}
	if len(query["connect_timeout"]) != 1 {
		return nil, errors.New("connect_timeout must be specified once")
	}
	timeout, err := strconv.Atoi(query.Get("connect_timeout"))
	if err != nil || timeout <= 0 || timeout > 300 {
		return nil, errors.New("connect_timeout must be between 1 and 300 seconds")
	}
	u.RawQuery = query.Encode()
	parsed, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection configuration")
	}
	for key := range parsed.RuntimeParams {
		if strings.EqualFold(key, "options") || strings.EqualFold(key, "timezone") || strings.EqualFold(key, "search_path") {
			delete(parsed.RuntimeParams, key)
		}
	}
	parsed.RuntimeParams["timezone"] = "UTC"
	return parsed, nil
}

var schemaName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// OpenPostgres also supports isolated schemas owned by the test factory.
func OpenPostgres(cfg *config.Config, schema string) (*Database, error) {
	if !schemaName.MatchString(schema) {
		return nil, errors.New("invalid database schema")
	}
	parsed, err := PostgreSQLConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	parsed.RuntimeParams["search_path"] = schema
	sqlDB := stdlib.OpenDB(*parsed)
	maxOpen, maxIdle := cfg.DBMaxOpenConns, cfg.DBMaxIdleConns
	lifetime, idleTime := cfg.DBConnMaxLifetime, cfg.DBConnMaxIdleTime
	if maxOpen == 0 {
		maxOpen, maxIdle = 10, 5
	}
	if maxOpen < 1 || maxIdle < 0 || maxIdle > maxOpen || lifetime < 0 || idleTime < 0 {
		_ = sqlDB.Close()
		return nil, errors.New("invalid PostgreSQL connection pool configuration")
	}
	if lifetime == 0 {
		lifetime = 30 * time.Minute
	}
	if idleTime == 0 {
		idleTime = 5 * time.Minute
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(lifetime)
	sqlDB.SetConnMaxIdleTime(idleTime)
	ctx, cancel := context.WithTimeout(context.Background(), parsed.ConnectTimeout)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("PostgreSQL connection check failed (%s)", ErrorCode(err))
	}
	value, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), gormConfig())
	if err != nil {
		_ = sqlDB.Close()
		return nil, errors.New("PostgreSQL initialization failed")
	}
	var version int
	var encoding string
	if err := value.WithContext(ctx).Raw("SELECT current_setting('server_version_num')::int").Scan(&version).Error; err != nil {
		_ = sqlDB.Close()
		return nil, errors.New("cannot inspect PostgreSQL version")
	}
	if err := value.WithContext(ctx).Raw("SHOW server_encoding").Scan(&encoding).Error; err != nil || encoding != "UTF8" || version < 160000 || version >= 190000 {
		_ = sqlDB.Close()
		return nil, errors.New("PostgreSQL 16-18 with UTF8 encoding is required")
	}
	return &Database{DB: value}, nil
}
func ErrorCode(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return "duplicate_key"
	}
	return "database_error"
}

// IsWriteConflict reports errors meaning another transaction got there first:
// the failed transaction rolled back and retrying it is safe.
func IsWriteConflict(err error) bool {
	var lite sqlite3.Error
	if errors.As(err, &lite) {
		return lite.Code == sqlite3.ErrBusy || lite.Code == sqlite3.ErrLocked
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code == "40001" || pg.Code == "40P01"
	}
	return false
}
func InitDB(cfg *config.Config) error {
	value, err := Open(cfg)
	if err != nil {
		return err
	}
	if err := MigrateSchema(value.DB); err != nil {
		_ = value.Close()
		return fmt.Errorf("database schema initialization failed (%s)", ErrorCode(err))
	}
	db = value
	log.Printf("数据库初始化完成: %s", cfg.DatabaseType())
	return nil
}
