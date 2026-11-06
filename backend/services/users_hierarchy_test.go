package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"oneimg/backend/database"
	"oneimg/backend/models"
	"oneimg/internal/testdb"

	"gorm.io/gorm"
)

var managerCodes = []string{"user:list", "user:create", "user:delete", "user:role:update", "user:permission:update", "user:password:reset", "user:passkey:reset", "tag:create"}

func createHierarchyAdmin(t *testing.T, db *gorm.DB, name string, codes ...string) models.User {
	t.Helper()
	user := createServiceTestUser(t, db, name, "password", models.RoleAdmin)
	user.Permission.Codes = codes
	if err := db.Model(&user).Update("permission", user.Permission).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func storedUser(t *testing.T, db *gorm.DB, id int) models.User {
	t.Helper()
	var user models.User
	if err := db.First(&user, id).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

type auditRecorder struct{ events []UserAuditEvent }

func (r *auditRecorder) record(event UserAuditEvent) { r.events = append(r.events, event) }

func TestUserServiceDecidesOnTheActorRowInsideTheTransaction(t *testing.T) {
	db := newServiceTestDB(t)
	createServiceTestUser(t, db, "root", "password", models.RoleAdmin)
	actor := createHierarchyAdmin(t, db, "actor", "user:permission:update", "user:password:reset")
	target := createHierarchyAdmin(t, db, "target", "user:password:reset", "storage:delete")
	service := NewUserService(db)

	stale := actor
	stale.Permission.Codes = models.AllPermissionCodes()
	if _, err := service.SetPermissions(stale, target.ID, models.Permission{Codes: []string{"user:password:reset"}}); !errors.Is(err, ErrUserNotManageable) {
		t.Fatalf("SetPermissions with a stale actor err = %v", err)
	}
	if _, err := service.ResetPassword(stale, target.ID); !errors.Is(err, ErrUserNotManageable) {
		t.Fatalf("ResetPassword with a stale actor err = %v", err)
	}
	lower := createHierarchyAdmin(t, db, "lower", "user:password:reset")
	if _, err := service.SetPermissions(stale, lower.ID, models.Permission{Codes: []string{"storage:delete"}}); !errors.As(err, new(*PermissionNotHeldError)) {
		t.Fatalf("granting a code only the stale struct holds err = %v", err)
	}
	if got := storedUser(t, db, lower.ID).Permission.Codes; !reflect.DeepEqual(got, []string{"user:password:reset"}) {
		t.Fatalf("lower codes = %v", got)
	}
}

func TestUserServiceRejectsAnActorThatLostAccess(t *testing.T) {
	db := newServiceTestDB(t)
	createServiceTestUser(t, db, "root", "password", models.RoleAdmin)
	plain := createServiceTestUser(t, db, "plain", "password", models.RoleUser)
	service := NewUserService(db)

	demoted := createHierarchyAdmin(t, db, "demoted", managerCodes...)
	if err := db.Model(&demoted).Update("role", models.RoleUser).Error; err != nil {
		t.Fatal(err)
	}
	deleted := createHierarchyAdmin(t, db, "deleted", managerCodes...)
	if err := db.Delete(&deleted).Error; err != nil {
		t.Fatal(err)
	}
	for _, actor := range []models.User{demoted, deleted} {
		if _, err := service.ResetPassword(actor, plain.ID); !errors.Is(err, ErrActorNotPermitted) {
			t.Fatalf("%s reset err = %v", actor.Username, err)
		}
		if _, err := service.Create(actor, "created-"+actor.Username, "password", models.RoleUser); !errors.Is(err, ErrActorNotPermitted) {
			t.Fatalf("%s create err = %v", actor.Username, err)
		}
	}
	if _, err := service.ResetPassword(demoted, 9999); !errors.Is(err, ErrActorNotPermitted) {
		t.Fatalf("missing target with a demoted actor err = %v", err)
	}
}

func TestUserServiceCreateAdminRequiresRoleUpdate(t *testing.T) {
	db := newServiceTestDB(t)
	createServiceTestUser(t, db, "root", "password", models.RoleAdmin)
	creator := createHierarchyAdmin(t, db, "creator", "user:create")
	promoter := createHierarchyAdmin(t, db, "promoter", "user:create", "user:role:update")
	service := NewUserService(db)

	if _, err := service.Create(creator, "admin-one", "password", models.RoleAdmin); !errors.Is(err, ErrRoleUpdateRequired) {
		t.Fatalf("create admin without role update err = %v", err)
	}
	var count int64
	db.Model(&models.User{}).Where("username = ?", "admin-one").Count(&count)
	if count != 0 {
		t.Fatal("rejected admin was created")
	}
	if _, err := service.Create(creator, "user-one", "password", models.RoleUser); err != nil {
		t.Fatal(err)
	}
	created, err := service.Create(promoter, "admin-two", "password", models.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if len(storedUser(t, db, created.ID).Permission.Codes) != 0 {
		t.Fatal("new admin must start without codes")
	}
}

func TestResetPasswordIsAFullAccountReset(t *testing.T) {
	db := newServiceTestDB(t)
	createServiceTestUser(t, db, "root", "password", models.RoleAdmin)
	actor := createHierarchyAdmin(t, db, "actor", managerCodes...)
	target := createHierarchyAdmin(t, db, "target", "user:list")
	if err := db.Create(&models.PasskeyCredential{UserID: target.ID, Name: "key", CredentialID: "cred", CredentialData: "data"}).Error; err != nil {
		t.Fatal(err)
	}
	tokens := NewTokenService(db, "test-token-hmac-secret")
	token, err := tokens.Create(target.ID, CreateTokenInput{AuthVersion: target.AuthVersion, Name: "t", Scopes: []string{"images:read"}})
	if err != nil {
		t.Fatal(err)
	}
	audit := &auditRecorder{}
	service := NewUserService(db)
	service.SetAuditor(audit.record)

	plain, err := service.ResetPassword(actor, target.ID)
	if err != nil || plain == "" {
		t.Fatal(plain, err)
	}
	reset := storedUser(t, db, target.ID)
	if !reset.PasswordChangeRequired || reset.AuthVersion != target.AuthVersion+1 {
		t.Fatalf("reset user = %+v", reset)
	}
	var passkeys int64
	db.Model(&models.PasskeyCredential{}).Where("user_id = ?", target.ID).Count(&passkeys)
	if passkeys != 0 {
		t.Fatal("reset kept the target's passkeys")
	}
	if _, _, err := tokens.Authenticate(token.Plain); err == nil {
		t.Fatal("reset kept the target's PAT")
	}
	want := UserAuditEvent{Op: "user.password_reset", ActorID: actor.ID, TargetID: target.ID, Outcome: "ok"}
	if len(audit.events) != 1 || audit.events[0] != want {
		t.Fatalf("audit = %+v", audit.events)
	}
}

func TestUserServiceAuditsDenials(t *testing.T) {
	db := newServiceTestDB(t)
	createServiceTestUser(t, db, "root", "password", models.RoleAdmin)
	actor := createHierarchyAdmin(t, db, "actor", managerCodes...)
	lower := createHierarchyAdmin(t, db, "lower", "user:list")
	audit := &auditRecorder{}
	service := NewUserService(db)
	service.SetAuditor(audit.record)

	_, err := service.SetPermissions(actor, lower.ID, models.Permission{Codes: []string{"user:list", "storage:delete"}})
	var notHeld *PermissionNotHeldError
	if !errors.As(err, &notHeld) || notHeld.Code != "storage:delete" {
		t.Fatalf("err = %v", err)
	}
	if err := service.Delete(actor, models.SuperAdminID); !errors.Is(err, ErrProtectedUser) {
		t.Fatal(err)
	}
	want := []UserAuditEvent{
		{Op: "user.permissions_update", ActorID: actor.ID, TargetID: lower.ID, Outcome: "denied", Code: "permission_not_held"},
		{Op: "user.delete", ActorID: actor.ID, TargetID: models.SuperAdminID, Outcome: "denied", Code: "protected_user"},
	}
	if !reflect.DeepEqual(audit.events, want) {
		t.Fatalf("audit = %+v", audit.events)
	}
}

func TestUserServiceReportsWriteContentionAsConcurrentUpdate(t *testing.T) {
	if _, ok := os.LookupEnv("ONEIMG_TEST_POSTGRES_URL"); ok {
		t.Skip("holds an SQLite write lock")
	}
	// mattn/go-sqlite3 waits 5 s for a lock by default; the contention is
	// deliberate here, so fail fast.
	opened, cleanup, err := testdb.Open(filepath.Join(t.TempDir(), "oneimg.db") + "?_busy_timeout=0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	db := opened.DB
	if err := database.MigrateSchema(db); err != nil {
		t.Fatal(err)
	}
	createServiceTestUser(t, db, "root", "password", models.RoleAdmin)
	actor := createHierarchyAdmin(t, db, "actor", managerCodes...)
	target := createHierarchyAdmin(t, db, "target", "user:list")
	audit := &auditRecorder{}
	service := NewUserService(db)
	service.SetAuditor(audit.record)

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	holder, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetPermissions(actor, target.ID, models.Permission{Codes: []string{"tag:create"}}); !errors.Is(err, ErrConcurrentUpdate) {
		t.Fatalf("SetPermissions err = %v", err)
	}
	if _, err := service.ResetPassword(actor, target.ID); !errors.Is(err, ErrConcurrentUpdate) {
		t.Fatalf("ResetPassword err = %v", err)
	}
	if _, err := holder.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	stored := storedUser(t, db, target.ID)
	if !reflect.DeepEqual(stored.Permission.Codes, []string{"user:list"}) || stored.AuthVersion != target.AuthVersion {
		t.Fatalf("contended writes left changes: %+v", stored)
	}
	for _, event := range audit.events {
		if event.Outcome != "conflict" || event.Code != "concurrent_update_conflict" {
			t.Fatalf("audit = %+v", audit.events)
		}
	}
}

func TestBucketDeleteWaitsForAConcurrentRevocation(t *testing.T) {
	if _, ok := os.LookupEnv("ONEIMG_TEST_POSTGRES_URL"); !ok {
		t.Skip("needs PostgreSQL row locks")
	}
	db := newServiceTestDB(t)
	createServiceTestUser(t, db, "root", "password", models.RoleAdmin)
	bucket := models.Buckets{Name: "doomed", Type: "local", Config: map[string]any{}}
	if err := db.Create(&bucket).Error; err != nil {
		t.Fatal(err)
	}
	target := createHierarchyAdmin(t, db, "target", "storage:delete", "tag:create")
	if err := db.Model(&target).Update("permission", models.Permission{Codes: []string{"storage:delete", "tag:create"}, Buckets: []int{bucket.Id}}).Error; err != nil {
		t.Fatal(err)
	}

	revocation := db.Begin()
	if err := revocation.Exec("SELECT id FROM users WHERE id = ? FOR UPDATE", target.ID).Error; err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- NewStorageService(db).deleteBucketRecords(bucket.Id, bucket) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int64
		if err := db.Raw("SELECT COUNT(*) FROM pg_locks WHERE NOT granted").Scan(&waiting).Error; err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bucket deletion did not wait for the locked user row")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := revocation.Model(&models.User{}).Where("id = ?", target.ID).Update("permission", models.Permission{Codes: []string{"tag:create"}, Buckets: []int{bucket.Id}}).Error; err != nil {
		t.Fatal(err)
	}
	if err := revocation.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stored := storedUser(t, db, target.ID)
	if !reflect.DeepEqual(stored.Permission.Codes, []string{"tag:create"}) || len(stored.Permission.Buckets) != 0 {
		t.Fatalf("permission = %+v", stored.Permission)
	}
}

func TestBucketDeleteKeepsRevokedCodes(t *testing.T) {
	db := newServiceTestDB(t)
	root := createServiceTestUser(t, db, "root", "password", models.RoleAdmin)
	bucket := models.Buckets{Name: "doomed", Type: "local", Config: map[string]any{}}
	if err := db.Create(&bucket).Error; err != nil {
		t.Fatal(err)
	}
	target := createHierarchyAdmin(t, db, "target", "storage:delete", "tag:create")
	if err := db.Model(&target).Update("permission", models.Permission{Codes: []string{"storage:delete", "tag:create"}, Buckets: []int{bucket.Id}}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := NewUserService(db).SetPermissions(root, target.ID, models.Permission{Codes: []string{"tag:create"}, Buckets: []int{bucket.Id}}); err != nil {
		t.Fatal(err)
	}
	if err := NewStorageService(db).deleteBucketRecords(bucket.Id, bucket); err != nil {
		t.Fatal(err)
	}
	stored := storedUser(t, db, target.ID)
	if !reflect.DeepEqual(stored.Permission.Codes, []string{"tag:create"}) || len(stored.Permission.Buckets) != 0 {
		t.Fatalf("permission = %+v", stored.Permission)
	}
}
