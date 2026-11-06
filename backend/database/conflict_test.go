package database_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mattn/go-sqlite3"
	"oneimg/backend/database"
	"oneimg/backend/models"
	"oneimg/internal/testdb"
)

func TestIsWriteConflict(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"sqlite busy", sqlite3.Error{Code: sqlite3.ErrBusy}, true},
		{"sqlite busy snapshot", sqlite3.Error{Code: sqlite3.ErrBusy, ExtendedCode: sqlite3.ErrBusySnapshot}, true},
		{"sqlite locked", sqlite3.Error{Code: sqlite3.ErrLocked}, true},
		{"wrapped sqlite busy", fmt.Errorf("update: %w", sqlite3.Error{Code: sqlite3.ErrBusy}), true},
		{"sqlite constraint", sqlite3.Error{Code: sqlite3.ErrConstraint}, false},
		{"postgres serialization failure", &pgconn.PgError{Code: "40001"}, true},
		{"postgres deadlock", fmt.Errorf("tx: %w", &pgconn.PgError{Code: "40P01"}), true},
		{"postgres unique violation", &pgconn.PgError{Code: "23505"}, false},
		{"other error", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := database.IsWriteConflict(test.err); got != test.want {
				t.Fatalf("IsWriteConflict = %v, want %v", got, test.want)
			}
		})
	}
}

func TestJSONSetKeyRewritesOnlyThatKey(t *testing.T) {
	db := testdb.New(t)
	user := models.User{Username: "keyed", Password: "x", Role: models.RoleAdmin, Permission: models.Permission{Codes: []string{"user:list"}, Buckets: []int{1, 2, 3}}}
	if err := db.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	expr, err := database.JSONSetKey(db.DB, "permission", "buckets", []int{1, 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Model(&models.User{}).Where("id = ?", user.ID).Update("permission", expr).Error; err != nil {
		t.Fatal(err)
	}
	var stored models.User
	if err := db.DB.First(&stored, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(stored.Permission.Codes) != "[user:list]" || fmt.Sprint(stored.Permission.Buckets) != "[1 3]" {
		t.Fatalf("permission = %+v", stored.Permission)
	}
}
