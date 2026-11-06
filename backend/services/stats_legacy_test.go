package services

import (
	"time"

	"oneimg/backend/models"

	"gorm.io/gorm"
)

// legacyDashboard and legacyImageTrend are the replaced per-statistic queries, kept as the parity reference.
func legacyDashboard(s *StatsService, user models.User) (DashboardStats, error) {
	var result DashboardStats
	var err error
	// GORM chain values retain clauses after use, so each aggregate starts from a fresh scope.
	if err := s.imageScope(user).Where("NOT (" + models.VideoSQL + ")").Count(&result.TotalImages).Error; err != nil {
		return result, err
	}
	if err := s.imageScope(user).Select("COALESCE(SUM(file_size), 0)").Scan(&result.TotalSize).Error; err != nil {
		return result, err
	}
	if err := s.imageScope(user).Where(models.VideoSQL).Count(&result.TotalVideos).Error; err != nil {
		return result, err
	}
	if err := s.imageScope(user).Where(models.VideoSQL).Select("COALESCE(SUM(file_size), 0)").Scan(&result.VideoSize).Error; err != nil {
		return result, err
	}
	result.ImageSize = result.TotalSize - result.VideoSize
	now := s.now()
	today := localDay(now)
	result.TodayUploads, err = legacyCountBetween(s.imageScope(user), today, today.AddDate(0, 0, 1))
	if err != nil {
		return result, err
	}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	result.MonthUploads, err = legacyCountBetween(s.imageScope(user), month, month.AddDate(0, 1, 0))
	if err != nil {
		return result, err
	}
	result.TodayVideoUploads, err = legacyCountBetween(s.imageScope(user).Where(models.VideoSQL), today, today.AddDate(0, 0, 1))
	if err != nil {
		return result, err
	}
	result.TodayImageUploads = result.TodayUploads - result.TodayVideoUploads
	result.MonthVideoUploads, err = legacyCountBetween(s.imageScope(user).Where(models.VideoSQL), month, month.AddDate(0, 1, 0))
	if err != nil {
		return result, err
	}
	result.MonthImageUploads = result.MonthUploads - result.MonthVideoUploads
	var recent []models.Image
	if err := s.imageScope(user).Order("created_at DESC").Limit(10).Find(&recent).Error; err != nil {
		return result, err
	}
	result.RecentImages, err = s.Images().attachMetadata(recent)
	if err != nil {
		return result, err
	}
	result.UploadTrend, err = legacyTrend(s.imageScope(user), now, "day", 7)
	if err != nil {
		return result, err
	}
	if err := s.imageScope(user).Select("mime_type format, COUNT(*) count, COALESCE(SUM(file_size), 0) size").Group("mime_type").Scan(&result.FormatStats).Error; err != nil {
		return result, err
	}
	ranges := []struct {
		name     string
		min, max int64
	}{
		{"< 100KB", 0, 100 * 1024}, {"100KB - 500KB", 100 * 1024, 500 * 1024},
		{"500KB - 1MB", 500 * 1024, 1024 * 1024}, {"1MB - 5MB", 1024 * 1024, 5 * 1024 * 1024},
		{"5MB - 10MB", 5 * 1024 * 1024, 10 * 1024 * 1024}, {">= 10MB", 10 * 1024 * 1024, 0},
	}
	for _, item := range ranges {
		var count int64
		query := s.imageScope(user).Where("file_size >= ?", item.min)
		if item.max > 0 {
			query = query.Where("file_size < ?", item.max)
		}
		if err := query.Count(&count).Error; err != nil {
			return result, err
		}
		result.SizeDistribution = append(result.SizeDistribution, SizeStat{Range: item.name, Count: count})
	}
	return result, nil
}

func legacyImageTrend(s *StatsService, user models.User, period string) ([]TrendPoint, error) {
	counts := map[string]int{"day": 30, "week": 12, "month": 12, "year": 5}
	return legacyTrend(s.imageScope(user), s.now(), period, counts[period])
}

func legacyTrend(scope *gorm.DB, now time.Time, period string, count int) ([]TrendPoint, error) {
	result := make([]TrendPoint, 0, count)
	for i := count - 1; i >= 0; i-- {
		var start, end time.Time
		var label string
		switch period {
		case "week":
			day := localDay(now)
			monday := day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
			start = monday.AddDate(0, 0, -i*7)
			end = start.AddDate(0, 0, 7)
			label = start.Format("2006-01-02")
		case "month":
			start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -i, 0)
			end = start.AddDate(0, 1, 0)
			label = start.Format("2006-01")
		case "year":
			value := now.AddDate(-i, 0, 0)
			start = time.Date(value.Year(), time.January, 1, 0, 0, 0, 0, value.Location())
			end = start.AddDate(1, 0, 0)
			label = start.Format("2006")
		default:
			start = localDay(now.AddDate(0, 0, -i))
			end = start.AddDate(0, 0, 1)
			label = start.Format("2006-01-02")
		}
		total, err := legacyCountBetween(scope, start, end)
		if err != nil {
			return nil, err
		}
		videos, err := legacyCountBetween(scope.Session(&gorm.Session{}).Where(models.VideoSQL), start, end)
		if err != nil {
			return nil, err
		}
		result = append(result, TrendPoint{Date: label, Count: total, VideoCount: videos, ImageCount: total - videos})
	}
	return result, nil
}

func legacyCountBetween(scope *gorm.DB, start, end time.Time) (int64, error) {
	var count int64
	query := scope.Session(&gorm.Session{})
	if scope.Dialector.Name() == "sqlite" {
		query = query.Where("julianday(created_at) >= julianday(?) AND julianday(created_at) < julianday(?)", start.UTC().Format(time.RFC3339Nano), end.UTC().Format(time.RFC3339Nano))
	} else {
		query = query.Where("created_at >= ? AND created_at < ?", start.UTC(), end.UTC())
	}
	err := query.Count(&count).Error
	return count, err
}
