package services

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"oneimg/backend/database"
	"oneimg/backend/models"
)

func TestPasskeyRenameKeepsASCIIComparison(t *testing.T) {
	db := newServiceTestDB(t)
	var keys []models.PasskeyCredential
	for index, name := range []string{"É", "Key", "other"} {
		key := models.PasskeyCredential{UserID: 1, Name: name, CredentialID: fmt.Sprint(index), CredentialData: "encrypted"}
		if err := db.Create(&key).Error; err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	service := NewPasskeyService(db, nil)
	if _, err := service.Rename(1, int(keys[2].ID), "é"); err != nil {
		t.Fatal("rename expanded the existing identity comparison to Unicode", err)
	}
	for _, name := range []string{"É", "KEY"} {
		if _, err := service.Rename(1, int(keys[2].ID), name); !errors.Is(err, ErrPasskeyNameConflict) {
			t.Fatal("existing Passkey collision was not mapped", err)
		}
	}
}

func TestUnicodeSearchAndStablePagination(t *testing.T) {
	db := newServiceTestDB(t)
	if err := db.Create(&models.Settings{}).Error; err != nil {
		t.Fatal(err)
	}
	user := createServiceTestUser(t, db, "Straße-Σ-É", "password", models.RoleAdmin)
	names := []string{"Straße", "ΟΣ", "é", "e\u0301", "İ", "ı", "I", "fullＡ", "a\\b", "x%y", "x_y"}
	sameTime := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	for _, name := range names {
		if err := db.Create(&models.Image{FileName: name, OriginalFileName: name, FileSize: 10, CreatedAt: sameTime, UserId: user.ID}).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := NewImageService(db)
	for _, item := range []struct {
		input string
		want  []string
	}{
		{"STRASSE", []string{"Straße"}},
		{"ος", []string{"ΟΣ"}},
		{"ς", []string{"ΟΣ"}},
		{"É", []string{"é", "e\u0301"}},
		{"e", []string{"Straße"}},
		{"i", []string{"İ", "I"}},
		{"ı", []string{"ı"}},
		{"fulla", nil},
		{"a\\b", []string{"a\\b"}},
		{"x_y", []string{"x%y", "x_y"}},
		{"x%y", []string{"x%y", "x_y"}},
	} {
		t.Run(item.input, func(t *testing.T) {
			items, total, err := service.List(ImageListQuery{User: user, Page: 1, PageSize: 100, Sort: "id", Order: "asc", Search: " " + item.input + " "})
			if err != nil || total != int64(len(item.want)) {
				t.Fatal("search mismatch", total, err)
			}
			var actual []string
			for _, record := range items {
				actual = append(actual, record.Image.FileName)
			}
			if !reflect.DeepEqual(actual, item.want) {
				t.Fatal("Unicode search changed", actual, item.want)
			}
		})
	}
	sortedNames := append([]string(nil), names...)
	sort.Strings(sortedNames)
	for _, sortField := range []string{"created_at", "file_size", "filename", "id"} {
		for _, order := range []string{"asc", "desc"} {
			var paged []int
			for page := 1; page <= len(names); page++ {
				items, total, err := service.List(ImageListQuery{User: user, Page: page, PageSize: 1, Sort: sortField, Order: order})
				if err != nil || total != int64(len(names)) || len(items) != 1 {
					t.Fatal("pagination failed", err)
				}
				paged = append(paged, items[0].Image.Id)
			}
			all, _, err := service.List(ImageListQuery{User: user, Page: 1, PageSize: 100, Sort: sortField, Order: order})
			if err != nil {
				t.Fatal(err)
			}
			for index, item := range all {
				if item.Image.Id != paged[index] {
					t.Fatal("unstable page order", sortField, order, paged)
				}
				if sortField == "filename" {
					nameIndex := index
					if order == "desc" {
						nameIndex = len(sortedNames) - 1 - index
					}
					if item.Image.FileName != sortedNames[nameIndex] {
						t.Fatal("filename sorting differed from binary order")
					}
				} else if index > 0 && ((order == "asc" && paged[index] <= paged[index-1]) || (order == "desc" && paged[index] >= paged[index-1])) {
					t.Fatal("equal primary values did not use same-direction ID ordering")
				}
			}
		}
	}
	if _, _, err := service.List(ImageListQuery{Page: 1, PageSize: 10, Sort: "id;SELECT 1"}); !errors.Is(err, ErrImageListQuery) {
		t.Fatal("sort whitelist bypassed")
	}
	users, _, err := NewUserService(db).List(UserListQuery{Page: 1, PageSize: 10, Sort: "id", Order: "asc", Username: "STRASSE-ς-é"})
	if err != nil || len(users) != 1 {
		t.Fatal("user Unicode search mismatch", err)
	}
	name := "Renamed-Σ"
	if _, err := NewAccountService(db).Update(user.ID, "password", &name, nil, "", user.AuthVersion); err != nil {
		t.Fatal(err)
	}
	users, _, err = NewUserService(db).List(UserListQuery{Page: 1, PageSize: 10, Sort: "id", Order: "asc", Username: "RENAMED-ς"})
	if err != nil || len(users) != 1 {
		t.Fatal("renamed search key stale", err)
	}
	other := createServiceTestUser(t, db, "other-user", "password", models.RoleUser)
	name = "Admin-É-Σ"
	if _, err := NewUserService(db).Update(user.ID, other.ID, &name, nil); err != nil {
		t.Fatal(err)
	}
	users, _, err = NewUserService(db).List(UserListQuery{Page: 1, PageSize: 10, Sort: "id", Order: "asc", Username: "ADMIN-E\u0301-ς"})
	if err != nil || len(users) != 1 || users[0].User.ID != other.ID {
		t.Fatal("administrator rename search key stale", err)
	}
}

func TestUsernameSortIsBinaryOnBothBackends(t *testing.T) {
	db := newServiceTestDB(t)
	for _, name := range []string{"admin", "Zed", "École", "Straße", "张三"} {
		createServiceTestUser(t, db, name, "password", models.RoleUser)
	}
	service := NewUserService(db)
	for order, want := range map[string][]string{
		"ASC":  {"Straße", "Zed", "admin", "École", "张三"},
		"DESC": {"张三", "École", "admin", "Zed", "Straße"},
	} {
		items, _, err := service.List(UserListQuery{Page: 1, PageSize: 10, Sort: "username", Order: order})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, item := range items {
			got = append(got, item.User.Username)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s username order = %v, want %v", order, got, want)
		}
	}
}

func TestImageExistsFiltersAndTimeOrdering(t *testing.T) {
	db := newServiceTestDB(t)
	if err := db.Create(&models.Settings{}).Error; err != nil {
		t.Fatal(err)
	}
	admin := createServiceTestUser(t, db, "admin", "password", models.RoleAdmin)
	member := createServiceTestUser(t, db, "member", "password", models.RoleUser)
	tags := []models.Tags{{Name: "first"}, {Name: "second"}}
	if err := db.Create(&tags).Error; err != nil {
		t.Fatal(err)
	}
	records := []models.Image{
		{FileName: "first", UserId: member.ID, CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{FileName: "second", UserId: member.ID, CreatedAt: time.Date(2026, 1, 1, 7, 0, 0, 0, time.FixedZone("Shanghai", 8*3600))},
		{FileName: "null time", UserId: admin.ID},
		{FileName: "historical orphan", UserId: 9999},
	}
	if err := db.Create(&records).Error; err != nil {
		t.Fatal(err)
	}
	for _, link := range []models.ImageToTags{{ImageId: records[0].Id, TagId: tags[0].Id}, {ImageId: records[0].Id, TagId: tags[1].Id}, {ImageId: records[1].Id, TagId: tags[0].Id}} {
		if err := db.Create(&link).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := NewImageService(db)
	role := models.RoleUser
	for _, query := range []ImageListQuery{
		{TagIDs: []int{tags[0].Id, tags[1].Id}},
		{UploaderRole: &role},
		{Untagged: true},
		{TagIDs: []int{tags[0].Id, tags[1].Id}, UploaderRole: &role},
	} {
		query.User, query.Page, query.PageSize, query.Sort = admin, 1, 100, "id"
		items, total, err := service.List(query)
		if err != nil || total != 2 || len(items) != 2 || items[0].Image.Id == items[1].Image.Id {
			t.Fatal("EXISTS filter duplicated or lost records", total, err)
		}
	}
	if err := db.Model(&records[2]).UpdateColumn("created_at", nil).Error; err != nil {
		t.Fatal(err)
	}
	for _, order := range []string{"asc", "desc"} {
		items, total, err := service.List(ImageListQuery{User: admin, UploaderRole: &role, Page: 1, PageSize: 100, Sort: "created_at", Order: order})
		if err != nil || total != 2 {
			t.Fatal(err)
		}
		first := records[1].Id
		if order == "desc" {
			first = records[0].Id
		}
		if items[0].Image.Id != first {
			t.Fatal("time ordering compared wall-clock strings")
		}
		items, _, err = service.List(ImageListQuery{User: admin, Page: 1, PageSize: 100, Sort: "created_at", Order: order})
		index := 0
		if order == "desc" {
			index = len(items) - 1
		}
		if err != nil || items[index].Image.Id != records[2].Id {
			t.Fatal("null time ordering differed", err)
		}
	}
}

func TestStatsMonthEndDSTAndErrors(t *testing.T) {
	db := newServiceTestDB(t)
	if err := db.Create(&models.Settings{}).Error; err != nil {
		t.Fatal(err)
	}
	user := models.User{ID: 1, Role: models.RoleAdmin}
	service := NewStatsService(db)
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, now := range []time.Time{time.Date(2024, 3, 31, 12, 0, 0, 0, location), time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC), time.Date(2024, 11, 3, 12, 0, 0, 0, location)} {
		service.now = func() time.Time { return now }
		items, err := service.ImageTrend(user, "month")
		if err != nil || len(items) != 12 {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, point := range items {
			if seen[point.Date] {
				t.Fatal("duplicate month", point.Date)
			}
			seen[point.Date] = true
		}
		if items[11].Date != now.Format("2006-01") {
			t.Fatal("month end skipped")
		}
	}
	// 23-hour and 25-hour days use civil boundaries rather than 24-hour durations.
	for _, date := range []time.Time{time.Date(2024, 3, 10, 0, 0, 0, 0, location), time.Date(2024, 11, 3, 0, 0, 0, 0, location)} {
		end := date.AddDate(0, 0, 1)
		image := models.Image{FileName: fmt.Sprint(date), CreatedAt: end.Add(-time.Second)}
		if err := db.Create(&image).Error; err != nil {
			t.Fatal(err)
		}
		condition, args := windowCondition(db, timeWindow{start: date, end: end})
		var count int64
		err := db.Model(&models.Image{}).Where(condition, args...).Count(&count).Error
		if err != nil || count != 1 {
			t.Fatal("DST boundary count", count, err)
		}
	}
	if err := db.Migrator().DropTable(&models.Image{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ImageTrend(user, "day"); err == nil {
		t.Fatal("trend failure hidden")
	}
	if _, err := service.Dashboard(user); err == nil {
		t.Fatal("dashboard failure hidden")
	}
}

func TestConcurrentQuotaAndCapacityValidation(t *testing.T) {
	db := newServiceTestDB(t)
	if _, err := validateStorageInput(StorageInput{CapacityBytes: math.MaxInt64 + 1}, true); !errors.Is(err, ErrStorageCapacity) {
		t.Fatal("capacity overflow accepted")
	}
	bucket := models.Buckets{Name: "concurrent", Type: "default", Capacity: 10}
	if err := db.Create(&bucket).Error; err != nil {
		t.Fatal(err)
	}
	var success atomic.Int32
	var wait sync.WaitGroup
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			allowed, err := database.ReserveUsage(db, bucket.Id, 1)
			if err != nil {
				t.Error(err)
			}
			if allowed {
				success.Add(1)
			}
		}()
	}
	wait.Wait()
	if success.Load() != 10 {
		t.Fatal("concurrent quota exceeded or lost", success.Load())
	}
	if err := db.First(&bucket, bucket.Id).Error; err != nil || bucket.Usage != 10 {
		t.Fatal("incorrect counter", bucket.Usage, err)
	}
}
