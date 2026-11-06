package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"oneimg/backend/database"
	"oneimg/backend/models"
	utilsBuckets "oneimg/backend/utils/buckets"
	"oneimg/backend/utils/ftp"
	"oneimg/backend/utils/publicurl"
	"oneimg/backend/utils/s3"
	"oneimg/backend/utils/secureconfig"
	"oneimg/backend/utils/webdav"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"gorm.io/gorm"
)

var (
	ErrStorageTypeInvalid     = errors.New("invalid storage type")
	ErrStorageConfigInvalid   = errors.New("invalid storage configuration")
	ErrDefaultStorage         = errors.New("default storage cannot be modified")
	ErrStorageInUseAsDefault  = errors.New("storage is the current upload default")
	ErrStorageDisabled        = errors.New("storage is disabled")
	ErrStorageCapacity        = errors.New("storage capacity is invalid")
	ErrPermissionDenied       = errors.New("permission denied")
	ErrNewEndpointCredentials = errors.New("credentials required for new endpoint")
)

// StorageDomainError 表示直链域名无效，Message 可以直接展示给用户。
type StorageDomainError struct {
	Message string
}

func (e *StorageDomainError) Error() string { return e.Message }

type FilesystemMetrics struct {
	TotalBytes     uint64
	UsedBytes      uint64
	AvailableBytes uint64
}

type StorageBucketSummary struct {
	Bucket     models.Buckets
	UsageBytes uint64
	UsageExact bool
	Filesystem *FilesystemMetrics
}

type StorageService struct {
	db                *gorm.DB
	uploadsPath       string
	filesystemMetrics func(string) (*FilesystemMetrics, error)
}

type StorageInput struct {
	Name          string
	Type          string
	CapacityBytes uint64
	Config        map[string]any
}

func NewStorageService(db *gorm.DB) *StorageService {
	return &StorageService{db: db, uploadsPath: "uploads", filesystemMetrics: readFilesystemMetrics}
}

func (s *StorageService) List() ([]StorageBucketSummary, error) {
	items := make([]models.Buckets, 0)
	if err := s.db.Where("type <> ?", "telegram").Order("id ASC").Find(&items).Error; err != nil {
		return nil, err
	}
	return s.summarize(items)
}

func (s *StorageService) UploadOptions(user models.User) (models.Settings, []models.Buckets, error) {
	var setting models.Settings
	if err := s.db.First(&setting).Error; err != nil {
		return setting, nil, err
	}
	var buckets []models.Buckets
	if err := s.db.Where("type <> ?", "telegram").Order("id ASC").Find(&buckets).Error; err != nil {
		return setting, nil, err
	}
	result := make([]models.Buckets, 0, len(buckets))
	for _, bucket := range buckets {
		if bucket.Disabled {
			continue
		}
		if bucket.Id != setting.DefaultStorage {
			if bucket.Capacity > 0 && bucket.Usage >= bucket.Capacity {
				continue
			}
			if user.Role != models.RoleAdmin && !models.IntSliceContains(user.Permission.Buckets, bucket.Id) {
				continue
			}
		}
		bucket.Config = nil
		result = append(result, bucket)
	}
	return setting, result, nil
}

func (s *StorageService) Get(id int) (StorageBucketSummary, error) {
	var item models.Buckets
	if err := s.db.First(&item, id).Error; err != nil {
		return StorageBucketSummary{}, err
	}
	items, err := s.summarize([]models.Buckets{item})
	if err != nil {
		return StorageBucketSummary{}, err
	}
	return items[0], nil
}

func (s *StorageService) summarize(items []models.Buckets) ([]StorageBucketSummary, error) {
	aggregates, err := loadStorageAggregates(s.db)
	if err != nil {
		return nil, err
	}
	result := make([]StorageBucketSummary, 0, len(items))
	for _, item := range items {
		aggregate := aggregates[item.Id]
		usageExact := aggregate.UnknownCount == 0
		usageBytes := nonNegativeBytes(aggregate.ExactBytes)
		if !usageExact {
			usageBytes = maxUint64(item.Usage, nonNegativeBytes(aggregate.FallbackBytes))
		}
		item.Config = secureconfig.MaskBucketConfigValues(item.Config)
		summary := StorageBucketSummary{Bucket: item, UsageBytes: usageBytes, UsageExact: usageExact}
		if item.Type == "default" && s.filesystemMetrics != nil {
			if filesystem, metricErr := s.filesystemMetrics(s.uploadsPath); metricErr == nil {
				summary.Filesystem = filesystem
			}
		}
		result = append(result, summary)
	}
	return result, nil
}

func readFilesystemMetrics(path string) (*FilesystemMetrics, error) {
	var stats unix.Statfs_t
	if err := unix.Statfs(path, &stats); err != nil {
		return nil, err
	}
	blockSize := filesystemBlockSize(stats)
	total := stats.Blocks * blockSize
	free := stats.Bfree * blockSize
	available := stats.Bavail * blockSize
	used := uint64(0)
	if total >= free {
		used = total - free
	}
	return &FilesystemMetrics{TotalBytes: total, UsedBytes: used, AvailableBytes: available}, nil
}

func validateStorageInput(input StorageInput, allowDefault bool) (StorageInput, error) {
	if input.CapacityBytes > math.MaxInt64 {
		return input, ErrStorageCapacity
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Type = strings.ToLower(strings.TrimSpace(input.Type))
	valid := map[string]bool{"s3": true, "r2": true, "ftp": true, "webdav": true}
	if allowDefault {
		valid["default"] = true
	}
	if input.Name == "" || !valid[input.Type] {
		return input, ErrStorageTypeInvalid
	}
	required := map[string][]string{
		"s3":     {"s3_endpoint", "s3_access_key", "s3_secret_key", "s3_bucket"},
		"r2":     {"r2_endpoint", "r2_access_key", "r2_secret_key", "r2_bucket"},
		"ftp":    {"ftp_host", "ftp_port", "ftp_user", "ftp_pass"},
		"webdav": {"webdav_url", "webdav_user", "webdav_pass"},
	}
	for _, key := range required[input.Type] {
		if key == "ftp_port" {
			port := secureconfig.GetInt(input.Config, key)
			if port < 1 || port > 65535 {
				return input, ErrStorageConfigInvalid
			}
			continue
		}
		if strings.TrimSpace(secureconfig.GetString(input.Config, key)) == "" {
			return input, ErrStorageConfigInvalid
		}
	}
	if err := normalizeStorageDomain(&input); err != nil {
		return input, err
	}
	return input, nil
}

// normalizeStorageDomain 校验并规范 S3/R2 的直链域名；其他类型不经存储域名访问，不能保存该字段。
func normalizeStorageDomain(input *StorageInput) error {
	raw, exists := input.Config[publicurl.ConfigKey]
	if !exists {
		return nil
	}
	value, ok := raw.(string)
	if raw != nil && !ok {
		return &StorageDomainError{Message: "访问域名必须是字符串"}
	}
	if !publicurl.SupportsStorage(input.Type) {
		if strings.TrimSpace(value) != "" {
			return &StorageDomainError{Message: "只有 S3 和 R2 存储可以设置直链域名"}
		}
		delete(input.Config, publicurl.ConfigKey)
		return nil
	}
	domain, err := publicurl.NormalizeDomain(value)
	if err != nil {
		return &StorageDomainError{Message: err.Error()}
	}
	if domain == "" {
		delete(input.Config, publicurl.ConfigKey)
	} else {
		input.Config[publicurl.ConfigKey] = domain
	}
	return nil
}

func (s *StorageService) Create(input StorageInput) (models.Buckets, error) {
	input, err := validateStorageInput(input, false)
	if err != nil {
		return models.Buckets{}, err
	}
	config, err := secureconfig.EncryptBucketConfigValues(input.Config)
	if err != nil {
		return models.Buckets{}, err
	}
	item := models.Buckets{Name: input.Name, Type: input.Type, Capacity: input.CapacityBytes, Config: config}
	if err := s.db.Create(&item).Error; err != nil {
		return models.Buckets{}, err
	}
	item.Config = secureconfig.MaskBucketConfigValues(item.Config)
	return item, nil
}

func (s *StorageService) Update(id int, input StorageInput) (models.Buckets, error) {
	storageTopology.Lock()
	defer storageTopology.Unlock()
	if id == 1 {
		return models.Buckets{}, ErrDefaultStorage
	}
	var existing models.Buckets
	if err := s.db.First(&existing, id).Error; err != nil {
		return existing, err
	}
	if input.Type != existing.Type {
		return existing, ErrStorageTypeInvalid
	}
	merged, err := mergeStorageConfig(existing, input.Config)
	if err != nil {
		return existing, err
	}
	input.Config = merged
	input, err = validateStorageInput(input, false)
	if err != nil {
		return existing, err
	}
	if input.CapacityBytes > 0 && input.CapacityBytes < existing.Usage {
		return existing, ErrStorageCapacity
	}
	encrypted, err := secureconfig.EncryptBucketConfigValues(input.Config)
	if err != nil {
		return existing, err
	}
	changes := models.Buckets{Name: input.Name, Capacity: input.CapacityBytes, Config: encrypted}
	if err := s.db.Model(&existing).Select("Name", "Capacity", "Config").Updates(&changes).Error; err != nil {
		return existing, err
	}
	existing.Name, existing.Capacity, existing.Config = input.Name, input.CapacityBytes, secureconfig.MaskBucketConfigValues(encrypted)
	return existing, nil
}

// SetDisabled 停用或启用存储。停用只拒绝新上传，不改动已有记录、授权和物理对象。
func (s *StorageService) SetDisabled(id int, disabled bool) (models.Buckets, error) {
	storageTopology.Lock()
	defer storageTopology.Unlock()
	if id == 1 {
		return models.Buckets{}, ErrDefaultStorage
	}
	var bucket models.Buckets
	if err := s.db.First(&bucket, id).Error; err != nil {
		return bucket, err
	}
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if disabled {
			var defaultRefs int64
			if err := tx.Model(&models.Settings{}).Where("default_storage = ?", id).Count(&defaultRefs).Error; err != nil {
				return err
			}
			if defaultRefs > 0 {
				return ErrStorageInUseAsDefault
			}
		}
		return tx.Model(&models.Buckets{}).Where("id = ?", id).Update("disabled", disabled).Error
	})
	if err != nil {
		return bucket, err
	}
	bucket.Disabled = disabled
	return bucket, nil
}

// Delete 删除存储及其图片记录、标签关联和用户授权。purgeObjects 为 false 时保留远端物理对象。
func (s *StorageService) Delete(ctx context.Context, id int, purgeObjects bool) error {
	storageTopology.RLock()
	defer storageTopology.RUnlock()
	if id == 1 {
		return ErrDefaultStorage
	}
	unlockBucket := lockBucket(id, true)
	defer unlockBucket()
	var bucket models.Buckets
	if err := s.db.First(&bucket, id).Error; err != nil {
		return err
	}
	// 删除前拒绝当前上传默认存储，避免上传默认值被静默改回本地存储。
	var defaultRefs int64
	if err := s.db.Model(&models.Settings{}).Where("default_storage = ?", id).Count(&defaultRefs).Error; err != nil {
		return err
	}
	if defaultRefs > 0 {
		return ErrStorageInUseAsDefault
	}
	if purgeObjects {
		images, keys, err := bucketImageKeys(s.db, bucket)
		if err != nil {
			return err
		}
		unlock := lockObjects(keys)
		defer unlock()
		if err := purgeBucketObjects(ctx, s.db, bucket, images); err != nil {
			return err
		}
	}
	return s.deleteBucketRecords(id, bucket)
}

func bucketImageKeys(db *gorm.DB, bucket models.Buckets) ([]models.Image, []string, error) {
	var images []models.Image
	if err := db.Where("bucket_id = ?", bucket.Id).Find(&images).Error; err != nil {
		return nil, nil, err
	}
	var keys []string
	for _, image := range images {
		imageKeys, err := imageObjectKeys(image, bucket)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: physical identity is unavailable", ErrStorageDelete)
		}
		keys = append(keys, imageKeys...)
	}
	return images, keys, nil
}

// purgeBucketObjects 删除桶内未被其他记录引用的物理对象；调用方需持有对象锁。
func purgeBucketObjects(ctx context.Context, db *gorm.DB, bucket models.Buckets, images []models.Image) error {
	ids := make([]int, 0, len(images))
	for _, image := range images {
		ids = append(ids, image.Id)
	}
	deleted := map[string]bool{}
	for _, image := range images {
		target, err := unreferencedImageObjects(db, image, bucket, ids)
		if err != nil {
			return err
		}
		target, err = distinctDeletionTarget(target, bucket, deleted)
		if err != nil {
			return fmt.Errorf("%w: physical identity is unavailable", ErrStorageDelete)
		}
		if err := deleteStoredImage(ctx, target, bucket); err != nil {
			return fmt.Errorf("%w: %v", ErrStorageDelete, err)
		}
	}
	return nil
}

func (s *StorageService) deleteBucketRecords(id int, bucket models.Buckets) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("image_id IN (SELECT id FROM images WHERE bucket_id = ?)", id).Delete(&models.ImageToTags{}).Error; err != nil {
			return err
		}
		if err := tx.Where("bucket_id = ?", id).Delete(&models.Image{}).Error; err != nil {
			return err
		}
		var users []models.User
		if err := lockUsersForUpdate(tx).Find(&users).Error; err != nil {
			return err
		}
		for _, user := range users {
			filtered := make([]int, 0, len(user.Permission.Buckets))
			for _, bucketID := range user.Permission.Buckets {
				if bucketID != id {
					filtered = append(filtered, bucketID)
				}
			}
			if len(filtered) != len(user.Permission.Buckets) {
				// Rewriting only the buckets key keeps the codes as currently
				// committed, including a revocation that commits meanwhile.
				expr, err := database.JSONSetKey(tx, "permission", "buckets", filtered)
				if err != nil {
					return err
				}
				if err := tx.Model(&models.User{}).Where("id = ?", user.ID).Update("permission", expr).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Model(&models.Settings{}).Where("default_storage = ?", id).Update("default_storage", 1).Error; err != nil {
			return err
		}
		return tx.Delete(&bucket).Error
	})
}

func (s *StorageService) TestConnection(ctx context.Context, actor models.User, input StorageInput, existingID *int) (string, error) {
	canUpdate := models.Can(actor, "storage:update")
	if (existingID != nil && !canUpdate) || (existingID == nil && !models.Can(actor, "storage:create") && !canUpdate) {
		return "", ErrPermissionDenied
	}
	if existingID != nil {
		var existing models.Buckets
		if err := s.db.First(&existing, *existingID).Error; err != nil {
			return "", err
		}
		if input.Type != "" && input.Type != existing.Type {
			return "", ErrStorageTypeInvalid
		}
		merged, err := mergeStorageConfig(existing, input.Config)
		if err != nil {
			return "", err
		}
		if input.Name == "" {
			input.Name = existing.Name
		}
		if input.Type == "" {
			input.Type = existing.Type
		}
		if input.CapacityBytes == 0 {
			input.CapacityBytes = existing.Capacity
		}
		input.Config = merged
	}
	input, err := validateStorageInput(input, true)
	if err != nil {
		return "", err
	}
	bucket := models.Buckets{Name: input.Name, Type: input.Type, Capacity: input.CapacityBytes, Config: input.Config}
	switch bucket.Type {
	case "default":
		if err := os.MkdirAll("uploads", 0755); err != nil {
			return "", err
		}
		file, err := os.CreateTemp("uploads", ".oneimg-storage-test-*")
		if err != nil {
			return "", err
		}
		name := file.Name()
		defer os.Remove(name)
		if _, err := file.WriteString("oneimg storage test"); err != nil {
			file.Close()
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
		return "local storage is writable", nil
	case "s3", "r2":
		client, err := s3.NewS3Client(models.Settings{}, bucket)
		if err != nil {
			return "", err
		}
		bucketName := utilsBuckets.ConvertToS3Bucket(bucket.Config).S3Bucket
		if bucket.Type == "r2" {
			bucketName = utilsBuckets.ConvertToR2Bucket(bucket.Config).R2Bucket
		}
		key := ".oneimg-connection-test/" + uuid.NewString() + ".txt"
		if _, err := client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(bucketName), Key: aws.String(key), Body: bytes.NewReader([]byte("oneimg storage test"))}); err != nil {
			return "", err
		}
		if _, err := client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(bucketName), Key: aws.String(key)}); err != nil {
			return "", err
		}
		return "object write and delete succeeded", nil
	case "ftp":
		config := utilsBuckets.ConvertToFTPBucket(bucket.Config)
		client := ftp.NewFTPUtil(ftp.FTPConfig{Host: config.FTPHost, Port: config.FTPPort, User: config.FTPUser, Password: config.FTPPass, Timeout: 10})
		defer client.Close()
		path := ".oneimg-connection-test-" + uuid.NewString() + ".txt"
		if err := client.UploadImage(path, []byte("oneimg storage test"), "text/plain"); err != nil {
			return "", err
		}
		if err := client.DeleteImage(path); err != nil {
			return "", err
		}
		return "file write and delete succeeded", nil
	case "webdav":
		config := utilsBuckets.ConvertToWebDavBucket(bucket.Config)
		client := webdav.Client(webdav.Config{BaseURL: config.WebdavURL, Username: config.WebdavUser, Password: config.WebdavPass, Timeout: 10 * time.Second})
		path := "/.oneimg-connection-test-" + uuid.NewString() + ".txt"
		if err := client.WebDAVUpload(ctx, path, bytes.NewReader([]byte("oneimg storage test"))); err != nil {
			return "", err
		}
		if err := client.WebDAVDelete(ctx, path); err != nil {
			return "", err
		}
		return "file write and delete succeeded", nil
	default:
		return "", ErrStorageTypeInvalid
	}
}

var _ = gorm.ErrRecordNotFound
