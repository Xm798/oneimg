package services

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"oneimg/backend/models"
	"oneimg/internal/testdb"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func newServiceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testdb.New(t).DB
}

func createServiceTestUser(t *testing.T, db *gorm.DB, username, password string, role int) models.User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	user := models.User{Username: username, Password: string(hash), Role: role, Permission: models.Permission{Codes: []string{}, Buckets: []int{}}}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	return user
}

func TestPersonalAccessTokenLifecycle(t *testing.T) {
	db := newServiceTestDB(t)
	user := createServiceTestUser(t, db, "token-user", "correct-password", models.RoleAdmin)
	service := NewTokenService(db, "test-token-hmac-secret")
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	created, err := service.Create(user.ID, CreateTokenInput{
		AuthVersion: user.AuthVersion,
		Name:        "automation", Scopes: []string{"images:read", "images:read", "tags:read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^oneimg_pat_[A-Za-z0-9_-]{12}_[A-Za-z0-9_-]{43}$`).MatchString(created.Plain) {
		t.Fatalf("unexpected token format: %q", created.Plain)
	}
	if created.Token.SecretHash == created.Plain || len(created.Token.SecretHash) != sha256HexLength {
		t.Fatalf("token hash was not stored as SHA-256 hex: %q", created.Token.SecretHash)
	}
	if created.Token.ExpiresAt == nil || !created.Token.ExpiresAt.Equal(now.AddDate(0, 0, 90)) {
		t.Fatalf("default expiry = %v", created.Token.ExpiresAt)
	}
	if len(created.Token.Scopes) != 2 {
		t.Fatalf("normalized scopes = %#v", created.Token.Scopes)
	}

	var stored models.PersonalAccessToken
	if err := db.First(&stored, created.Token.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.SecretHash != created.Token.SecretHash || stored.SecretHash == created.Plain {
		t.Fatal("database must contain only the HMAC digest")
	}
	authenticated, authenticatedUser, err := service.Authenticate(created.Plain)
	if err != nil {
		t.Fatal(err)
	}
	if authenticatedUser.ID != user.ID || authenticated.LastUsedAt == nil || !authenticated.LastUsedAt.Equal(now) {
		t.Fatalf("authenticated token = %#v, user = %#v", authenticated, authenticatedUser)
	}

	never := 0
	permanent, err := service.Create(user.ID, CreateTokenInput{AuthVersion: user.AuthVersion, Name: "permanent", Scopes: []string{"stats:read"}, ExpirationDays: &never})
	if err != nil {
		t.Fatal(err)
	}
	if permanent.Token.ExpiresAt != nil {
		t.Fatalf("permanent token expiry = %v", permanent.Token.ExpiresAt)
	}

	if err := service.Revoke(user.ID, created.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Authenticate(created.Plain); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token error = %v", err)
	}
	if err := db.Delete(&user).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Authenticate(permanent.Plain); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("deleted user token error = %v", err)
	}
}

const sha256HexLength = 64

func TestParseTokenPrefixAllowsBase64URLUnderscore(t *testing.T) {
	prefix := "abc_defghijk"
	plain := personalTokenPrefix + prefix + "_" + strings.Repeat("A", personalTokenSecretLength)

	parsed, ok := parseTokenPrefix(plain)
	if !ok || parsed != prefix {
		t.Fatalf("parseTokenPrefix(%q) = %q, %t", plain, parsed, ok)
	}

	for _, malformed := range []string{
		personalTokenPrefix + prefix + strings.Repeat("A", personalTokenSecretLength),
		personalTokenPrefix + prefix + "_" + strings.Repeat("A", personalTokenSecretLength-1),
		personalTokenPrefix + prefix + "_" + strings.Repeat("A", personalTokenSecretLength+1),
	} {
		if parsed, ok := parseTokenPrefix(malformed); ok {
			t.Fatalf("malformed token parsed as %q", parsed)
		}
	}
}

func TestPersonalAccessTokenValidation(t *testing.T) {
	db := newServiceTestDB(t)
	user := createServiceTestUser(t, db, "scope-user", "correct-password", models.RoleAdmin)
	service := NewTokenService(db, "test-token-hmac-secret")
	for _, test := range []struct {
		name  string
		input CreateTokenInput
		err   error
	}{
		{"missing name", CreateTokenInput{Scopes: []string{"images:read"}}, ErrInvalidTokenName},
		{"missing scope", CreateTokenInput{Name: "test"}, ErrTokenScopesRequired},
		{"unknown scope", CreateTokenInput{Name: "test", Scopes: []string{"admin:all"}}, ErrInvalidTokenScope},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.Create(user.ID, test.input)
			if !errors.Is(err, test.err) {
				t.Fatalf("error = %v, want %v", err, test.err)
			}
		})
	}
}

func TestImageTagTransactionIsAtomicAndIdempotent(t *testing.T) {
	db := newServiceTestDB(t)
	image := models.Image{FileName: "image.png", BucketId: 1, UserId: 1}
	tag := models.Tags{Name: "valid"}
	if err := db.Create(&image).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&tag).Error; err != nil {
		t.Fatal(err)
	}
	service := NewTagService(db)

	if err := service.UpdateImageTags([]int{image.Id}, []int{tag.Id, tag.Id + 1000}, nil); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("invalid batch error = %v", err)
	}
	var count int64
	if err := db.Model(&models.ImageToTags{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid batch wrote %d links", count)
	}

	for range 2 {
		if err := service.UpdateImageTags([]int{image.Id}, []int{tag.Id}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&models.ImageToTags{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("idempotent update wrote %d links", count)
	}
}

func TestSettingsUpdateRollsBackAtomically(t *testing.T) {
	db := newServiceTestDB(t)
	setting := models.Settings{SEOTitle: "before", DefaultStorage: 1}
	if err := db.Create(&setting).Error; err != nil {
		t.Fatal(err)
	}
	title := "after"
	missingBucket := 999
	_, err := NewSettingsService(db).Update(SettingsPatch{SEOTitle: &title, DefaultStorage: &missingBucket})
	var fieldErr *SettingsFieldError
	if !errors.As(err, &fieldErr) || fieldErr.Field != "default_storage" || !errors.Is(err, ErrSettingsValidation) {
		t.Fatalf("error = %v", err)
	}
	var stored models.Settings
	if err := db.First(&stored, setting.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.SEOTitle != "before" || stored.DefaultStorage != 1 {
		t.Fatalf("settings changed after rollback: %#v", stored)
	}
}

func TestSettingsUpdateValidatesChangedPathAndNameTemplates(t *testing.T) {
	db := newServiceTestDB(t)
	// 升级前保存的旧值不合规，只要不改动就不阻止保存其他设置。
	setting := models.Settings{SEOTitle: "before", DefaultStorage: 1, DefaultPath: "/uploads/{yaer}", FileName: "{date}"}
	if err := db.Create(&setting).Error; err != nil {
		t.Fatal(err)
	}
	service := NewSettingsService(db)
	str := func(v string) *string { return &v }
	if _, err := service.Update(SettingsPatch{SEOTitle: str("after"), DefaultPath: str("/uploads/{yaer}"), FileName: str(" {date} ")}); err != nil {
		t.Fatalf("unchanged legacy templates blocked update: %v", err)
	}
	for _, tt := range []struct {
		patch SettingsPatch
		field string
	}{
		{SettingsPatch{DefaultPath: str("/uploads/{year}/ {month}")}, "default_path"},
		{SettingsPatch{DefaultPath: str("/uploads/../data")}, "default_path"},
		{SettingsPatch{DefaultPath: str("/uploads/{Year}")}, "default_path"},
		{SettingsPatch{FileName: str("{filename")}, "file_name"},
		{SettingsPatch{SEOTitle: str("rolled back"), FileName: str("{day}{hours}")}, "file_name"},
	} {
		_, err := service.Update(tt.patch)
		var fieldErr *SettingsFieldError
		if !errors.As(err, &fieldErr) || fieldErr.Field != tt.field || !errors.Is(err, ErrSettingsValidation) {
			t.Fatalf("update %+v error = %v, want field %s", tt.patch, err, tt.field)
		}
	}
	stored, err := service.Update(SettingsPatch{DefaultPath: str(" /img/{year}/{month} "), FileName: str("{filename}-{year}")})
	if err != nil {
		t.Fatal(err)
	}
	if stored.DefaultPath != "/img/{year}/{month}" || stored.FileName != "{filename}-{year}" || stored.SEOTitle != "after" {
		t.Fatalf("stored settings = %#v", stored)
	}
}

func TestStorageDeleteFailureKeepsDatabaseRecords(t *testing.T) {
	db := newServiceTestDB(t)
	bucket := models.Buckets{Id: 2, Name: "broken", Type: "unsupported", Capacity: 1024}
	if err := db.Create(&bucket).Error; err != nil {
		t.Fatal(err)
	}
	image := models.Image{FileName: "image.png", Url: "remote/image.png", Storage: bucket.Type, BucketId: bucket.Id, UserId: 1}
	if err := db.Create(&image).Error; err != nil {
		t.Fatal(err)
	}
	err := NewStorageService(db).Delete(context.Background(), bucket.Id, true)
	if !errors.Is(err, ErrStorageDelete) {
		t.Fatalf("error = %v", err)
	}
	for model, id := range map[any]int{&models.Buckets{}: bucket.Id, &models.Image{}: image.Id} {
		var count int64
		if err := db.Model(model).Where("id = ?", id).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%T record was deleted", model)
		}
	}
}

func TestDashboardStatsQueriesAreIndependentAndUserScoped(t *testing.T) {
	db := newServiceTestDB(t)
	admin := createServiceTestUser(t, db, "stats-admin", "correct-password", models.RoleAdmin)
	alice := createServiceTestUser(t, db, "stats-alice", "correct-password", models.RoleUser)
	bob := createServiceTestUser(t, db, "stats-bob", "correct-password", models.RoleUser)
	if err := db.Create(&models.Settings{}).Error; err != nil {
		t.Fatal(err)
	}

	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Date(2026, time.August, 30, 10, 0, 0, 0, location)
	images := []models.Image{
		{FileName: "before-month.jpg", FileSize: 10, MimeType: "image/jpeg", UserId: alice.ID, CreatedAt: time.Date(2026, time.July, 31, 23, 59, 59, 0, location)},
		{FileName: "month-start.png", FileSize: 20, MimeType: "image/png", UserId: alice.ID, CreatedAt: time.Date(2026, time.August, 1, 0, 0, 0, 0, location)},
		{FileName: "before-today.png", FileSize: 30, MimeType: "image/png", UserId: alice.ID, CreatedAt: time.Date(2026, time.August, 29, 23, 59, 59, 0, location)},
		{FileName: "today-start.webp", FileSize: 40, MimeType: "image/webp", UserId: alice.ID, CreatedAt: time.Date(2026, time.August, 30, 0, 0, 0, 0, location)},
		{FileName: "today-bob.jpg", FileSize: 50, MimeType: "image/jpeg", UserId: bob.ID, CreatedAt: time.Date(2026, time.August, 30, 9, 0, 0, 0, location)},
		{FileName: "month-bob.png", FileSize: 60, MimeType: "image/png", UserId: bob.ID, CreatedAt: time.Date(2026, time.August, 15, 12, 0, 0, 0, location)},
	}
	if err := db.Create(&images).Error; err != nil {
		t.Fatal(err)
	}

	service := NewStatsService(db)
	service.now = func() time.Time { return now }
	for _, test := range []struct {
		name                      string
		user                      models.User
		total, size, today, month int64
	}{
		{name: "administrator sees all images", user: admin, total: 6, size: 210, today: 2, month: 5},
		{name: "ordinary user sees own images", user: alice, total: 4, size: 100, today: 1, month: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			stats, err := service.Dashboard(test.user)
			if err != nil {
				t.Fatal(err)
			}
			if stats.TotalImages != test.total || stats.TotalSize != test.size || stats.TodayUploads != test.today || stats.MonthUploads != test.month {
				t.Fatalf("dashboard totals = (%d, %d, %d, %d), want (%d, %d, %d, %d)", stats.TotalImages, stats.TotalSize, stats.TodayUploads, stats.MonthUploads, test.total, test.size, test.today, test.month)
			}
			if int64(len(stats.RecentImages)) != test.total {
				t.Fatalf("recent image count = %d, want %d", len(stats.RecentImages), test.total)
			}
		})
	}

	trend, err := service.ImageTrend(admin, "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(trend) != 30 || trend[len(trend)-2].Count != 1 || trend[len(trend)-1].Count != 2 {
		t.Fatalf("unexpected daily trend tail: %#v", trend[len(trend)-2:])
	}
}

func TestRemoteImportRejectsNonPublicAddresses(t *testing.T) {
	for _, rawURL := range []string{
		"http://127.0.0.1/image.png",
		"http://10.0.0.1/image.png",
		"http://169.254.169.254/latest/meta-data",
		"http://[::1]/image.png",
		"http://[fe80::1]/image.png",
		"http://224.0.0.1/image.png",
		"http://198.18.0.1/image.png",
		"http://192.0.0.8/image.png",
		"http://240.0.0.1/image.png",
		"http://[64:ff9b::a9fe:a9fe]/image.png",
		"http://[2002:7f00:1::1]/image.png",
	} {
		t.Run(rawURL, func(t *testing.T) {
			if _, err := ValidateRemoteURL(rawURL, PublicAddr); !errors.Is(err, ErrImportSSRF) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestStorageDeleteRejectsCurrentDefaultStorage(t *testing.T) {
	db := newServiceTestDB(t)
	if err := db.Create(&models.Settings{DefaultStorage: 2}).Error; err != nil {
		t.Fatal(err)
	}
	bucket := models.Buckets{Id: 2, Name: "remote-default", Type: "unsupported", Capacity: 1024}
	if err := db.Create(&bucket).Error; err != nil {
		t.Fatal(err)
	}
	image := models.Image{FileName: "image.png", Url: "remote/image.png", Storage: bucket.Type, BucketId: bucket.Id, UserId: 1}
	if err := db.Create(&image).Error; err != nil {
		t.Fatal(err)
	}
	user := createServiceTestUser(t, db, "granted", "password", models.RoleUser)
	user.Permission.Buckets = []int{bucket.Id}
	if err := db.Model(&user).Update("permission", user.Permission).Error; err != nil {
		t.Fatal(err)
	}

	err := NewStorageService(db).Delete(context.Background(), bucket.Id, true)
	if !errors.Is(err, ErrStorageInUseAsDefault) {
		t.Fatalf("error = %v, want ErrStorageInUseAsDefault", err)
	}
	// 拒绝发生在物理删除之前：不可删除的 unsupported 类型不会触发 ErrStorageDelete。
	for model, id := range map[any]int{&models.Buckets{}: bucket.Id, &models.Image{}: image.Id} {
		var count int64
		if err := db.Model(model).Where("id = ?", id).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%T record was deleted", model)
		}
	}
	var setting models.Settings
	if err := db.First(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if setting.DefaultStorage != bucket.Id {
		t.Fatalf("default storage = %d, want %d", setting.DefaultStorage, bucket.Id)
	}
	var stored models.User
	if err := db.First(&stored, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(stored.Permission.Buckets) != 1 || stored.Permission.Buckets[0] != bucket.Id {
		t.Fatalf("bucket permission changed: %v", stored.Permission.Buckets)
	}
}

func TestStorageDeleteNonDefaultStorageStillSucceeds(t *testing.T) {
	db := newServiceTestDB(t)
	if err := db.Create(&models.Settings{DefaultStorage: 1}).Error; err != nil {
		t.Fatal(err)
	}
	bucket := models.Buckets{Id: 3, Name: "secondary", Type: "unsupported", Capacity: 1024}
	if err := db.Create(&bucket).Error; err != nil {
		t.Fatal(err)
	}
	user := createServiceTestUser(t, db, "granted", "password", models.RoleUser)
	user.Permission.Buckets = []int{1, bucket.Id}
	if err := db.Model(&user).Update("permission", user.Permission).Error; err != nil {
		t.Fatal(err)
	}

	if err := NewStorageService(db).Delete(context.Background(), bucket.Id, false); err != nil {
		t.Fatalf("delete non-default storage: %v", err)
	}
	var count int64
	if err := db.Model(&models.Buckets{}).Where("id = ?", bucket.Id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("non-default storage was not deleted")
	}
	var setting models.Settings
	if err := db.First(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if setting.DefaultStorage != 1 {
		t.Fatalf("default storage = %d, want 1", setting.DefaultStorage)
	}
	var stored models.User
	if err := db.First(&stored, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(stored.Permission.Buckets) != 1 || stored.Permission.Buckets[0] != 1 {
		t.Fatalf("bucket permission = %v, want [1]", stored.Permission.Buckets)
	}
}

func TestStorageDeleteWithoutPurgeKeepsObjectsAndRemovesRecords(t *testing.T) {
	db := newServiceTestDB(t)
	if err := db.Create(&models.Settings{DefaultStorage: 1}).Error; err != nil {
		t.Fatal(err)
	}
	// unsupported 类型无法执行物理删除；若未传 purge 仍尝试删除对象会返回 ErrStorageDelete。
	bucket := models.Buckets{Id: 4, Name: "keep-objects", Type: "unsupported", Capacity: 1024}
	if err := db.Create(&bucket).Error; err != nil {
		t.Fatal(err)
	}
	image := models.Image{FileName: "image.png", Url: "remote/image.png", Storage: bucket.Type, BucketId: bucket.Id, UserId: 1}
	if err := db.Create(&image).Error; err != nil {
		t.Fatal(err)
	}
	tag := models.Tags{Name: "kept"}
	if err := db.Create(&tag).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&models.ImageToTags{ImageId: image.Id, TagId: tag.Id}).Error; err != nil {
		t.Fatal(err)
	}
	user := createServiceTestUser(t, db, "granted", "password", models.RoleUser)
	user.Permission.Buckets = []int{bucket.Id}
	if err := db.Model(&user).Update("permission", user.Permission).Error; err != nil {
		t.Fatal(err)
	}

	if err := NewStorageService(db).Delete(context.Background(), bucket.Id, false); err != nil {
		t.Fatalf("delete without purge attempted physical deletion: %v", err)
	}
	for model, where := range map[any][]any{
		&models.Buckets{}:     {"id = ?", bucket.Id},
		&models.Image{}:       {"id = ?", image.Id},
		&models.ImageToTags{}: {"image_id = ?", image.Id},
	} {
		var count int64
		if err := db.Model(model).Where(where[0], where[1:]...).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%T record was kept", model)
		}
	}
	var stored models.User
	if err := db.First(&stored, user.ID).Error; err != nil {
		t.Fatal(err)
	}
	if len(stored.Permission.Buckets) != 0 {
		t.Fatalf("bucket permission = %v, want empty", stored.Permission.Buckets)
	}
}

func TestStorageSetDisabledGuardsAndBlocksUploads(t *testing.T) {
	db := newServiceTestDB(t)
	if err := db.Create(&models.Settings{DefaultStorage: 2}).Error; err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []models.Buckets{
		{Id: 1, Name: "local", Type: "default", Config: map[string]any{}},
		{Id: 2, Name: "remote-default", Type: "webdav", Config: map[string]any{}},
		{Id: 3, Name: "remote-secondary", Type: "webdav", Config: map[string]any{}},
	} {
		if err := db.Create(&bucket).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := NewStorageService(db)
	if _, err := service.SetDisabled(1, true); !errors.Is(err, ErrDefaultStorage) {
		t.Fatalf("disable local storage error = %v", err)
	}
	if _, err := service.SetDisabled(2, true); !errors.Is(err, ErrStorageInUseAsDefault) {
		t.Fatalf("disable default storage error = %v", err)
	}
	bucket, err := service.SetDisabled(3, true)
	if err != nil || !bucket.Disabled {
		t.Fatalf("disable secondary = %+v, %v", bucket, err)
	}

	admin := models.User{ID: 1, Role: models.RoleAdmin}
	_, options, err := service.UploadOptions(admin)
	if err != nil {
		t.Fatal(err)
	}
	for _, option := range options {
		if option.Id == 3 {
			t.Fatal("disabled storage is offered for upload")
		}
	}
	if _, err := NewUploadService(db).UploadBatch(nil, admin, nil, 3, nil); !errors.Is(err, ErrStorageDisabled) {
		t.Fatalf("upload to disabled storage error = %v", err)
	}
	title := "unchanged"
	if _, err := NewSettingsService(db).Update(SettingsPatch{SEOTitle: &title, DefaultStorage: &bucket.Id}); !errors.Is(err, ErrStorageDisabled) {
		t.Fatalf("set disabled default error = %v", err)
	}
	var setting models.Settings
	if err := db.First(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if setting.DefaultStorage != 2 || setting.SEOTitle == title {
		t.Fatalf("settings changed after rejected update: %+v", setting)
	}

	if bucket, err = service.SetDisabled(3, false); err != nil || bucket.Disabled {
		t.Fatalf("enable secondary = %+v, %v", bucket, err)
	}
	if _, options, err = service.UploadOptions(admin); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, option := range options {
		found = found || option.Id == 3
	}
	if !found {
		t.Fatal("re-enabled storage is not offered for upload")
	}
}

func TestImageStoragesIncludesUnavailableUploadTargets(t *testing.T) {
	db := newServiceTestDB(t)
	if err := db.Create(&models.Settings{DefaultStorage: 1}).Error; err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []models.Buckets{
		{Id: 1, Name: "local", Type: "default", Config: map[string]any{}},
		{Id: 2, Name: "disabled", Type: "webdav", Config: map[string]any{"webdav_pass": "secret"}, Disabled: true},
		{Id: 3, Name: "full", Type: "webdav", Config: map[string]any{}, Capacity: 10, Usage: 10},
		{Id: 4, Name: "unreferenced", Type: "webdav", Config: map[string]any{}},
		{Id: 5, Name: "other-user-only", Type: "webdav", Config: map[string]any{}},
	} {
		if err := db.Create(&bucket).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, image := range []models.Image{
		{FileName: "a.png", Url: "a.png", BucketId: 2, UserId: 7},
		{FileName: "b.png", Url: "b.png", BucketId: 3, UserId: 7},
		{FileName: "c.png", Url: "c.png", BucketId: 3, UserId: 7},
		{FileName: "d.png", Url: "d.png", BucketId: 5, UserId: 8},
	} {
		if err := db.Create(&image).Error; err != nil {
			t.Fatal(err)
		}
	}

	// 普通用户无任何存储权限：上传选项只剩默认存储，图库仍需拿到自己媒体所在存储。
	user := models.User{ID: 7, Role: models.RoleUser}
	_, options, err := NewStorageService(db).UploadOptions(user)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 1 || options[0].Id != 1 {
		t.Fatalf("upload options = %+v, want only default storage", options)
	}
	images := NewImageService(db)
	items, err := images.Storages(user)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Id != 2 || items[0].Name != "disabled" || !items[0].Disabled || items[1].Id != 3 || items[1].Name != "full" {
		t.Fatalf("user storages = %+v", items)
	}
	for _, item := range items {
		if item.Config != nil {
			t.Fatalf("storage %d leaks config: %+v", item.Id, item.Config)
		}
	}

	items, err = images.Storages(models.User{ID: 1, Role: models.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.Id)
	}
	if fmt.Sprint(ids) != "[2 3 5]" {
		t.Fatalf("admin storage ids = %v, want [2 3 5]", ids)
	}
}
