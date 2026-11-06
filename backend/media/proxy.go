package media

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"oneimg/backend/database"
	"oneimg/backend/models"
	"oneimg/backend/utils/ftp"
	"oneimg/backend/utils/s3"
	"oneimg/backend/utils/settings"
	"oneimg/backend/utils/storageobject"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type mediaProxyError struct {
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func mediaError(status int, detail string) mediaProxyError {
	return mediaProxyError{Status: status, Detail: detail}
}

func ImageProxy(c *gin.Context) bool {
	if !AllowReadMethod(c) {
		return true
	}
	// 获取并清理路径
	cleanPath := c.Request.URL.Path
	if cleanPath == "" || cleanPath == "/" {
		// 根路径不应由图片代理处理，由 NoRoute 后续逻辑处理
		return false
	}

	// 获取数据库实例
	db := database.GetDB()
	if db == nil || db.DB == nil {
		return false
	}

	// 查询图片信息
	var imageModel models.Image
	sqlResult := mediaLookup(db.DB, cleanPath).Find(&imageModel)
	if sqlResult.Error != nil || sqlResult.RowsAffected == 0 {
		// 图片不存在，直接返回，交给 NoRoute 后续逻辑处理（如渲染 SPA）
		return false
	}

	// 获取配置信息
	setting, setErr := settings.GetSettings()
	if setErr != nil {
		c.JSON(http.StatusInternalServerError, mediaError(500, fmt.Sprintf("获取系统配置失败: %v", setErr)))
		return true
	}

	// 检查是否开启来源白名单
	if setting.RefererWhiteEnable && setting.RefererWhiteList != "" {
		// 校验Referer白名单
		if !checkReferer(c.Request.Referer(), setting.RefererWhiteList, GetSelfDomain(c)) {
			c.JSON(http.StatusForbidden, mediaError(403, "来源非法"))
			return true
		}
	}

	// 校验图片元信息
	if imageModel.Width == 0 && imageModel.Height == 0 && !strings.HasPrefix(imageModel.MimeType, "video/") {
		log.Printf("图片[%s]元信息不完整（宽高为0），继续代理访问", cleanPath)
	}

	// 获取存储配置
	var bucket models.Buckets
	if err := db.DB.Where("id = ?", imageModel.BucketId).First(&bucket).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusForbidden, mediaError(400, "存储配置不存在"))
			return true
		}
		c.JSON(http.StatusForbidden, mediaError(400, "存储配置查询失败"))
		return true
	}

	var imageUrl string
	mimeType := imageModel.MimeType
	isThumbnail := imageModel.Thumbnail == cleanPath
	// 判断当前访问的是缩略图还是原图
	if isThumbnail {
		imageUrl = imageModel.Thumbnail
		mimeType = "image/webp"
	} else {
		imageUrl = imageModel.Url
	}
	// Preserve protective headers even when the backend returns a cache/error status.
	mediaHeaders(c, mimeType, imageModel.Storage)

	switch imageModel.Storage {
	case "default":
		proxyLocalFile(c, imageUrl, mimeType)

	case "webdav":
		proxyWebDAVFile(c, imageUrl, mimeType, imageModel.FileSize, bucket)
	case "r2":
		// 初始化S3客户端
		s3Client, err := s3.NewS3Client(setting, bucket)
		if err != nil {
			c.JSON(http.StatusInternalServerError, mediaError(500, fmt.Sprintf("R2客户端初始化失败: %v", err)))
			return true
		}
		proxyR2File(c, imageUrl, mimeType, imageModel.FileSize, bucket, s3Client)

	case "s3":
		// 初始化S3客户端
		s3Client, err := s3.NewS3Client(setting, bucket)
		if err != nil {
			c.JSON(http.StatusInternalServerError, mediaError(500, fmt.Sprintf("S3客户端初始化失败: %v", err)))
			return true
		}
		// 代理S3/R2文件
		proxyS3File(c, imageUrl, mimeType, imageModel.FileSize, bucket, s3Client)

	case "ftp":
		proxyFTPFile(c, imageUrl, mimeType, bucket)

	default:
		c.JSON(http.StatusUnprocessableEntity, mediaError(422, fmt.Sprintf("不支持的存储类型: %s", imageModel.Storage)))
	}

	return true
}

// idx_images_url and idx_images_thumbnail serve the two arms of the OR on both dialects.
func mediaLookup(db *gorm.DB, path string) *gorm.DB {
	return db.Where("url = ? OR thumbnail = ?", path, path).Limit(1)
}

// proxyLocalFile 本地文件代理
func proxyLocalFile(c *gin.Context, realPath string, mimeType string) {
	mediaHeaders(c, mimeType, "default")
	if c.Request.Method == http.MethodHead || strings.Contains(c.GetHeader("Range"), ",") {
		c.Request.Header.Del("Range")
	}
	fullPath := localProxyPath(realPath)
	// 去除第一个/和\
	fullPath = strings.TrimPrefix(fullPath, "/")
	fullPath = strings.TrimPrefix(fullPath, "\\")

	fileInfo, err := os.Stat(fullPath)
	if os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, mediaError(404, "文件不存在"))
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, mediaError(500, "文件状态验证失败"))
		return
	}

	if fileInfo.IsDir() {
		c.JSON(http.StatusForbidden, mediaError(403, "文件不可访问"))
		return
	}

	if _, _, _, err := singleRange(requestedRange(c), fileInfo.Size()); err != nil {
		rangeError(c, fileInfo.Size())
		return
	}
	// 流式传输
	c.File(fullPath)
}

func localProxyPath(realPath string) string {
	return storageobject.LocalPath(realPath)
}

func isThumbnailPath(path string) bool {
	cleanPath := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "/")
	return strings.HasPrefix(cleanPath, "thumbnails/") || strings.Contains(cleanPath, "/thumbnails/")
}

// 辅助函数
func cleanFTPPath(path string) string {
	return ftp.ObjectPath(path)
}

// 辅助函数，校验来源
func checkReferer(referer string, whiteList string, selfDomain string) bool {
	if referer == "" {
		return true
	}

	refererDomain, err := extractDomainFromReferer(referer)
	if err != nil {
		return false
	}

	selfDomain = strings.TrimSpace(strings.ToLower(selfDomain))
	if selfDomain != "" {
		if refererDomain == selfDomain || strings.HasSuffix(refererDomain, "."+selfDomain) {
			return true
		}
	}

	whiteListDomains := strings.Split(strings.TrimSpace(whiteList), ",")

	domainSet := make(map[string]bool)
	for _, d := range whiteListDomains {
		domain := strings.TrimSpace(d)
		if domain != "" {
			domainSet[domain] = true
		}
	}

	for allowedDomain := range domainSet {
		if refererDomain == allowedDomain {
			return true
		}
		if strings.HasSuffix(refererDomain, "."+allowedDomain) {
			return true
		}
	}

	return false
}

func extractDomainFromReferer(referer string) (string, error) {
	if !strings.HasPrefix(referer, "http") {
		referer = "http://" + referer
	}

	// 解析URL
	parsedURL, err := url.Parse(referer)
	if err != nil {
		return "", err
	}

	host := parsedURL.Hostname()

	return strings.ToLower(host), nil
}

// 辅助函数，获取本站域名
func GetSelfDomain(c *gin.Context) string {
	host := c.GetHeader("X-Forwarded-Host")
	if host == "" {
		host = c.Request.Host
	}
	domain := strings.Split(host, ":")[0]
	return strings.ToLower(domain)
}
