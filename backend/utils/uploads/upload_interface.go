package uploads

import (
	"fmt"

	"oneimg/backend/interfaces"
	"oneimg/backend/models"
)

const (
	MaxUploadFiles     = 10
	DefaultStorageType = "default"
)

// GetStorageUploader selects an existing storage implementation without
// coupling storage code to an HTTP response format.
func GetStorageUploader(bucket *models.Buckets) (interfaces.StorageUploader, error) {
	switch bucket.Type {
	case "default", "r2", "s3", "webdav", "ftp":
		return &createdUploader{}, nil
	default:
		return nil, fmt.Errorf("不支持的存储类型：%s", bucket.Type)
	}
}
