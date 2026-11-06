package uploads

import (
	"testing"

	"oneimg/backend/models"
)

func TestBuildThumbnailPathUsesIndependentPrefix(t *testing.T) {
	tests := []struct {
		name     string
		subDir   string
		fileName string
		want     string
	}{
		{
			name:     "uploads path strips uploads prefix",
			subDir:   "uploads/2026/07",
			fileName: "photo.png",
			want:     "thumbnails/2026/07/photo.webp",
		},
		{
			name:     "leading slash path",
			subDir:   "/uploads/2026/07",
			fileName: "photo.webp",
			want:     "thumbnails/2026/07/photo.webp",
		},
		{
			name:     "custom path stays under thumbnails",
			subDir:   "custom/dir",
			fileName: "photo",
			want:     "thumbnails/custom/dir/photo.webp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildThumbnailPath(tt.subDir, tt.fileName); got != tt.want {
				t.Fatalf("buildThumbnailPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLocalThumbnailFilePathUsesDataDirectory(t *testing.T) {
	got := localThumbnailFilePath("/thumbnails/2026/07/photo.webp")
	want := "data/thumbnails/2026/07/photo.webp"
	if got != want && got != "./"+want {
		t.Fatalf("localThumbnailFilePath() = %q, want %q", got, want)
	}
}

func TestObjectDirKeepsLocalUploadsUnderUploadsAndDropsDotSegments(t *testing.T) {
	tests := []struct {
		dir, storage, want string
	}{
		{"/uploads/2026/10", "default", "uploads/2026/10"},
		{"uploads", "default", "uploads"},
		{"", "default", "uploads"},
		{"/img/2026", "default", "uploads/img/2026"},
		{"/data/2026", "default", "uploads/data/2026"},
		{"/uploadsx/2026", "default", "uploads/uploadsx/2026"},
		{"/uploads/../../etc", "default", "uploads/etc"},
		{`uploads\..\2026`, "default", "uploads/2026"},
		{"/img/2026", "s3", "img/2026"},
		{"/2026/10", "r2", "2026/10"},
		{"", "r2", ""},
		{"/uploads/../../2026", "ftp", "uploads/2026"},
		{"/./a/../b", "webdav", "a/b"},
	}
	for _, tt := range tests {
		if got := objectDir(tt.dir, tt.storage); got != tt.want {
			t.Errorf("objectDir(%q, %q) = %q, want %q", tt.dir, tt.storage, got, tt.want)
		}
	}
}

func TestGetStorageUploaderRejectsRemovedTelegramStorage(t *testing.T) {
	_, err := GetStorageUploader(&models.Buckets{Type: "telegram"})
	if err == nil {
		t.Fatal("GetStorageUploader() should reject removed Telegram storage")
	}
}

func TestGetStorageUploaderSupportsEveryStorageType(t *testing.T) {
	for _, storageType := range []string{"default", "r2", "s3", "webdav", "ftp"} {
		uploader, err := GetStorageUploader(&models.Buckets{Type: storageType})
		if err != nil || uploader == nil {
			t.Fatalf("GetStorageUploader(%q) = %v, %v; want uploader", storageType, uploader, err)
		}
	}
	if uploader, err := GetStorageUploader(&models.Buckets{Type: "gcs"}); err == nil || uploader != nil {
		t.Fatalf("GetStorageUploader(gcs) = %v, %v; want error", uploader, err)
	}
}
