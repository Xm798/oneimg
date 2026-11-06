package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"oneimg/backend/database"
	"oneimg/backend/models"
	utilsBuckets "oneimg/backend/utils/buckets"
	"oneimg/backend/utils/ftp"
	"oneimg/backend/utils/publicurl"
	"oneimg/backend/utils/s3"
	"oneimg/backend/utils/webdav"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"gorm.io/gorm"
)

var ErrImageListQuery = errors.New("invalid image list query")

var ErrStorageDelete = errors.New("stored image could not be deleted")

type ImageService struct{ db *gorm.DB }

type ImageListQuery struct {
	MediaType    string
	Page         int
	PageSize     int
	Sort         string
	Order        string
	Search       string
	BucketID     *int
	TagIDs       []int
	Untagged     bool
	UploaderRole *int
	User         models.User
}

type ImageRecord struct {
	Image        models.Image
	Tags         []models.Tags
	UploaderRole int
}

func NewImageService(db *gorm.DB) *ImageService { return &ImageService{db: db} }

func (s *ImageService) List(input ImageListQuery) ([]ImageRecord, int64, error) {
	sortFields := map[string]string{"created_at": "images.created_at", "file_size": "images.file_size", "filename": "images.file_name", "id": "images.id"}
	if input.Sort == "" {
		input.Sort = "created_at"
	}
	input.Order = strings.ToUpper(input.Order)
	if input.Order == "" {
		input.Order = "DESC"
	}
	if sortFields[input.Sort] == "" || (input.Order != "ASC" && input.Order != "DESC") || input.Page < 1 || input.PageSize < 1 {
		return nil, 0, ErrImageListQuery
	}
	query := s.db.Model(&models.Image{})
	if input.MediaType == "video" {
		query = query.Where(models.VideoSQL)
	} else if input.MediaType == "image" {
		query = query.Where("NOT (" + models.VideoSQL + ")")
	}
	if input.User.Role != models.RoleAdmin {
		query = query.Where("images.user_id = ?", input.User.ID)
	}
	if input.BucketID != nil {
		query = query.Where("images.bucket_id = ?", *input.BucketID)
	}
	if strings.TrimSpace(input.Search) != "" {
		pattern := "%" + models.SearchKey(strings.TrimSpace(input.Search)) + "%"
		expressions := make([]string, 0, 4)
		for _, column := range []string{"images.file_name_search", "images.original_filename_search", "images.url_search", "images.content_hash_search"} {
			expressions = append(expressions, database.SearchLike(s.db, column))
		}
		query = query.Where("("+strings.Join(expressions, " OR ")+")", pattern, pattern, pattern, pattern)
	}
	if input.UploaderRole != nil {
		query = query.Where("EXISTS (SELECT 1 FROM users uploader WHERE uploader.id = images.user_id AND uploader.role = ?)", *input.UploaderRole)
	}
	if input.Untagged {
		query = query.Where("NOT EXISTS (SELECT 1 FROM image_to_tags WHERE image_to_tags.image_id = images.id)")
	}
	if len(input.TagIDs) > 0 {
		query = query.Where("EXISTS (SELECT 1 FROM image_to_tags WHERE image_to_tags.image_id = images.id AND image_to_tags.tag_id IN ?)", input.TagIDs)
	}

	var total int64
	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	field := sortFields[input.Sort]
	if input.Sort == "filename" {
		field = database.BinaryOrder(s.db, field)
	}
	if input.Sort == "created_at" && s.db.Dialector.Name() == "sqlite" {
		field = "julianday(images.created_at)"
	}
	orderClause := field + " " + input.Order
	if input.Sort == "created_at" {
		orderClause = "CASE WHEN images.created_at IS NULL THEN 0 ELSE 1 END " + input.Order + ", " + orderClause
	}
	if input.Sort != "id" {
		orderClause += ", images.id " + input.Order
	}
	ids := make([]int, 0)
	if err := query.Session(&gorm.Session{}).Select("images.id").Order(orderClause).Offset((input.Page - 1) * input.PageSize).Limit(input.PageSize).Find(&ids).Error; err != nil {
		return nil, 0, err
	}
	if len(ids) == 0 {
		return []ImageRecord{}, total, nil
	}
	var fetched []models.Image
	if err := s.db.Where("id IN ?", ids).Find(&fetched).Error; err != nil {
		return nil, 0, err
	}
	byID := make(map[int]models.Image, len(fetched))
	for _, image := range fetched {
		byID[image.Id] = image
	}
	images := make([]models.Image, 0, len(ids))
	for _, id := range ids {
		if image, ok := byID[id]; ok {
			images = append(images, image)
		}
	}
	records, err := s.attachMetadata(images)
	if err != nil {
		return nil, 0, err
	}
	return records, total, nil
}

// Storages 返回当前用户可见媒体实际引用的存储名称，供图库展示与筛选；
// 与上传选项不同，已停用、容量已满或无上传权限的存储同样返回。
func (s *ImageService) Storages(user models.User) ([]models.Buckets, error) {
	referenced := s.db.Model(&models.Image{}).Distinct("bucket_id")
	if user.Role != models.RoleAdmin {
		referenced = referenced.Where("user_id = ?", user.ID)
	}
	items := make([]models.Buckets, 0)
	err := s.db.Select("id", "name", "type", "disabled").Where("id IN (?)", referenced).Order("id ASC").Find(&items).Error
	return items, err
}

func (s *ImageService) Get(id int) (ImageRecord, error) {
	var image models.Image
	if err := s.db.First(&image, id).Error; err != nil {
		return ImageRecord{}, err
	}
	items, err := s.attachMetadata([]models.Image{image})
	if err != nil {
		return ImageRecord{}, err
	}
	return items[0], nil
}

func (s *ImageService) GetMany(ids []int) ([]models.Image, error) {
	items := make([]models.Image, 0)
	err := s.db.Where("id IN ?", uniqueInts(ids)).Find(&items).Error
	if err == nil && len(items) != len(uniqueInts(ids)) {
		err = gorm.ErrRecordNotFound
	}
	return items, err
}

func (s *ImageService) attachMetadata(images []models.Image) ([]ImageRecord, error) {
	ids := make([]int, 0, len(images))
	userIDs := make([]int, 0, len(images))
	bucketIDs := make([]int, 0, len(images))
	for _, image := range images {
		ids = append(ids, image.Id)
		userIDs = append(userIDs, image.UserId)
		bucketIDs = append(bucketIDs, image.BucketId)
	}
	var links []models.ImageToTags
	if err := s.db.Where("image_id IN ?", ids).Find(&links).Error; err != nil {
		return nil, err
	}
	tagIDs := make([]int, 0, len(links))
	for _, link := range links {
		tagIDs = append(tagIDs, link.TagId)
	}
	var tags []models.Tags
	if len(tagIDs) > 0 {
		if err := s.db.Where("id IN ?", uniqueInts(tagIDs)).Find(&tags).Error; err != nil {
			return nil, err
		}
	}
	tagByID := make(map[int]models.Tags, len(tags))
	for _, tag := range tags {
		tagByID[tag.Id] = tag
	}
	tagsByImage := make(map[int][]models.Tags, len(images))
	for _, link := range links {
		if tag, ok := tagByID[link.TagId]; ok {
			tagsByImage[link.ImageId] = append(tagsByImage[link.ImageId], tag)
		}
	}
	var users []models.User
	if err := s.db.Select("id", "role").Where("id IN ?", uniqueInts(userIDs)).Find(&users).Error; err != nil {
		return nil, err
	}
	roleByUser := make(map[int]int, len(users))
	for _, user := range users {
		roleByUser[user.ID] = user.Role
	}
	var setting models.Settings
	if err := s.db.First(&setting).Error; err != nil {
		return nil, err
	}
	var buckets []models.Buckets
	if err := s.db.Select("id", "type", "config").Where("id IN ?", uniqueInts(bucketIDs)).Find(&buckets).Error; err != nil {
		return nil, err
	}
	domainByBucket := make(map[int]string, len(buckets))
	for _, bucket := range buckets {
		domainByBucket[bucket.Id] = publicurl.BucketDomain(bucket)
	}
	result := make([]ImageRecord, 0, len(images))
	for _, image := range images {
		domain := domainByBucket[image.BucketId]
		image.Url = publicurl.BuildCDNForStorage(setting, image.Storage, publicurl.BuildForStorage(image.Storage, domain, image.Url))
		image.Thumbnail = publicurl.BuildCDNForStorage(setting, image.Storage, publicurl.BuildForStorage(image.Storage, domain, image.Thumbnail))
		itemTags := tagsByImage[image.Id]
		if itemTags == nil {
			itemTags = []models.Tags{}
		}
		result = append(result, ImageRecord{Image: image, Tags: itemTags, UploaderRole: roleByUser[image.UserId]})
	}
	return result, nil
}

func CanAccessImage(user models.User, image models.Image, permission string) bool {
	if image.UserId == models.SuperAdminID && user.ID != models.SuperAdminID {
		return false
	}
	if user.ID == models.SuperAdminID || image.UserId == user.ID {
		return true
	}
	return user.Role == models.RoleAdmin && (permission == "" || models.Can(user, permission))
}

func (s *ImageService) Delete(ctx context.Context, id int) error {
	storageTopology.RLock()
	defer storageTopology.RUnlock()
	var image models.Image
	if err := s.db.First(&image, id).Error; err != nil {
		return err
	}
	var bucket models.Buckets
	if err := s.db.First(&bucket, image.BucketId).Error; err != nil {
		return err
	}
	unlockBucket := lockBucket(bucket.Id, false)
	defer unlockBucket()
	keys, err := imageObjectKeys(image, bucket)
	if err != nil {
		return fmt.Errorf("%w: physical identity is unavailable", ErrStorageDelete)
	}
	unlock := lockObjects(keys)
	defer unlock()
	if err := s.db.First(&image, id).Error; err != nil {
		return err
	}
	target, err := unreferencedImageObjects(s.db, image, bucket, []int{image.Id})
	if err != nil {
		return err
	}
	target, err = distinctDeletionTarget(target, bucket, map[string]bool{})
	if err != nil {
		return err
	}
	if err := deleteStoredImage(ctx, target, bucket); err != nil {
		return fmt.Errorf("%w: %v", ErrStorageDelete, err)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("image_id = ?", image.Id).Delete(&models.ImageToTags{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&image).Error; err != nil {
			return err
		}
		return decrementBucketUsage(tx, image.BucketId, imageStorageBytes(image))
	})
}

func deleteStoredImage(ctx context.Context, image models.Image, bucket models.Buckets) error {
	deletePair := func(deleteOne func(string) error) error {
		if image.Url != "" {
			if err := deleteOne(image.Url); err != nil {
				return err
			}
		}
		if strings.TrimSpace(image.Thumbnail) != "" && image.Thumbnail != image.Url {
			return deleteOne(image.Thumbnail)
		}
		return nil
	}
	switch image.Storage {
	case "default":
		return deletePair(func(value string) error {
			clean := localObjectPath(value)
			err := os.Remove(clean)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		})
	case "s3", "r2":
		var setting models.Settings
		client, err := s3.NewS3Client(setting, bucket)
		if err != nil {
			return err
		}
		bucketName := utilsBuckets.ConvertToS3Bucket(bucket.Config).S3Bucket
		if image.Storage == "r2" {
			bucketName = utilsBuckets.ConvertToR2Bucket(bucket.Config).R2Bucket
		}
		return deletePair(func(value string) error {
			_, err := client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(bucketName), Key: aws.String(strings.TrimPrefix(value, "/"))})
			return err
		})
	case "webdav":
		config := utilsBuckets.ConvertToWebDavBucket(bucket.Config)
		client := webdav.Client(webdav.Config{BaseURL: config.WebdavURL, Username: config.WebdavUser, Password: config.WebdavPass, Timeout: 30 * time.Second})
		return deletePair(func(value string) error { return client.WebDAVDelete(ctx, value) })
	case "ftp":
		config := utilsBuckets.ConvertToFTPBucket(bucket.Config)
		client := ftp.NewFTPUtil(ftp.FTPConfig{Host: config.FTPHost, Port: config.FTPPort, User: config.FTPUser, Password: config.FTPPass, Timeout: 30})
		defer client.Close()
		return deletePair(client.DeleteImage)
	default:
		return fmt.Errorf("unsupported storage type %q", image.Storage)
	}
}
