package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/textproto"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"oneimg/backend/config"
	"oneimg/backend/models"
	"oneimg/backend/services"
	"oneimg/backend/utils/images"
)

type importFixture struct {
	t      *testing.T
	router *gin.Engine
	admin  string
	member string
	temp   string
}

func loopbackImportPolicy(addr netip.Addr) bool {
	addr = addr.Unmap()
	return addr == netip.MustParseAddr("127.0.0.1") || services.PublicAddr(addr)
}

func newImportFixture(t *testing.T) (*importFixture, *gin.Engine) {
	t.Helper()
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	t.Chdir(t.TempDir())
	images.InitImageService()
	router, system := setupTestRouter(t, &config.Config{SqlitePath: filepath.Join(t.TempDir(), "db"), SessionSecret: "test", ConfigSecret: "test"})
	system.Services.Imports = services.NewRemoteImporter(services.ImportConfig{Policy: loopbackImportPolicy})
	db := system.Database.DB
	for _, bucket := range []models.Buckets{{Id: 1, Name: "primary", Type: "default"}, {Id: 2, Name: "restricted", Type: "default"}, {Id: 3, Name: "disabled", Type: "default", Disabled: true}} {
		if err := db.Create(&bucket).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&models.Settings{}).Where("id = ?", 1).UpdateColumn("default_storage", 1).Error; err != nil {
		t.Fatal(err)
	}
	token := func(username string, role int) string {
		user := models.User{Username: username, Role: role}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		created, err := system.Services.Tokens.Create(user.ID, services.CreateTokenInput{AuthVersion: user.AuthVersion, Name: username, Scopes: []string{"images:write"}})
		if err != nil {
			t.Fatal(err)
		}
		return created.Plain
	}
	return &importFixture{t: t, router: router, admin: token("import-admin", models.RoleAdmin), member: token("import-member", models.RoleUser), temp: temp}, router
}

func (f *importFixture) importRequest(ctx context.Context, token string, body map[string]any) *httptest.ResponseRecorder {
	encoded, _ := json.Marshal(body)
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/image-imports", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	f.router.ServeHTTP(response, request)
	return response
}

func (f *importFixture) importURL(token, rawURL string) *httptest.ResponseRecorder {
	return f.importRequest(context.Background(), token, map[string]any{"url": rawURL})
}

func (f *importFixture) assertNoSpool() {
	f.t.Helper()
	matches, _ := filepath.Glob(filepath.Join(f.temp, "multipart-*"))
	if len(matches) != 0 {
		f.t.Fatalf("spool files left behind: %v", matches)
	}
}

// gatedServer answers /stall/* only after release is closed and everything else at once.
type gatedServer struct {
	*httptest.Server
	hits    atomic.Int64
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedServer(t *testing.T, payload []byte) *gatedServer {
	t.Helper()
	gated := &gatedServer{arrived: make(chan struct{}, 16), release: make(chan struct{})}
	gated.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gated.hits.Add(1)
		if strings.HasPrefix(r.URL.Path, "/stall/") {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			gated.arrived <- struct{}{}
			select {
			case <-gated.release:
			case <-r.Context().Done():
				return
			}
		}
		w.Write(payload)
	}))
	t.Cleanup(func() { gated.Release(); gated.Close() })
	return gated
}

func (g *gatedServer) Release() { g.once.Do(func() { close(g.release) }) }

func (g *gatedServer) waitArrivals(t *testing.T, count int) {
	t.Helper()
	for range count {
		select {
		case <-g.arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("import request did not reach the remote server")
		}
	}
}

func problemCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	json.Unmarshal(response.Body.Bytes(), &problem)
	return problem.Code
}

// downloadedImport reports whether an import got past its download. Two
// downloads released together race for the single fail-fast processing slot,
// so one of them may end with image_processing_busy.
func downloadedImport(t *testing.T, response *httptest.ResponseRecorder) bool {
	return response.Code == http.StatusCreated ||
		(response.Code == http.StatusServiceUnavailable && problemCode(t, response) == "image_processing_busy")
}

// assertPermitsReleased starts two concurrent imports for the admin user; both
// reaching the remote server proves no permit was left behind.
func (f *importFixture) assertPermitsReleased(gated *gatedServer) {
	f.t.Helper()
	responses := make(chan *httptest.ResponseRecorder, 2)
	for i := range 2 {
		go func() { responses <- f.importURL(f.admin, gated.URL+fmt.Sprintf("/stall/check%d.png", i)) }()
	}
	gated.waitArrivals(f.t, 2)
	gated.Release()
	for range 2 {
		if response := <-responses; !downloadedImport(f.t, response) {
			f.t.Fatalf("permit check import: %d %s", response.Code, response.Body.String())
		}
	}
}

func TestImportImageSuccess(t *testing.T) {
	fixture, _ := newImportFixture(t)
	server := newGatedServer(t, testAvatarPNG(t, 12, 8))
	response := fixture.importURL(fixture.admin, server.URL+"/photo.png")
	if response.Code != http.StatusCreated || !strings.HasPrefix(response.Header().Get("Location"), "/api/v1/images/") {
		t.Fatal(response.Code, response.Body.String())
	}
	var envelope struct {
		Data struct {
			Width int `json:"width"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Data.Width != 12 {
		t.Fatal(response.Body.String())
	}
	fixture.assertNoSpool()
}

func TestImportImageSlowServerDoesNotBlockUploads(t *testing.T) {
	fixture, router := newImportFixture(t)
	server := newGatedServer(t, testAvatarPNG(t, 12, 8))
	imported := make(chan *httptest.ResponseRecorder, 1)
	go func() { imported <- fixture.importURL(fixture.admin, server.URL+"/stall/slow.png") }()
	server.waitArrivals(t, 1)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, _ := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="images"; filename="other.png"`},
		"Content-Type":        {"image/png"},
	})
	part.Write(testAvatarPNG(t, 20, 8))
	writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/images", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+fixture.member)
	upload := httptest.NewRecorder()
	router.ServeHTTP(upload, request)
	if upload.Code != http.StatusOK || strings.Contains(upload.Body.String(), "image_processing_busy") || !strings.Contains(upload.Body.String(), `"succeeded":1`) {
		t.Fatalf("upload during a slow import: %d %s", upload.Code, upload.Body.String())
	}

	server.Release()
	if response := <-imported; response.Code != http.StatusCreated {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestImportImageChecksAccessBeforeDownload(t *testing.T) {
	fixture, _ := newImportFixture(t)
	server := newGatedServer(t, testAvatarPNG(t, 12, 8))
	for _, test := range []struct {
		name  string
		token string
		body  map[string]any
		code  int
	}{
		{"inaccessible bucket", fixture.member, map[string]any{"bucket_id": 2}, http.StatusForbidden},
		{"disabled storage", fixture.admin, map[string]any{"bucket_id": 3}, http.StatusConflict},
		{"unknown tag", fixture.admin, map[string]any{"tag_ids": []int{999}}, http.StatusUnprocessableEntity},
		{"unknown bucket", fixture.admin, map[string]any{"bucket_id": 99}, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.body["url"] = server.URL + "/photo.png"
			if response := fixture.importRequest(context.Background(), test.token, test.body); response.Code != test.code {
				t.Fatal(response.Code, response.Body.String())
			}
		})
	}
	if server.hits.Load() != 0 {
		t.Fatalf("remote server was contacted %d times before authorization failed", server.hits.Load())
	}
}

func TestImportImagePerUserConcurrency(t *testing.T) {
	fixture, _ := newImportFixture(t)
	server := newGatedServer(t, testAvatarPNG(t, 12, 8))
	responses := make(chan *httptest.ResponseRecorder, 2)
	for _, name := range []string{"a", "b"} {
		go func() { responses <- fixture.importURL(fixture.admin, server.URL+"/stall/"+name+".png") }()
	}
	server.waitArrivals(t, 2)

	third := fixture.importURL(fixture.admin, server.URL+"/third.png")
	if third.Code != http.StatusTooManyRequests || problemCode(t, third) != "import_concurrency_exceeded" || third.Header().Get("Retry-After") != "5" {
		t.Fatal(third.Code, third.Header(), third.Body.String())
	}
	if other := fixture.importURL(fixture.member, server.URL+"/other.png"); other.Code != http.StatusCreated {
		t.Fatal(other.Code, other.Body.String())
	}

	server.Release()
	for range 2 {
		if response := <-responses; !downloadedImport(t, response) {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}

func TestImportImageProcessingBusy(t *testing.T) {
	fixture, _ := newImportFixture(t)
	server := newGatedServer(t, testAvatarPNG(t, 12, 8))
	_, release, err := images.AcquireProcessing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	response := fixture.importURL(fixture.admin, server.URL+"/busy.png")
	release()
	if response.Code != http.StatusServiceUnavailable || problemCode(t, response) != "image_processing_busy" || response.Header().Get("Retry-After") != "2" {
		t.Fatal(response.Code, response.Header(), response.Body.String())
	}
	fixture.assertNoSpool()
	fixture.assertPermitsReleased(server)
}

func TestImportImageClientCancel(t *testing.T) {
	fixture, _ := newImportFixture(t)
	server := newGatedServer(t, testAvatarPNG(t, 12, 8))
	ctx, cancel := context.WithCancel(context.Background())
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- fixture.importRequest(ctx, fixture.admin, map[string]any{"url": server.URL + "/stall/cancel.png"})
	}()
	server.waitArrivals(t, 1)
	cancel()
	select {
	case response := <-responses:
		if response.Code != 499 || response.Body.Len() != 0 {
			t.Fatal(response.Code, response.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("import did not stop after the client went away")
	}
	fixture.assertNoSpool()
	fixture.assertPermitsReleased(server)
}
