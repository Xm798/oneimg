package v1

import (
	"errors"
	"net/http"
	"strings"

	"oneimg/backend/models"
	"oneimg/backend/services"
	"oneimg/backend/utils/images"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func (s *Server) updateMyAvatar(c *gin.Context) {
	if !strings.HasPrefix(strings.ToLower(c.GetHeader("Content-Type")), "multipart/form-data") {
		writeProblem(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "头像上传必须使用 multipart/form-data")
		return
	}
	limit := int64(images.AvatarMaxBytes) + 1<<20
	if c.Request.ContentLength > limit {
		writeProblem(c, http.StatusRequestEntityTooLarge, "file_too_large", "头像文件不能超过 10 MiB")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	if err := c.Request.ParseMultipartForm(0); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeProblem(c, http.StatusRequestEntityTooLarge, "file_too_large", "头像文件不能超过 10 MiB")
		} else {
			writeProblem(c, http.StatusBadRequest, "malformed_multipart", "无法解析 multipart 请求")
		}
		return
	}
	defer c.Request.MultipartForm.RemoveAll()
	files := c.Request.MultipartForm.File["avatar"]
	if len(files) != 1 {
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "需要且只能有一个 avatar 文件字段")
		return
	}
	file, err := files[0].Open()
	if err != nil {
		writeProblem(c, http.StatusBadRequest, "malformed_multipart", "无法读取上传的头像")
		return
	}
	defer file.Close()
	// 先取得处理槽位再读入内存，与图片上传共用同一份像素预算。
	ctx, release, err := images.AcquireProcessing(c.Request.Context())
	if err != nil {
		c.Header("Retry-After", "2")
		writeProblem(c, http.StatusServiceUnavailable, "image_processing_busy", "图像处理繁忙，请稍后重试")
		return
	}
	defer release()
	data, err := images.ReadBounded(ctx, file, images.AvatarMaxBytes)
	if err == nil {
		actor, _ := currentUser(c)
		var user models.User
		if user, err = s.services.Avatars.Set(ctx, actor.ID, data); err == nil {
			writeData(c, http.StatusOK, toUserDTO(user), nil)
			return
		}
	}
	switch {
	case errors.Is(err, images.ErrFileTooLarge):
		writeProblem(c, http.StatusRequestEntityTooLarge, "file_too_large", "头像文件不能超过 10 MiB")
	case errors.Is(err, images.ErrDimensionsExceeded):
		writeProblem(c, http.StatusUnprocessableEntity, "image_dimensions_exceeded", "图片尺寸超过服务器处理预算")
	case errors.Is(err, images.ErrUnsupportedFormat):
		writeProblem(c, http.StatusUnprocessableEntity, "unsupported_image_format", "头像只支持 JPEG、PNG、GIF、WebP 静态图片")
	case errors.Is(err, gorm.ErrRecordNotFound):
		writeProblem(c, http.StatusNotFound, "user_not_found", "用户不存在")
	default:
		writeProblem(c, http.StatusInternalServerError, "avatar_update_failed", "保存头像失败")
	}
}

func (s *Server) deleteMyAvatar(c *gin.Context) {
	actor, _ := currentUser(c)
	user, err := s.services.Avatars.Remove(actor.ID)
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		writeProblem(c, http.StatusNotFound, "user_not_found", "用户不存在")
	case err != nil:
		writeProblem(c, http.StatusInternalServerError, "avatar_update_failed", "移除头像失败")
	default:
		writeData(c, http.StatusOK, toUserDTO(user), nil)
	}
}

// getUserAvatar 允许读取自己的头像；读取他人头像需要与用户列表相同的 scope 和权限。
func (s *Server) getUserAvatar(c *gin.Context) {
	id, ok := parsePositiveID(c, "id")
	if !ok {
		return
	}
	principal, _ := principalFrom(c)
	actor := principal.User
	if id != actor.ID {
		if principal.IsToken() && (principal.Token == nil || !principal.Token.HasScope("users:read")) {
			writeProblem(c, http.StatusForbidden, "insufficient_scope", "Token 缺少所需 scope: users:read")
			return
		}
		if !models.Can(*actor, "user:list") {
			writeProblem(c, http.StatusForbidden, "permission_denied", "缺少权限: "+models.PermissionName("user:list"))
			return
		}
	}
	file, version, err := s.services.Avatars.Open(id)
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		writeProblem(c, http.StatusNotFound, "user_not_found", "用户不存在")
		return
	case errors.Is(err, services.ErrAvatarNotFound):
		writeProblem(c, http.StatusNotFound, "avatar_not_found", "该用户没有上传头像")
		return
	case err != nil:
		writeProblem(c, http.StatusInternalServerError, "avatar_read_failed", "读取头像失败")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "avatar_read_failed", "读取头像失败")
		return
	}
	header := c.Writer.Header()
	header.Set("Content-Type", "image/webp")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	// 地址里的版本号和当前文件一致时可以长期缓存；换头像后地址随之变化。
	if c.Query("v") == version {
		header.Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		header.Set("Cache-Control", "private, no-cache")
	}
	http.ServeContent(c.Writer, c.Request, "", info.ModTime(), file)
}
