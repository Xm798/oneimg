package s3

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"oneimg/backend/models"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

type recordedRequest struct {
	method string
	host   string
	path   string
}

func TestNewS3ClientUsesPathStyleAgainstConfiguredEndpoint(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []recordedRequest
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		requests = append(requests, recordedRequest{method: r.Method, host: r.Host, path: r.URL.Path})
		mu.Unlock()
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("body"))
		}
	}))
	defer upstream.Close()
	upstreamHost := strings.TrimPrefix(upstream.URL, "http://")

	tests := []struct {
		name     string
		bucket   models.Buckets
		wantPath string
	}{
		{
			name: "s3",
			bucket: models.Buckets{Type: "s3", Config: map[string]any{
				"s3_endpoint": upstream.URL, "s3_bucket": "photos", "s3_access_key": "test", "s3_secret_key": "test-only",
			}},
			wantPath: "/photos/uploads/a.png",
		},
		{
			name: "r2",
			bucket: models.Buckets{Type: "r2", Config: map[string]any{
				"r2_endpoint": upstream.URL, "r2_bucket": "photos", "r2_access_key": "test", "r2_secret_key": "test-only",
			}},
			wantPath: "/photos/uploads/a.png",
		},
		{
			name: "endpoint path prefix",
			bucket: models.Buckets{Type: "s3", Config: map[string]any{
				"s3_endpoint": upstream.URL + "/prefix", "s3_bucket": "photos", "s3_access_key": "test", "s3_secret_key": "test-only",
			}},
			wantPath: "/prefix/photos/uploads/a.png",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mu.Lock()
			requests = nil
			mu.Unlock()
			client, err := NewS3Client(models.Settings{}, tt.bucket)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if _, err := client.PutObject(ctx, &awss3.PutObjectInput{
				Bucket: aws.String("photos"), Key: aws.String("uploads/a.png"), Body: strings.NewReader("body"),
			}); err != nil {
				t.Fatal(err)
			}
			output, err := client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String("photos"), Key: aws.String("uploads/a.png")})
			if err != nil {
				t.Fatal(err)
			}
			_ = output.Body.Close()

			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 2 {
				t.Fatalf("requests = %+v, want PUT and GET", requests)
			}
			for i, method := range []string{http.MethodPut, http.MethodGet} {
				got := requests[i]
				if got.method != method || got.host != upstreamHost || got.path != tt.wantPath {
					t.Fatalf("request %d = %+v, want %s %s%s", i, got, method, upstreamHost, tt.wantPath)
				}
			}
		})
	}
}

func TestNewS3ClientRejectsMissingCredentials(t *testing.T) {
	endpoint := (&url.URL{Scheme: "http", Host: "127.0.0.1:1"}).String()
	_, err := NewS3Client(models.Settings{}, models.Buckets{Type: "s3", Config: map[string]any{"s3_endpoint": endpoint, "s3_bucket": "photos"}})
	if err == nil {
		t.Fatal("missing credentials accepted")
	}
}
