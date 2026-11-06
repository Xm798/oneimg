package services

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"oneimg/backend/models"
	"oneimg/internal/testdb"

	"gorm.io/gorm"
)

type statsFixture struct {
	db                 *gorm.DB
	service            *StatsService
	admin, alice, bob  models.User
	nullCreatedAtCount int
}

// newStatsFixture seeds rows on every boundary the windows use: local midnights
// across a DST change, Monday, month and year starts (each ±1 ns), size range
// edges, and MIME spellings that differ only in case or whitespace.
func newStatsFixture(t *testing.T) statsFixture {
	t.Helper()
	db := newServiceTestDB(t)
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 9, 10, 0, 0, 0, location)
	f := statsFixture{
		db:    db,
		admin: createServiceTestUser(t, db, "stats-admin", "correct-password", models.RoleAdmin),
		alice: createServiceTestUser(t, db, "stats-alice", "correct-password", models.RoleUser),
		bob:   createServiceTestUser(t, db, "stats-bob", "correct-password", models.RoleUser),
	}
	if err := db.Create(&models.Settings{}).Error; err != nil {
		t.Fatal(err)
	}
	f.service = NewStatsService(db)
	f.service.now = func() time.Time { return now }

	var instants []time.Time
	edge := func(value time.Time) {
		instants = append(instants, value.Add(-time.Nanosecond), value, value.Add(time.Nanosecond))
	}
	for day := -100; day <= 1; day++ {
		midnight := localDay(now.AddDate(0, 0, day))
		edge(midnight)
		instants = append(instants, midnight.Add(12*time.Hour))
	}
	for month := 0; month < 14; month++ {
		edge(time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, location).AddDate(0, -month, 0))
	}
	for year := 0; year < 7; year++ {
		edge(time.Date(now.Year()-year, time.January, 1, 0, 0, 0, 0, location))
	}
	mimes := []string{"image/png", "", "IMAGE/WEBP", " video/mp4 ", "VIDEO/WEBM", "video/quicktime", "image/gif", "video/mp4"}
	sizes := []int64{-5, 0, 100*1024 - 1, 100 * 1024, 500 * 1024, 1024*1024 - 1, 5 * 1024 * 1024, 10*1024*1024 - 1, 10 * 1024 * 1024, 64 * 1024 * 1024}
	users := []models.User{f.admin, f.alice, f.bob}
	rows := make([]models.Image, 0, len(instants)+3)
	for i, instant := range instants {
		rows = append(rows, models.Image{FileName: "seed", MimeType: mimes[i%len(mimes)], FileSize: sizes[i%len(sizes)], UserId: users[i%len(users)].ID, CreatedAt: instant})
	}
	for i := range 3 {
		rows = append(rows, models.Image{FileName: "undated", MimeType: mimes[i+3], FileSize: sizes[i+5], UserId: users[i].ID, CreatedAt: now})
	}
	if err := db.CreateInBatches(rows, 200).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&models.Image{}).Where("file_name = ?", "undated").Update("created_at", nil).Error; err != nil {
		t.Fatal(err)
	}
	f.nullCreatedAtCount = 3
	if err := db.Model(&models.Image{}).Where("id = ?", rows[1].Id).Update("mime_type", nil).Error; err != nil {
		t.Fatal(err)
	}
	return f
}

func normalizeDashboard(stats DashboardStats) DashboardStats {
	sort.Slice(stats.FormatStats, func(i, j int) bool { return stats.FormatStats[i].Format < stats.FormatStats[j].Format })
	for i := range stats.RecentImages {
		stats.RecentImages[i] = ImageRecord{Image: models.Image{Id: stats.RecentImages[i].Image.Id}}
	}
	return stats
}

func TestDashboardMatchesLegacy(t *testing.T) {
	f := newStatsFixture(t)
	for _, user := range []models.User{f.admin, f.alice, f.bob} {
		want, err := legacyDashboard(f.service, user)
		if err != nil {
			t.Fatal(err)
		}
		got, err := f.service.Dashboard(user)
		if err != nil {
			t.Fatal(err)
		}
		if want, got := normalizeDashboard(want), normalizeDashboard(got); !reflect.DeepEqual(want, got) {
			t.Fatalf("%s dashboard differs\nwant %+v\ngot  %+v", user.Username, want, got)
		}
		if got.TodayUploads == 0 || got.MonthUploads == 0 || got.TotalVideos == 0 {
			t.Fatalf("%s fixture does not exercise windows: %+v", user.Username, got)
		}
	}
}

func TestImageTrendMatchesLegacy(t *testing.T) {
	f := newStatsFixture(t)
	for _, user := range []models.User{f.admin, f.alice} {
		for _, period := range []string{"day", "week", "month", "year"} {
			want, err := legacyImageTrend(f.service, user, period)
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.service.ImageTrend(user, period)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("%s %s trend differs\nwant %+v\ngot  %+v", user.Username, period, want, got)
			}
		}
	}
}

func TestDashboardNonAdminIsolation(t *testing.T) {
	f := newStatsFixture(t)
	var total int64
	if err := f.db.Model(&models.Image{}).Count(&total).Error; err != nil {
		t.Fatal(err)
	}
	sum := func(stats DashboardStats) (count, formats, sizes int64) {
		for _, item := range stats.FormatStats {
			formats += item.Count
		}
		for _, item := range stats.SizeDistribution {
			sizes += item.Count
		}
		return stats.TotalImages + stats.TotalVideos, formats, sizes
	}
	var users int64
	for _, user := range []models.User{f.admin, f.alice, f.bob} {
		stats, err := f.service.Dashboard(user)
		if err != nil {
			t.Fatal(err)
		}
		var own, sized int64
		scope := f.db.Model(&models.Image{})
		if user.Role != models.RoleAdmin {
			scope = scope.Where("user_id = ?", user.ID)
		}
		if err := scope.Session(&gorm.Session{}).Count(&own).Error; err != nil {
			t.Fatal(err)
		}
		// The size ranges start at 0 bytes, so negative legacy sizes fall outside them.
		if err := scope.Where("file_size >= 0").Count(&sized).Error; err != nil {
			t.Fatal(err)
		}
		count, formats, sizes := sum(stats)
		if count != own || formats != own || sizes != sized {
			t.Fatalf("%s sees %d/%d/%d rows, owns %d/%d", user.Username, count, formats, sizes, own, sized)
		}
		for _, image := range stats.RecentImages {
			if user.Role != models.RoleAdmin && image.Image.UserId != user.ID {
				t.Fatalf("%s sees image of user %d", user.Username, image.Image.UserId)
			}
		}
		if user.Role != models.RoleAdmin {
			users += own
		}
	}
	if users >= total {
		t.Fatalf("fixture lacks admin-owned rows: %d of %d", users, total)
	}
}

func TestDashboardNullCreatedAt(t *testing.T) {
	f := newStatsFixture(t)
	before, err := f.service.Dashboard(f.admin)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Where("file_name = ?", "undated").Delete(&models.Image{}).Error; err != nil {
		t.Fatal(err)
	}
	after, err := f.service.Dashboard(f.admin)
	if err != nil {
		t.Fatal(err)
	}
	if delta := before.TotalImages + before.TotalVideos - after.TotalImages - after.TotalVideos; delta != int64(f.nullCreatedAtCount) {
		t.Fatalf("undated rows counted %d times in totals", delta)
	}
	if before.TodayUploads != after.TodayUploads || before.MonthUploads != after.MonthUploads || !reflect.DeepEqual(before.UploadTrend, after.UploadTrend) {
		t.Fatal("undated rows counted in a time window")
	}
}

func countQueries(t *testing.T, db *gorm.DB, run func()) int {
	t.Helper()
	count := 0
	name := "test:count_queries_" + t.Name()
	increment := func(*gorm.DB) { count++ }
	if err := db.Callback().Query().After("gorm:query").Register(name, increment); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Row().After("gorm:row").Register(name, increment); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = db.Callback().Query().Remove(name)
		_ = db.Callback().Row().Remove(name)
	}()
	run()
	return count
}

func TestDashboardQueryCount(t *testing.T) {
	f := newStatsFixture(t)
	var recent []models.Image
	if err := f.db.Order("created_at DESC").Limit(10).Find(&recent).Error; err != nil {
		t.Fatal(err)
	}
	metadata := countQueries(t, f.db, func() {
		if _, err := f.service.Images().attachMetadata(recent); err != nil {
			t.Fatal(err)
		}
	})
	for _, user := range []models.User{f.admin, f.alice} {
		dashboard := countQueries(t, f.db, func() {
			if _, err := f.service.Dashboard(user); err != nil {
				t.Fatal(err)
			}
		})
		if dashboard-metadata > 3 {
			t.Fatalf("%s dashboard ran %d queries besides metadata", user.Username, dashboard-metadata)
		}
		for _, period := range []string{"day", "week", "month", "year"} {
			trend := countQueries(t, f.db, func() {
				if _, err := f.service.ImageTrend(user, period); err != nil {
					t.Fatal(err)
				}
			})
			if trend != 1 {
				t.Fatalf("%s %s trend ran %d queries", user.Username, period, trend)
			}
		}
	}
}

// BenchmarkDashboard compares the aggregate with the per-statistic queries at
// 50k rows; set ONEIMG_TEST_POSTGRES_URL to measure PostgreSQL.
func BenchmarkDashboard(b *testing.B) {
	db := testdb.New(b).DB
	admin := models.User{ID: 1, Username: "bench-admin", Role: models.RoleAdmin, Permission: models.Permission{Codes: []string{}, Buckets: []int{}}}
	user := models.User{ID: 2, Username: "bench-user", Role: models.RoleUser, Permission: models.Permission{Codes: []string{}, Buckets: []int{}}}
	if err := db.Create(&[]models.User{admin, user}).Error; err != nil {
		b.Fatal(err)
	}
	if err := db.Create(&models.Settings{}).Error; err != nil {
		b.Fatal(err)
	}
	now := time.Now()
	mimes := []string{"image/png", "image/webp", "video/mp4", "image/jpeg"}
	rows := make([]models.Image, 0, 50000)
	for i := range 50000 {
		rows = append(rows, models.Image{FileName: "bench", Url: fmt.Sprintf("/uploads/%d.png", i), MimeType: mimes[i%len(mimes)], FileSize: int64(i * 997 % (12 << 20)), UserId: 1 + i%2, CreatedAt: now.Add(-time.Duration(i) * 13 * time.Minute)})
	}
	if err := db.CreateInBatches(rows, 1000).Error; err != nil {
		b.Fatal(err)
	}
	service := NewStatsService(db)
	for _, account := range []models.User{admin, user} {
		b.Run("legacy/"+account.Username, func(b *testing.B) {
			for b.Loop() {
				if _, err := legacyDashboard(service, account); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("aggregate/"+account.Username, func(b *testing.B) {
			for b.Loop() {
				if _, err := service.Dashboard(account); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
