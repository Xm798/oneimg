package uploads

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"time"

	"oneimg/backend/interfaces"
	"oneimg/backend/models"
	"oneimg/backend/utils/buckets"
	"oneimg/backend/utils/ftp"
	"oneimg/backend/utils/images"
	"oneimg/backend/utils/s3"
	"oneimg/backend/utils/storageobject"
	"oneimg/backend/utils/webdav"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/gin-gonic/gin"
)

type createdUploader struct{}

func (*createdUploader) Upload(c *gin.Context, s *models.Settings, b *models.Buckets, f *multipart.FileHeader) (*interfaces.ImageUploadResult, error) {
	return uploadCreated(c, s, b, f)
}

type objectWriter struct {
	create func(context.Context, string, io.ReadSeeker, string) error
	remove func(context.Context, string) error
	close  func()
}

func newObjectWriter(setting *models.Settings, bucket *models.Buckets) (objectWriter, error) {
	writer := objectWriter{close: func() {}}
	switch bucket.Type {
	case "default":
		writer.create = func(ctx context.Context, key string, body io.ReadSeeker, _ string) error {
			path := storageobject.LocalPath(key)
			if err := ensureUploadDir(filepath.Dir(path)); err != nil {
				return err
			}
			err := saveStream(path, contextFile{ReadSeeker: body, ctx: ctx})
			if errors.Is(err, os.ErrExist) {
				return storageobject.ErrConflict
			}
			return err
		}
		writer.remove = func(_ context.Context, key string) error { return os.Remove(storageobject.LocalPath(key)) }
	case "s3", "r2":
		client, err := s3.NewS3Client(*setting, *bucket)
		if err != nil {
			return writer, errors.New("cannot initialize object storage")
		}
		name := buckets.ConvertToS3Bucket(bucket.Config).S3Bucket
		if bucket.Type == "r2" {
			name = buckets.ConvertToR2Bucket(bucket.Config).R2Bucket
		}
		writer.create = func(ctx context.Context, key string, body io.ReadSeeker, mime string) error {
			_, err := client.PutObject(ctx, &awss3.PutObjectInput{
				Bucket: aws.String(name), Key: aws.String(strings.TrimPrefix(key, "/")),
				Body: body, ContentType: aws.String(mime), IfNoneMatch: aws.String("*"),
			})
			if err == nil {
				return nil
			}
			var api smithy.APIError
			if errors.As(err, &api) && (api.ErrorCode() == "PreconditionFailed" || api.ErrorCode() == "ConditionalRequestConflict") {
				return storageobject.ErrConflict
			}
			return storageobject.ErrOutcomeUncertain
		}
		writer.remove = func(ctx context.Context, key string) error {
			_, err := client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(name), Key: aws.String(strings.TrimPrefix(key, "/"))})
			return err
		}
	case "webdav":
		cfg := buckets.ConvertToWebDavBucket(bucket.Config)
		client := webdav.Client(webdav.Config{BaseURL: cfg.WebdavURL, Username: cfg.WebdavUser, Password: cfg.WebdavPass, Timeout: 10 * time.Minute})
		writer.create = func(ctx context.Context, key string, body io.ReadSeeker, _ string) error {
			return client.WebDAVCreate(ctx, key, body)
		}
		writer.remove = client.WebDAVDelete
	case "ftp":
		cfg := buckets.ConvertToFTPBucket(bucket.Config)
		client := ftp.NewFTPUtil(ftp.FTPConfig{Host: cfg.FTPHost, Port: cfg.FTPPort, User: cfg.FTPUser, Password: cfg.FTPPass, Timeout: 30})
		writer.create = func(ctx context.Context, key string, body io.ReadSeeker, _ string) error {
			return client.UploadCreate(ctx, key, body)
		}
		writer.remove = func(_ context.Context, key string) error { return client.DeleteImage(key) }
		writer.close = func() { _ = client.Close() }
	default:
		return writer, errors.New("unsupported storage type")
	}
	return writer, nil
}

func uploadCreated(c *gin.Context, setting *models.Settings, bucket *models.Buckets, header *multipart.FileHeader) (*interfaces.ImageUploadResult, error) {
	processed, err := prepareUpload(c.Request.Context(), header, setting, c.GetInt("user_role"))
	if err != nil {
		return nil, err
	}
	defer processed.Close()
	if duplicate := findDuplicateUploadResult(c, bucket, processed.ContentHash); duplicate != nil {
		return duplicate, nil
	}
	writer, err := newObjectWriter(setting, bucket)
	if err != nil {
		return nil, err
	}
	defer writer.close()
	return createProcessedUpload(c, setting, bucket, header, processed, writer)
}

func createProcessedUpload(c *gin.Context, setting *models.Settings, bucket *models.Buckets, header *multipart.FileHeader, processed *preparedUpload, writer objectWriter) (*interfaces.ImageUploadResult, error) {
	subdir := setting.DefaultPath
	if subdir == "" {
		subdir = "uploads/{year}/{month}"
	}
	subdir = objectDir(images.ImageSvc.ReplaceMagicVariables(subdir, header.Filename, c.GetInt("user_role")), bucket.Type)
	name := processed.UniqueFileName
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			name = images.ImageSvc.BuildOutputFileName(header.Filename, processed.OutputExt, *setting, c.GetInt("user_role"))
		}
		key := ensureLeadingSlash(PathJoin(subdir, name))
		thumb := ensureLeadingSlash(buildThumbnailPath(subdir, name))
		if _, err := processed.Body.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		err := writer.create(c.Request.Context(), key, processed.Body, processed.MimeType)
		if errors.Is(err, storageobject.ErrConflict) {
			continue
		}
		if err != nil {
			logUncertainObject(err, bucket.Id, key)
			return nil, err
		}
		created := []interfaces.CreatedObject{{Key: key}}
		thumbnailURL := ""
		var thumbnailSize int64
		if setting.Thumbnail && len(processed.ThumbnailBytes) > 0 {
			err := writer.create(c.Request.Context(), thumb, bytes.NewReader(processed.ThumbnailBytes), "image/webp")
			if errors.Is(err, storageobject.ErrConflict) {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if cleanupErr := writer.remove(ctx, key); cleanupErr != nil {
					log.Printf("对象清理失败，需核查孤儿对象 bucket=%d key=%q", bucket.Id, key)
				}
				cancel()
				continue
			}
			if err == nil {
				thumbnailURL, thumbnailSize = thumb, int64(len(processed.ThumbnailBytes))
				created = append(created, interfaces.CreatedObject{Key: thumb})
			} else {
				logUncertainObject(err, bucket.Id, thumb)
			}
		}
		return &interfaces.ImageUploadResult{
			Success: true, Message: "上传成功", FileName: name, OriginalFileName: header.Filename,
			OriginalFileSize: processed.OriginalSize, FileSize: processed.FileSize, ThumbnailSize: thumbnailSize,
			MimeType: processed.MimeType, URL: key, ThumbnailURL: thumbnailURL, Storage: bucket.Type,
			Width: processed.Width, Height: processed.Height, ContentHash: processed.ContentHash,
			CreatedAt: time.Now().Format("2006-01-02 15:04:05"), CreatedObjects: created,
		}, nil
	}
	return nil, storageobject.ErrConflict
}

func logUncertainObject(err error, bucket int, key string) {
	if errors.Is(err, storageobject.ErrOutcomeUncertain) {
		log.Printf("写入结果不确定，未尝试删除，需核查孤儿对象 bucket=%d key=%q", bucket, key)
	}
}
