package uploads

import (
	"bytes"
	"github.com/gin-gonic/gin"
	"log"
	"oneimg/backend/database"
	"oneimg/backend/interfaces"
	"oneimg/backend/models"
	"os"
	"path/filepath"
	"strings"
)

// ensureUploadDir 确保上传目录存在
func ensureUploadDir(uploadPath string) error {
	if _, err := os.Stat(uploadPath); os.IsNotExist(err) {
		return os.MkdirAll(uploadPath, 0755)
	}
	return nil
}

// saveFile 保存文件到磁盘
func saveFile(path string, data []byte) error { return saveStream(path, bytes.NewReader(data)) }

func buildThumbnailPath(subDir, uniqueFileName string) string {
	relDir := strings.Trim(strings.TrimPrefix(strings.Trim(subDir, "/"), "uploads/"), "/")
	fileName := thumbnailFileName(uniqueFileName)
	if relDir == "" || relDir == "uploads" {
		return PathJoin("thumbnails", fileName)
	}
	return PathJoin("thumbnails", relDir, fileName)
}

func thumbnailFileName(fileName string) string {
	ext := filepath.Ext(fileName)
	if ext == "" {
		return fileName + ".webp"
	}
	return strings.TrimSuffix(fileName, ext) + ".webp"
}

func localThumbnailFilePath(thumbnailPath string) string {
	return filepath.Join(".", "data", strings.TrimPrefix(thumbnailPath, "/"))
}

func findDuplicateUploadResult(c *gin.Context, bucket *models.Buckets, contentHash string) *interfaces.ImageUploadResult {
	if strings.TrimSpace(contentHash) == "" || bucket == nil {
		return nil
	}
	db := database.GetDB()
	if db == nil {
		return nil
	}

	var imageModel models.Image
	result := db.DB.
		Where("content_hash = ? AND storage = ? AND bucket_id = ? AND user_id = ?", contentHash, bucket.Type, bucket.Id, c.GetInt("user_id")).
		Order("id ASC").
		Limit(1).
		Find(&imageModel)
	if result.Error != nil {
		log.Printf("检查重复图片失败 (%s)", database.ErrorCode(result.Error))
		return nil
	}
	if result.RowsAffected == 0 {
		return nil
	}

	return &interfaces.ImageUploadResult{
		Success:          true,
		Message:          "图片已存在",
		ID:               imageModel.Id,
		URL:              imageModel.Url,
		ThumbnailURL:     imageModel.Thumbnail,
		Storage:          imageModel.Storage,
		FileName:         imageModel.FileName,
		OriginalFileName: imageModel.OriginalFileName,
		OriginalFileSize: imageModel.OriginalFileSize,
		FileSize:         imageModel.FileSize,
		MimeType:         imageModel.MimeType,
		Width:            imageModel.Width,
		Height:           imageModel.Height,
		ContentHash:      imageModel.ContentHash,
		Duplicate:        true,
		CreatedAt:        imageModel.CreatedAt.Format("2006-01-02 15:04:05"),
	}
}

// objectDir 生成对象所在目录：\ 按 / 处理，去掉首尾 / 以及 "."、".." 目录段。
// 本地存储始终写在 uploads 目录下，保证文件位于持久化挂载内，本地 CDN 域名也能生效；
// 远端存储保持配置的前缀不变。
func objectDir(dir, storageType string) string {
	parts := strings.Split(strings.ReplaceAll(dir, `\`, "/"), "/")
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "." || part == ".." {
			continue
		}
		kept = append(kept, part)
	}
	dir = strings.Trim(strings.Join(kept, "/"), "/")
	if storageType == "default" && dir != "uploads" && !strings.HasPrefix(dir, "uploads/") {
		return strings.TrimSuffix(PathJoin("uploads", dir), "/")
	}
	return dir
}

func ensureLeadingSlash(path string) string {
	if path == "" || strings.HasPrefix(path, "/") {
		return path
	}
	return "/" + path
}

// 辅助函数
func PathJoin(parts ...string) string {
	return strings.Join(parts, "/")
}
