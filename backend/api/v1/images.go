package v1

import (
	"errors"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"oneimg/backend/models"
	"oneimg/backend/services"
	"oneimg/backend/utils/images"
	"oneimg/backend/utils/storageobject"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type tagDTO struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type imageDTO struct {
	ID               int       `json:"id"`
	URL              string    `json:"url"`
	Thumbnail        string    `json:"thumbnail"`
	FileName         string    `json:"file_name"`
	OriginalFileName string    `json:"original_file_name"`
	OriginalFileSize int64     `json:"original_file_size"`
	FileSize         int64     `json:"file_size"`
	MimeType         string    `json:"mime_type"`
	Width            int       `json:"width"`
	Height           int       `json:"height"`
	Storage          string    `json:"storage"`
	BucketID         int       `json:"bucket_id"`
	UserID           int       `json:"user_id"`
	UploaderRole     int       `json:"uploader_role"`
	Tags             []tagDTO  `json:"tags"`
	CreatedAt        time.Time `json:"created_at"`
}

func toTagDTO(item models.Tags) tagDTO { return tagDTO{ID: item.Id, Name: item.Name} }

func toImageDTO(item services.ImageRecord) imageDTO {
	tags := make([]tagDTO, 0, len(item.Tags))
	for _, tag := range item.Tags {
		tags = append(tags, toTagDTO(tag))
	}
	image := item.Image
	return imageDTO{ID: image.Id, URL: image.Url, Thumbnail: image.Thumbnail, FileName: image.FileName,
		OriginalFileName: image.OriginalFileName, OriginalFileSize: image.OriginalFileSize, FileSize: image.FileSize,
		MimeType: image.MimeType, Width: image.Width, Height: image.Height, Storage: image.Storage,
		BucketID: image.BucketId, UserID: image.UserId, UploaderRole: item.UploaderRole, Tags: tags, CreatedAt: image.CreatedAt.UTC()}
}

// imageSortFields must match the ImageSort enum in api/openapi.yaml.
var imageSortFields = map[string]bool{"created_at": true, "file_size": true, "filename": true, "id": true}

func (s *Server) listImages(c *gin.Context) {
	page, ok := parsePositiveQuery(c, "page", 1, 0)
	if !ok {
		return
	}
	pageSize, ok := parsePositiveQuery(c, "page_size", 20, 100)
	if !ok {
		return
	}
	mediaType := c.Query("media_type")
	if mediaType != "" && mediaType != "image" && mediaType != "video" {
		writeProblem(c, http.StatusUnprocessableEntity, "invalid_query_parameter", "media_type 只能是 image 或 video")
		return
	}
	sort := c.DefaultQuery("sort", "created_at")
	if !imageSortFields[sort] {
		writeProblem(c, http.StatusUnprocessableEntity, "invalid_query_parameter", "sort 参数无效")
		return
	}
	order := strings.ToLower(c.DefaultQuery("order", "desc"))
	if order != "asc" && order != "desc" {
		writeProblem(c, http.StatusUnprocessableEntity, "invalid_query_parameter", "order 只能是 asc 或 desc")
		return
	}
	tagIDs, ok := parseCSVPositiveInts(c, "tag_ids")
	if !ok {
		return
	}
	untagged := false
	if raw, exists := c.GetQuery("untagged"); exists {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			writeProblem(c, http.StatusUnprocessableEntity, "invalid_query_parameter", "untagged 必须是布尔值")
			return
		}
		untagged = value
	}
	var bucketID *int
	if raw, exists := c.GetQuery("bucket_id"); exists {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			writeProblem(c, http.StatusUnprocessableEntity, "invalid_query_parameter", "bucket_id 必须是正整数")
			return
		}
		bucketID = &value
	}
	var uploaderRole *int
	if raw, exists := c.GetQuery("uploader_role"); exists {
		value, err := strconv.Atoi(raw)
		if err != nil || (value != models.RoleAdmin && value != models.RoleUser) {
			writeProblem(c, http.StatusUnprocessableEntity, "invalid_query_parameter", "uploader_role 参数无效")
			return
		}
		principal, _ := principalFrom(c)
		if principal.User.Role != models.RoleAdmin {
			writeProblem(c, http.StatusForbidden, "permission_denied", "只有管理员可以按上传者角色筛选")
			return
		}
		uploaderRole = &value
	}
	principal, _ := principalFrom(c)
	items, total, err := s.services.Images.List(services.ImageListQuery{MediaType: mediaType, Page: page, PageSize: pageSize, Sort: sort, Order: order, Search: c.Query("q"), BucketID: bucketID, TagIDs: tagIDs, Untagged: untagged, UploaderRole: uploaderRole, User: *principal.User})
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "image_list_failed", "读取图片列表失败")
		return
	}
	dtos := make([]imageDTO, 0, len(items))
	for _, item := range items {
		dtos = append(dtos, toImageDTO(item))
	}
	totalPages := (total + int64(pageSize) - 1) / int64(pageSize)
	writeData(c, http.StatusOK, dtos, &Meta{Pagination: &Pagination{Page: page, PageSize: pageSize, Total: total, TotalPages: totalPages}})
}

type imageStorageDTO struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Disabled bool   `json:"disabled"`
}

func (s *Server) listImageStorages(c *gin.Context) {
	principal, _ := principalFrom(c)
	items, err := s.services.Images.Storages(*principal.User)
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "image_storage_list_failed", "读取媒体存储列表失败")
		return
	}
	result := make([]imageStorageDTO, 0, len(items))
	for _, item := range items {
		result = append(result, imageStorageDTO{ID: item.Id, Name: item.Name, Type: item.Type, Disabled: item.Disabled})
	}
	writeData(c, http.StatusOK, result, nil)
}

func (s *Server) getImage(c *gin.Context) {
	id, ok := parsePositiveID(c, "id")
	if !ok {
		return
	}
	item, err := s.services.Images.Get(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeProblem(c, http.StatusNotFound, "image_not_found", "图片不存在")
		return
	}
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "image_read_failed", "读取图片失败")
		return
	}
	principal, _ := principalFrom(c)
	if !services.CanAccessImage(*principal.User, item.Image, "") {
		writeProblem(c, http.StatusForbidden, "image_access_denied", "无权访问该图片")
		return
	}
	writeData(c, http.StatusOK, toImageDTO(item), nil)
}

func (s *Server) deleteImage(c *gin.Context) {
	id, ok := parsePositiveID(c, "id")
	if !ok {
		return
	}
	item, err := s.services.Images.Get(id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeProblem(c, http.StatusNotFound, "image_not_found", "图片不存在")
		return
	}
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "image_read_failed", "读取图片失败")
		return
	}
	principal, _ := principalFrom(c)
	if !services.CanAccessImage(*principal.User, item.Image, "image:delete") {
		writeProblem(c, http.StatusForbidden, "image_delete_denied", "无权删除该图片")
		return
	}
	err = s.services.Images.Delete(c.Request.Context(), id)
	if errors.Is(err, services.ErrStorageDelete) {
		writeProblem(c, http.StatusBadGateway, "storage_delete_failed", "物理文件删除失败，数据库记录已保留")
		return
	}
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "image_delete_failed", "删除图片失败")
		return
	}
	writeNoContent(c)
}

func (s *Server) putImageTag(c *gin.Context)    { s.changeSingleImageTag(c, true) }
func (s *Server) deleteImageTag(c *gin.Context) { s.changeSingleImageTag(c, false) }

func (s *Server) changeSingleImageTag(c *gin.Context, add bool) {
	imageID, ok := parsePositiveID(c, "id")
	if !ok {
		return
	}
	tagID, ok := parsePositiveID(c, "tag_id")
	if !ok {
		return
	}
	item, err := s.services.Images.Get(imageID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeProblem(c, http.StatusNotFound, "image_not_found", "图片不存在")
		return
	}
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "image_read_failed", "读取图片失败")
		return
	}
	principal, _ := principalFrom(c)
	permission := "image:tag:delete"
	if add {
		permission = "image:tag:add"
	}
	if !services.CanAccessImage(*principal.User, item.Image, permission) {
		writeProblem(c, http.StatusForbidden, "image_tag_denied", "无权修改图片标签")
		return
	}
	adds, removes := []int{}, []int{}
	if add {
		adds = []int{tagID}
	} else {
		removes = []int{tagID}
	}
	if err := s.services.Tags.UpdateImageTags([]int{imageID}, adds, removes); errors.Is(err, gorm.ErrRecordNotFound) {
		writeProblem(c, http.StatusNotFound, "tag_not_found", "标签不存在")
		return
	} else if err != nil {
		writeProblem(c, http.StatusInternalServerError, "image_tag_update_failed", "修改图片标签失败")
		return
	}
	writeNoContent(c)
}

func (s *Server) patchImageTags(c *gin.Context) {
	var input struct {
		ImageIDs     []int `json:"image_ids"`
		AddTagIDs    []int `json:"add_tag_ids"`
		RemoveTagIDs []int `json:"remove_tag_ids"`
	}
	if !bindJSON(c, &input) {
		return
	}
	if !allPositive(input.ImageIDs) || !allPositive(input.AddTagIDs) || !allPositive(input.RemoveTagIDs) {
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "图片和标签 ID 必须全部为正整数")
		return
	}
	input.ImageIDs = positiveUnique(input.ImageIDs)
	input.AddTagIDs = positiveUnique(input.AddTagIDs)
	input.RemoveTagIDs = positiveUnique(input.RemoveTagIDs)
	if len(input.ImageIDs) == 0 || (len(input.AddTagIDs) == 0 && len(input.RemoveTagIDs) == 0) {
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "image_ids 及标签变更不能为空")
		return
	}
	images, err := s.services.Images.GetMany(input.ImageIDs)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeProblem(c, http.StatusNotFound, "image_not_found", "一个或多个图片不存在")
		return
	}
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "image_read_failed", "读取图片失败")
		return
	}
	principal, _ := principalFrom(c)
	for _, image := range images {
		if len(input.AddTagIDs) > 0 && !services.CanAccessImage(*principal.User, image, "image:tag:add") {
			writeProblem(c, http.StatusForbidden, "image_tag_denied", "无权为一个或多个图片添加标签")
			return
		}
		if len(input.RemoveTagIDs) > 0 && !services.CanAccessImage(*principal.User, image, "image:tag:delete") {
			writeProblem(c, http.StatusForbidden, "image_tag_denied", "无权修改一个或多个图片的标签")
			return
		}
	}
	if err := s.services.Tags.UpdateImageTags(input.ImageIDs, input.AddTagIDs, input.RemoveTagIDs); errors.Is(err, gorm.ErrRecordNotFound) {
		writeProblem(c, http.StatusNotFound, "resource_not_found", "一个或多个图片或标签不存在")
		return
	} else if err != nil {
		writeProblem(c, http.StatusInternalServerError, "image_tag_update_failed", "修改图片标签失败")
		return
	}
	writeData(c, http.StatusOK, gin.H{"image_ids": input.ImageIDs, "added_tag_ids": input.AddTagIDs, "removed_tag_ids": input.RemoveTagIDs}, nil)
}

func positiveUnique(values []int) []int {
	seen := map[int]struct{}{}
	result := make([]int, 0, len(values))
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

func allPositive(values []int) bool {
	for _, value := range values {
		if value <= 0 {
			return false
		}
	}
	return true
}

type uploadFileDTO struct {
	FileName  string    `json:"file_name"`
	Success   bool      `json:"success"`
	Duplicate bool      `json:"duplicate,omitempty"`
	Image     *imageDTO `json:"image,omitempty"`
	Error     *struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"error,omitempty"`
}

func (s *Server) uploadImages(c *gin.Context) {
	contentType := strings.ToLower(c.GetHeader("Content-Type"))
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		writeProblem(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "图片上传必须使用 multipart/form-data")
		return
	}
	setting, err := s.services.Settings.Get()
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "upload_options_failed", "读取上传限制失败")
		return
	}
	maxSize := int64(setting.MaxFileSize)
	if setting.VideoUploadEnabled && int64(setting.MaxVideoFileSize) > maxSize {
		maxSize = int64(setting.MaxVideoFileSize)
	}
	limit := int64(10)*maxSize + 1<<20
	if c.Request.ContentLength > limit {
		writeProblem(c, http.StatusRequestEntityTooLarge, "upload_batch_too_large", "上传批次超过允许大小")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	// Spool encoded files to disk; no request may retain a full raster buffer before its slot.
	if err := c.Request.ParseMultipartForm(0); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeProblem(c, http.StatusRequestEntityTooLarge, "upload_batch_too_large", "上传批次超过允许大小")
		} else {
			writeProblem(c, http.StatusBadRequest, "malformed_multipart", "无法解析 multipart 请求")
		}
		return
	}
	if c.Request.MultipartForm != nil {
		defer c.Request.MultipartForm.RemoveAll()
	}
	files := c.Request.MultipartForm.File["images"]
	if len(files) == 0 {
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "至少需要一个 images 文件字段")
		return
	}
	if len(files) > 10 {
		writeProblem(c, http.StatusUnprocessableEntity, "too_many_files", "每批最多上传 10 个文件")
		return
	}
	tagIDs := make([]int, 0)
	for _, raw := range c.Request.MultipartForm.Value["tag_ids"] {
		value, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || value <= 0 {
			writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "tag_ids 必须使用重复的正整数表单字段")
			return
		}
		tagIDs = append(tagIDs, value)
	}
	bucketID := 0
	if values := c.Request.MultipartForm.Value["bucket_id"]; len(values) > 0 {
		if len(values) != 1 {
			writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "bucket_id 只能出现一次")
			return
		}
		value, err := strconv.Atoi(strings.TrimSpace(values[0]))
		if err != nil || value <= 0 {
			writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "bucket_id 必须是正整数")
			return
		}
		bucketID = value
	}
	principal, _ := principalFrom(c)
	results, err := s.services.Uploads.UploadBatch(c, *principal.User, files, bucketID, tagIDs)
	if errors.Is(err, services.ErrStorageDisabled) {
		writeProblem(c, http.StatusConflict, "storage_disabled", "该存储已停用，不能上传新文件")
		return
	}
	if errors.Is(err, services.ErrBucketAccess) {
		writeProblem(c, http.StatusForbidden, "storage_access_denied", "无权使用该存储桶")
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "包含不存在的标签或存储桶")
		return
	}
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "upload_failed", "无法处理上传批次")
		return
	}
	items := make([]uploadFileDTO, 0, len(results))
	succeeded := 0
	for _, result := range results {
		dto := uploadFileDTO{FileName: result.FileName}
		if result.Error != nil {
			code, detail, _ := uploadError(result.Error)
			dto.Error = &struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			}{Code: code, Detail: detail}
		} else {
			record, readErr := s.services.Images.Get(result.ImageID)
			if readErr != nil {
				dto.Error = &struct {
					Code   string `json:"code"`
					Detail string `json:"detail"`
				}{Code: "image_read_failed", Detail: "图片已上传但响应组装失败"}
			} else {
				image := toImageDTO(record)
				dto.Image = &image
				dto.Success = true
				dto.Duplicate = result.Result != nil && result.Result.Duplicate
				succeeded++
			}
		}
		items = append(items, dto)
	}
	writeData(c, http.StatusOK, gin.H{"files": items, "summary": gin.H{"total": len(items), "succeeded": succeeded, "failed": len(items) - succeeded}}, nil)
}

func (s *Server) importImage(c *gin.Context) {
	var input struct {
		URL      string `json:"url"`
		BucketID *int   `json:"bucket_id"`
		TagIDs   []int  `json:"tag_ids"`
	}
	if !bindJSON(c, &input) {
		return
	}
	if (input.BucketID != nil && *input.BucketID <= 0) || !allPositive(input.TagIDs) {
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "bucket_id 和 tag_ids 必须为正整数")
		return
	}
	setting, err := s.services.Settings.Get()
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "upload_options_failed", "读取上传限制失败")
		return
	}
	ctx, release, err := images.AcquireProcessing(c.Request.Context())
	if err != nil {
		c.Header("Retry-After", "2")
		writeProblem(c, http.StatusServiceUnavailable, "image_processing_busy", "图像处理繁忙，请稍后重试")
		return
	}
	defer release()
	c.Request = c.Request.WithContext(ctx)
	header, err := services.DownloadRemoteImage(c.Request.Context(), input.URL, int64(setting.MaxFileSize))
	switch {
	case errors.Is(err, services.ErrImportURLInvalid):
		writeProblem(c, http.StatusUnprocessableEntity, "invalid_import_url", "只允许有效的 HTTP/HTTPS 图片 URL")
		return
	case errors.Is(err, services.ErrImportSSRF):
		writeProblem(c, http.StatusForbidden, "remote_address_forbidden", "URL 指向禁止访问的网络地址")
		return
	case errors.Is(err, services.ErrImportTooLarge):
		writeProblem(c, http.StatusUnprocessableEntity, "remote_image_too_large", "远端图片超过单文件大小限制")
		return
	case errors.Is(err, services.ErrImportNotImage):
		writeProblem(c, http.StatusUnprocessableEntity, "remote_content_not_image", "远端内容不是有效图片")
		return
	case err != nil:
		writeProblem(c, http.StatusBadGateway, "remote_image_download_failed", "下载远端图片失败")
		return
	}
	bucketID := 0
	if input.BucketID != nil {
		bucketID = *input.BucketID
	}
	principal, _ := principalFrom(c)
	results, err := s.services.Uploads.UploadBatch(c, *principal.User, []*multipart.FileHeader{header}, bucketID, positiveUnique(input.TagIDs))
	if errors.Is(err, services.ErrStorageDisabled) {
		writeProblem(c, http.StatusConflict, "storage_disabled", "该存储已停用，不能上传新文件")
		return
	}
	if errors.Is(err, services.ErrBucketAccess) {
		writeProblem(c, http.StatusForbidden, "storage_access_denied", "无权使用该存储桶")
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "包含不存在的标签或存储桶")
		return
	}
	if err != nil || len(results) != 1 {
		writeProblem(c, http.StatusInternalServerError, "image_import_failed", "导入图片失败")
		return
	}
	if results[0].Error != nil {
		code, detail, status := uploadError(results[0].Error)
		if status == http.StatusServiceUnavailable {
			c.Header("Retry-After", "2")
		}
		writeProblem(c, status, code, detail)
		return
	}
	item, err := s.services.Images.Get(results[0].ImageID)
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "image_read_failed", "图片已导入但读取失败")
		return
	}
	c.Header("Location", "/api/v1/images/"+itoa(item.Image.Id))
	writeData(c, http.StatusCreated, toImageDTO(item), nil)
}

func uploadError(err error) (string, string, int) {
	switch {
	case errors.Is(err, images.ErrDimensionsExceeded):
		return "image_dimensions_exceeded", "图片尺寸超过服务器处理预算（默认 4000 万像素，单边 16384）", http.StatusUnprocessableEntity
	case errors.Is(err, images.ErrUnsupportedFormat), errors.Is(err, services.ErrImportNotImage):
		return "unsupported_image_format", "不是有效的受支持栅格图片，SVG 不允许上传", http.StatusUnprocessableEntity
	case errors.Is(err, images.ErrProcessingBusy):
		return "image_processing_busy", "图像处理繁忙，请稍后重试", http.StatusServiceUnavailable
	case errors.Is(err, storageobject.ErrConflict):
		return "storage_object_conflict", "对象名称冲突，请重试", http.StatusConflict
	case errors.Is(err, images.ErrFileTooLarge):
		return "file_too_large", "文件超过实际字节大小限制", http.StatusUnprocessableEntity
	default:
		return "file_upload_failed", "文件上传失败，请检查存储配置或稍后重试", http.StatusBadGateway
	}
}
