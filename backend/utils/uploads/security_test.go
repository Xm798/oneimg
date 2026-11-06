package uploads

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"oneimg/backend/models"
	"oneimg/backend/utils/images"
	"oneimg/backend/utils/storageobject"
)

func TestS3AndR2ConditionalCreation(t *testing.T) {
	for _, kind := range []string{"s3", "r2"} {
		t.Run(kind, func(t *testing.T) {
			var stored []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || r.Header.Get("If-None-Match") != "*" {
					t.Error("missing conditional PUT", r.Method, r.Header)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/unsupported") {
					w.WriteHeader(http.StatusNotImplemented)
					_, _ = w.Write([]byte(`<Error><Code>NotImplemented</Code></Error>`))
					return
				}
				if stored != nil {
					w.WriteHeader(http.StatusPreconditionFailed)
					_, _ = w.Write([]byte(`<Error><Code>PreconditionFailed</Code></Error>`))
					return
				}
				stored, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusOK)
			}))
			defer upstream.Close()
			cfg := map[string]any{
				kind + "_endpoint": upstream.URL, kind + "_bucket": "test-bucket",
				kind + "_access_key": "test-only-access-key", kind + "_secret_key": "test-only-secret-key",
			}
			writer, err := newObjectWriter(&models.Settings{}, &models.Buckets{Type: kind, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			defer writer.close()
			if err := writer.create(context.Background(), "/object", bytes.NewReader([]byte("original")), "image/png"); err != nil {
				t.Fatal(err)
			}
			if err := writer.create(context.Background(), "/object", bytes.NewReader([]byte("replacement")), "image/png"); !errors.Is(err, storageobject.ErrConflict) {
				t.Fatal("collision was not rejected", err)
			}
			if string(stored) != "original" {
				t.Fatal("original object changed")
			}
			if err := writer.create(context.Background(), "/unsupported", bytes.NewReader([]byte("replacement")), "image/png"); !errors.Is(err, storageobject.ErrOutcomeUncertain) {
				t.Fatal("unsupported conditional creation did not fail closed", err)
			}
		})
	}
}

func TestCollisionRetriesAreBoundedAndUseNewKeys(t *testing.T) {
	images.InitImageService()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	header := uploadHeader(t, "same.png", "image/png", []byte("test-only"))
	setting := &models.Settings{SaveOriginalName: true}
	processed := &preparedUpload{
		ProcessedImage: &images.ProcessedImage{UniqueFileName: images.ImageSvc.BuildOutputFileName(header.Filename, ".png", *setting, 1), OutputExt: ".png"},
		Body:           bytes.NewReader([]byte("test-only")),
	}
	seen := map[string]bool{}
	writer := objectWriter{create: func(_ context.Context, key string, _ io.ReadSeeker, _ string) error {
		if seen[key] {
			t.Error("retry reused a conflicting key")
		}
		seen[key] = true
		return storageobject.ErrConflict
	}, remove: func(context.Context, string) error {
		t.Error("deleted an existing conflicting object")
		return nil
	}}
	_, err := createProcessedUpload(c, setting, &models.Buckets{Type: "default"}, header, processed, writer)
	if !errors.Is(err, storageobject.ErrConflict) || len(seen) != 3 {
		t.Fatal("retry count is not three", len(seen), err)
	}
}

func TestUploadKeysFollowStorageType(t *testing.T) {
	images.InitImageService()
	header := uploadHeader(t, "photo.png", "image/png", []byte("test-only"))
	setting := &models.Settings{DefaultPath: "/img/../{role}"}
	for storage, want := range map[string]string{"default": "/uploads/img/user/", "s3": "/img/user/", "ftp": "/img/user/"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		c.Set("user_role", models.RoleUser)
		processed := &preparedUpload{
			ProcessedImage: &images.ProcessedImage{UniqueFileName: "photo.png", OutputExt: ".png", MimeType: "image/png"},
			Body:           bytes.NewReader([]byte("test-only")),
		}
		var key string
		writer := objectWriter{create: func(_ context.Context, k string, _ io.ReadSeeker, _ string) error {
			key = k
			return nil
		}, remove: func(context.Context, string) error { return nil }}
		if _, err := createProcessedUpload(c, setting, &models.Buckets{Type: storage}, header, processed, writer); err != nil {
			t.Fatal(storage, err)
		}
		if key != want+"photo.png" {
			t.Errorf("%s key = %q, want %q", storage, key, want+"photo.png")
		}
	}
}

func TestLocalUploadOutsideUploadsPathStaysInUploadsDirectory(t *testing.T) {
	t.Chdir(t.TempDir())
	images.InitImageService()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	setting := models.Settings{AllowedTypes: "image/png", MaxFileSize: 1024, DefaultPath: "/data/{year}"}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Set("user_id", 1)
	result, err := (&createdUploader{}).Upload(c, &setting, &models.Buckets{Id: 1, Type: "default"}, uploadHeader(t, "photo.png", "image/png", buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.URL, "/uploads/data/") {
		t.Fatalf("local key = %q, want it under /uploads/data/", result.URL)
	}
	if _, err := os.Stat(strings.TrimPrefix(result.URL, "/")); err != nil {
		t.Fatal("file not written under uploads", err)
	}
	if entries, _ := os.ReadDir("data"); len(entries) != 0 {
		t.Fatalf("media written into data directory: %v", entries)
	}
}

func TestPreparedImageHoldsSlotUntilStorageFinishes(t *testing.T) {
	images.InitImageService()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	setting := &models.Settings{AllowedTypes: "image/png", MaxFileSize: 1024}
	prepared, err := prepareUpload(context.Background(), uploadHeader(t, "photo.png", "image/png", data.Bytes()), setting, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := images.AcquireProcessing(context.Background())
	if release != nil {
		release()
	}
	if !errors.Is(err, images.ErrProcessingBusy) {
		t.Fatal("buffers outlived the processing slot", err)
	}
	prepared.Close()
	_, release, err = images.AcquireProcessing(context.Background())
	if err != nil {
		t.Fatal("slot was not released", err)
	}
	release()
}

func TestExclusiveLocalCreationAndSameNameAcrossUsers(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("existing", []byte("other owner's data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := saveStream("existing", strings.NewReader("replacement")); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	if err := saveFile("existing", []byte("thumbnail replacement")); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	data, _ := os.ReadFile("existing")
	if string(data) != "other owner's data" {
		t.Fatal("existing object truncated")
	}
	images.InitImageService()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	header := uploadHeader(t, "same.png", "image/png", buf.Bytes())
	setting := models.Settings{AllowedTypes: "image/png", MaxFileSize: 1024, SaveOriginalName: true, Thumbnail: true, DefaultPath: "uploads/{filename}"}
	bucket := models.Buckets{Id: 1, Type: "default"}
	var keys []string
	for _, user := range []int{1, 2} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/", nil)
		c.Set("user_id", user)
		result, err := (&createdUploader{}).Upload(c, &setting, &bucket, header)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.CreatedObjects) != 2 {
			t.Fatal("missing cleanup capabilities")
		}
		keys = append(keys, result.URL)
		body, err := os.ReadFile(strings.TrimPrefix(result.URL, "/"))
		if err != nil || !bytes.Equal(body, buf.Bytes()) {
			t.Fatal(err)
		}
		if filepath.Base(result.URL) == filepath.Base(result.ThumbnailURL) {
			t.Fatal("unexpected same extension check")
		}
	}
	if keys[0] == keys[1] {
		t.Fatal("cross-user object reuse")
	}
	writer, err := newObjectWriter(&setting, &bucket)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.create(context.Background(), keys[0], bytes.NewReader([]byte("overwrite")), "image/png"); !errors.Is(err, storageobject.ErrConflict) {
		t.Fatal(err)
	}
}
