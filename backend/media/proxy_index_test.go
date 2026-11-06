package media

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oneimg/backend/database"
	"oneimg/backend/models"
	"oneimg/internal/testdb"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestMediaLookupQueryPlanUsesIndex(t *testing.T) {
	db := testdb.New(t).DB
	rows := make([]models.Image, 0, 2000)
	for i := range 2000 {
		rows = append(rows, models.Image{Url: fmt.Sprintf("/uploads/2026/01/%d.png", i), Thumbnail: fmt.Sprintf("/thumbnails/2026/01/%d.webp", i), FileName: "x.png"})
	}
	if err := db.CreateInBatches(rows, 500).Error; err != nil {
		t.Fatal(err)
	}
	statement := mediaLookup(db.Session(&gorm.Session{DryRun: true}), "/uploads/2026/01/7.png").Find(&models.Image{}).Statement
	var plan []string
	err := db.Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "sqlite" {
			var rows []struct{ Detail string }
			if err := tx.Raw("EXPLAIN QUERY PLAN "+statement.SQL.String(), statement.Vars...).Scan(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				plan = append(plan, row.Detail)
			}
			return nil
		}
		if err := tx.Exec("ANALYZE images").Error; err != nil {
			return err
		}
		// Small test tables make a sequential scan the cheapest plan; this
		// asks whether an index plan is available at all.
		if err := tx.Exec("SET LOCAL enable_seqscan = off").Error; err != nil {
			return err
		}
		return tx.Raw("EXPLAIN "+statement.SQL.String(), statement.Vars...).Scan(&plan).Error
	})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(plan, "\n")
	fullScan := strings.Contains(text, "Seq Scan")
	for _, line := range plan {
		fullScan = fullScan || strings.HasPrefix(strings.TrimSpace(line), "SCAN images")
	}
	if fullScan || !strings.Contains(text, "idx_images_url") || !strings.Contains(text, "idx_images_thumbnail") {
		t.Fatalf("media lookup does not use both indexes:\n%s", text)
	}
	t.Logf("%s plan:\n%s", db.Dialector.Name(), text)
}

func TestImageProxyResolvesHistoricalPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testdb.New(t)
	previous := database.GetDB()
	database.SetDB(db)
	t.Cleanup(func() { database.SetDB(previous) })
	t.Chdir(t.TempDir())
	if err := db.DB.Create(&models.Settings{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Create(&models.Buckets{Id: 1, Name: "local", Type: "default"}).Error; err != nil {
		t.Fatal(err)
	}
	image := models.Image{Url: "/uploads/2024/05/legacy.png", Thumbnail: "/thumbnails/2024/05/legacy.webp", FileName: "legacy.png", MimeType: "image/png", Storage: "default", BucketId: 1}
	if err := db.DB.Create(&image).Error; err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{"uploads/2024/05/legacy.png": "original", "data/thumbnails/2024/05/legacy.webp": "thumbnail"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		path, body, contentType string
		handled                 bool
	}{
		{image.Url, "original", "image/png", true},
		{image.Thumbnail, "thumbnail", "image/webp", true},
		{"/uploads/2024/05/missing.png", "", "", false},
	} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, tt.path, nil)
		if handled := ImageProxy(c); handled != tt.handled {
			t.Fatalf("%s handled=%v", tt.path, handled)
		}
		if !tt.handled {
			continue
		}
		if recorder.Code != http.StatusOK || recorder.Body.String() != tt.body || !strings.HasPrefix(recorder.Header().Get("Content-Type"), tt.contentType) {
			t.Fatalf("%s: %d %q %q", tt.path, recorder.Code, recorder.Body.String(), recorder.Header().Get("Content-Type"))
		}
	}
}
