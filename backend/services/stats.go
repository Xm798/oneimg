package services

import (
	"fmt"
	"strings"
	"time"

	"oneimg/backend/models"

	"gorm.io/gorm"
)

type StatsService struct {
	db  *gorm.DB
	now func() time.Time
}

type TrendPoint struct {
	Date       string
	Count      int64
	ImageCount int64
	VideoCount int64
}

type FormatStat struct {
	Format string
	Count  int64
	Size   int64
}

type SizeStat struct {
	Range string
	Count int64
}

type DashboardStats struct {
	TotalImages       int64
	TotalVideos       int64
	ImageSize         int64
	VideoSize         int64
	TodayImageUploads int64
	TodayVideoUploads int64
	MonthImageUploads int64
	MonthVideoUploads int64
	TotalSize         int64
	TodayUploads      int64
	MonthUploads      int64
	RecentImages      []ImageRecord
	UploadTrend       []TrendPoint
	FormatStats       []FormatStat
	SizeDistribution  []SizeStat
}

func NewStatsService(db *gorm.DB) *StatsService { return &StatsService{db: db, now: time.Now} }

func (s *StatsService) imageScope(user models.User) *gorm.DB {
	query := s.db.Model(&models.Image{})
	if user.Role != models.RoleAdmin {
		query = query.Where("user_id = ?", user.ID)
	}
	return query
}

var sizeRanges = []struct {
	name     string
	min, max int64
}{
	{"< 100KB", 0, 100 * 1024}, {"100KB - 500KB", 100 * 1024, 500 * 1024},
	{"500KB - 1MB", 500 * 1024, 1024 * 1024}, {"1MB - 5MB", 1024 * 1024, 5 * 1024 * 1024},
	{"5MB - 10MB", 5 * 1024 * 1024, 10 * 1024 * 1024}, {">= 10MB", 10 * 1024 * 1024, 0},
}

func (s *StatsService) Dashboard(user models.User) (DashboardStats, error) {
	var result DashboardStats
	now := s.now()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	trendWindows := windows(now, "day", 7)

	// Every column is self-gated by its CASE, so the scope's user filter is the only WHERE.
	sums := &aggregate{db: s.db}
	totalImages := sums.add("CASE WHEN " + models.VideoSQL + " THEN 0 ELSE 1 END")
	totalVideos := sums.add("CASE WHEN " + models.VideoSQL + " THEN 1 ELSE 0 END")
	totalSize := sums.add("file_size")
	videoSize := sums.add("CASE WHEN " + models.VideoSQL + " THEN file_size ELSE 0 END")
	monthColumns := sums.window(timeWindow{start: month, end: month.AddDate(0, 1, 0)})
	trendColumns := sums.windows(trendWindows)
	todayColumns := trendColumns[len(trendColumns)-1]
	sizeColumns := make([]int, len(sizeRanges))
	for i, item := range sizeRanges {
		condition, args := "file_size >= ?", []any{item.min}
		if item.max > 0 {
			condition, args = condition+" AND file_size < ?", append(args, item.max)
		}
		sizeColumns[i] = sums.add("CASE WHEN "+condition+" THEN 1 ELSE 0 END", args...)
	}
	values, err := sums.scan(s.imageScope(user))
	if err != nil {
		return result, err
	}
	result.TotalImages = values[totalImages]
	result.TotalVideos = values[totalVideos]
	result.TotalSize = values[totalSize]
	result.VideoSize = values[videoSize]
	result.ImageSize = result.TotalSize - result.VideoSize
	result.TodayUploads = values[todayColumns.all]
	result.TodayVideoUploads = values[todayColumns.video]
	result.TodayImageUploads = result.TodayUploads - result.TodayVideoUploads
	result.MonthUploads = values[monthColumns.all]
	result.MonthVideoUploads = values[monthColumns.video]
	result.MonthImageUploads = result.MonthUploads - result.MonthVideoUploads
	result.UploadTrend = trendPoints(trendWindows, trendColumns, values)
	for i, item := range sizeRanges {
		result.SizeDistribution = append(result.SizeDistribution, SizeStat{Range: item.name, Count: values[sizeColumns[i]]})
	}

	var recent []models.Image
	if err := s.imageScope(user).Order("created_at DESC").Limit(10).Find(&recent).Error; err != nil {
		return result, err
	}
	result.RecentImages, err = s.Images().attachMetadata(recent)
	if err != nil {
		return result, err
	}
	if err := s.imageScope(user).Select("mime_type format, COUNT(*) count, COALESCE(SUM(file_size), 0) size").Group("mime_type").Scan(&result.FormatStats).Error; err != nil {
		return result, err
	}
	return result, nil
}

// Images returns an image service sharing the same database transaction scope.
func (s *StatsService) Images() *ImageService { return NewImageService(s.db) }

func (s *StatsService) ImageTrend(user models.User, period string) ([]TrendPoint, error) {
	counts := map[string]int{"day": 30, "week": 12, "month": 12, "year": 5}
	items := windows(s.now(), period, counts[period])
	if len(items) == 0 {
		return []TrendPoint{}, nil
	}
	sums := &aggregate{db: s.db}
	columns := sums.windows(items)
	// The overall range only prunes rows; each column still applies its own window.
	overall := timeWindow{start: items[0].start, end: items[len(items)-1].end}
	condition, args := windowCondition(s.db, overall)
	values, err := sums.scan(s.imageScope(user).Where(condition, args...))
	if err != nil {
		return nil, err
	}
	return trendPoints(items, columns, values), nil
}

type timeWindow struct {
	label      string
	start, end time.Time
}

// windows lists count consecutive periods ending with the one containing now,
// computed in now's location so day and month edges follow local DST rules.
func windows(now time.Time, period string, count int) []timeWindow {
	result := make([]timeWindow, 0, count)
	for i := count - 1; i >= 0; i-- {
		var item timeWindow
		switch period {
		case "week":
			day := localDay(now)
			monday := day.AddDate(0, 0, -(int(day.Weekday())+6)%7)
			item.start = monday.AddDate(0, 0, -i*7)
			item.end = item.start.AddDate(0, 0, 7)
			item.label = item.start.Format("2006-01-02")
		case "month":
			item.start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -i, 0)
			item.end = item.start.AddDate(0, 1, 0)
			item.label = item.start.Format("2006-01")
		case "year":
			value := now.AddDate(-i, 0, 0)
			item.start = time.Date(value.Year(), time.January, 1, 0, 0, 0, 0, value.Location())
			item.end = item.start.AddDate(1, 0, 0)
			item.label = item.start.Format("2006")
		default:
			item.start = localDay(now.AddDate(0, 0, -i))
			item.end = item.start.AddDate(0, 0, 1)
			item.label = item.start.Format("2006-01-02")
		}
		result = append(result, item)
	}
	return result
}

func trendPoints(items []timeWindow, columns []windowColumns, values []int64) []TrendPoint {
	result := make([]TrendPoint, 0, len(items))
	for i, item := range items {
		total, videos := values[columns[i].all], values[columns[i].video]
		result = append(result, TrendPoint{Date: item.label, Count: total, VideoCount: videos, ImageCount: total - videos})
	}
	return result
}

func localDay(value time.Time) time.Time {
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, value.Location())
}

// windowCondition tests created_at against [start, end). SQLite compares
// julianday values because stored timestamps may use mixed text formats; a
// NULL or unparseable created_at makes the test NULL, excluding the row.
func windowCondition(db *gorm.DB, item timeWindow) (string, []any) {
	if db.Dialector.Name() == "sqlite" {
		return "julianday(created_at) >= julianday(?) AND julianday(created_at) < julianday(?)", []any{item.start.UTC().Format(time.RFC3339Nano), item.end.UTC().Format(time.RFC3339Nano)}
	}
	return "created_at >= ? AND created_at < ?", []any{item.start.UTC(), item.end.UTC()}
}

// aggregate collects SUM columns for a single SELECT. CASE rather than FILTER
// and the BIGINT cast keep SQLite and PostgreSQL results scannable as int64.
type aggregate struct {
	db      *gorm.DB
	columns []string
	args    []any
}

func (a *aggregate) add(expression string, args ...any) int {
	a.columns = append(a.columns, fmt.Sprintf("CAST(COALESCE(SUM(%s), 0) AS BIGINT)", expression))
	a.args = append(a.args, args...)
	return len(a.columns) - 1
}

type windowColumns struct{ all, video int }

func (a *aggregate) window(item timeWindow) windowColumns {
	condition, args := windowCondition(a.db, item)
	return windowColumns{
		all:   a.add("CASE WHEN "+condition+" THEN 1 ELSE 0 END", args...),
		video: a.add("CASE WHEN "+condition+" AND "+models.VideoSQL+" THEN 1 ELSE 0 END", args...),
	}
}

func (a *aggregate) windows(items []timeWindow) []windowColumns {
	result := make([]windowColumns, len(items))
	for i, item := range items {
		result[i] = a.window(item)
	}
	return result
}

func (a *aggregate) scan(scope *gorm.DB) ([]int64, error) {
	values := make([]int64, len(a.columns))
	targets := make([]any, len(values))
	for i := range values {
		targets[i] = &values[i]
	}
	if err := scope.Select(strings.Join(a.columns, ", "), a.args...).Row().Scan(targets...); err != nil {
		return nil, err
	}
	return values, nil
}
