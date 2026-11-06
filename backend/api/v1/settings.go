package v1

import (
	"errors"
	"net/http"
	"strings"

	"oneimg/backend/models"
	"oneimg/backend/services"

	"github.com/gin-gonic/gin"
)

var settingGroupPermission = map[string]string{
	"upload": "setting:upload", "image": "setting:image", "security": "setting:security", "seo": "setting:seo",
}

type settingsPatchRequest struct {
	VideoUploadEnabled  *bool   `json:"video_upload_enabled"`
	MaxVideoFileSize    *int    `json:"max_video_file_size"`
	OriginalImage       *bool   `json:"original_image"`
	SaveWebP            *bool   `json:"save_webp"`
	Thumbnail           *bool   `json:"thumbnail"`
	RegistrationEnabled *bool   `json:"registration_enabled"`
	SaveOriginalName    *bool   `json:"save_original_name"`
	DefaultStorage      *int    `json:"default_storage"`
	MaxFileSize         *int    `json:"max_file_size"`
	AllowedTypes        *string `json:"allowed_types"`
	MainImageQuality    *int    `json:"main_image_quality"`
	SkipCompressFormats *string `json:"skip_compress_formats"`
	DefaultPath         *string `json:"default_path"`
	FileName            *string `json:"file_name"`
	PublicImageDomain   *string `json:"public_image_domain"` // 已移到 S3/R2 存储配置，只用于返回明确错误
	CDNDomain           *string `json:"cdn_domain"`
	RefererWhiteEnable  *bool   `json:"referer_white_enable"`
	RefererWhiteList    *string `json:"referer_white_list"`
	SEOTitle            *string `json:"seo_title"`
	SEODescription      *string `json:"seo_description"`
	SEOKeywords         *string `json:"seo_keywords"`
	SEOICP              *string `json:"seo_icp"`
	PublicSecurity      *string `json:"public_security"`
	SEOIcon             *string `json:"seo_icon"`
}

func (s *Server) getSettings(c *gin.Context) {
	user, _ := currentUser(c)
	groups, ok := requestedSettingGroups(c, *user)
	if !ok {
		return
	}
	item, err := s.services.Settings.Get()
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "settings_read_failed", "读取设置失败")
		return
	}
	all := settingsMap(item)
	result := map[string]any{}
	keys := map[string][]string{
		"upload":   {"default_storage", "default_path", "file_name", "max_file_size", "video_upload_enabled", "max_video_file_size", "allowed_types", "save_original_name", "cdn_domain"},
		"image":    {"original_image", "save_webp", "thumbnail", "main_image_quality", "skip_compress_formats"},
		"security": {"registration_enabled", "referer_white_enable", "referer_white_list"},
		"seo":      {"seo_title", "seo_description", "seo_keywords", "seo_icp", "public_security", "seo_icon"},
	}
	for _, group := range groups {
		for _, key := range keys[group] {
			result[key] = all[key]
		}
	}
	result["groups"] = groups
	writeData(c, http.StatusOK, result, nil)
}

func (s *Server) patchSettings(c *gin.Context) {
	var input settingsPatchRequest
	if !bindJSON(c, &input) {
		return
	}
	if input.PublicImageDomain != nil {
		message := "图片直链域名已改为在各 S3/R2 存储中设置，请使用存储接口的 config.public_domain"
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", message,
			FieldProblem{Field: "public_image_domain", Code: "moved", Message: message})
		return
	}
	user, _ := currentUser(c)
	groups := input.groups()
	if len(groups) == 0 {
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "至少需要更新一个设置字段")
		return
	}
	for _, group := range groups {
		if !canAccessSettingGroup(*user, group) {
			writeProblem(c, http.StatusForbidden, "permission_denied", "缺少设置分组权限: "+group)
			return
		}
	}
	item, err := s.services.Settings.Update(services.SettingsPatch{
		OriginalImage: input.OriginalImage, SaveWebP: input.SaveWebP, Thumbnail: input.Thumbnail,
		RegistrationEnabled: input.RegistrationEnabled, SaveOriginalName: input.SaveOriginalName,
		VideoUploadEnabled: input.VideoUploadEnabled, MaxVideoFileSize: input.MaxVideoFileSize,
		DefaultStorage: input.DefaultStorage, MaxFileSize: input.MaxFileSize, AllowedTypes: input.AllowedTypes,
		MainImageQuality: input.MainImageQuality, SkipCompressFormats: input.SkipCompressFormats,
		DefaultPath: input.DefaultPath, FileName: input.FileName, CDNDomain: input.CDNDomain,
		RefererWhiteEnable: input.RefererWhiteEnable, RefererWhiteList: input.RefererWhiteList,
		SEOTitle: input.SEOTitle, SEODescription: input.SEODescription, SEOKeywords: input.SEOKeywords,
		SEOICP: input.SEOICP, PublicSecurity: input.PublicSecurity, SEOIcon: input.SEOIcon,
	})
	if errors.Is(err, services.ErrStorageDisabled) {
		message := "已停用的存储不能设为系统默认存储"
		writeProblem(c, http.StatusUnprocessableEntity, "storage_disabled", message,
			FieldProblem{Field: "default_storage", Code: "storage_disabled", Message: message})
		return
	}
	var fieldErr *services.SettingsFieldError
	if errors.As(err, &fieldErr) {
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", fieldErr.Message+"，事务已回滚",
			FieldProblem{Field: fieldErr.Field, Code: "invalid", Message: fieldErr.Message})
		return
	}
	if errors.Is(err, services.ErrSettingsValidation) {
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "设置值无效，事务已回滚")
		return
	}
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "settings_update_failed", "更新设置失败，事务已回滚")
		return
	}
	writeData(c, http.StatusOK, settingsMap(item), nil)
}

func requestedSettingGroups(c *gin.Context, user models.User) ([]string, bool) {
	raw := strings.TrimSpace(c.Query("groups"))
	requested := []string{"upload", "image", "security", "seo"}
	if raw != "" {
		requested = strings.Split(raw, ",")
	}
	result := make([]string, 0, len(requested))
	seen := map[string]bool{}
	for _, group := range requested {
		group = strings.TrimSpace(group)
		if _, exists := settingGroupPermission[group]; !exists {
			writeProblem(c, http.StatusUnprocessableEntity, "invalid_query_parameter", "未知设置分组: "+group)
			return nil, false
		}
		if !canAccessSettingGroup(user, group) {
			continue
		}
		if !seen[group] {
			seen[group] = true
			result = append(result, group)
		}
	}
	if len(result) == 0 {
		writeProblem(c, http.StatusForbidden, "permission_denied", "无权读取所请求的设置分组")
		return nil, false
	}
	return result, true
}

func canAccessSettingGroup(user models.User, group string) bool {
	if user.ID == models.SuperAdminID {
		return true
	}
	return models.Can(user, settingGroupPermission[group])
}

func (r settingsPatchRequest) groups() []string {
	seen := map[string]bool{}
	result := []string{}
	add := func(group string) {
		if !seen[group] {
			seen[group] = true
			result = append(result, group)
		}
	}
	if r.VideoUploadEnabled != nil || r.MaxVideoFileSize != nil || r.DefaultStorage != nil || r.MaxFileSize != nil || r.AllowedTypes != nil || r.SaveOriginalName != nil || r.DefaultPath != nil || r.FileName != nil || r.CDNDomain != nil {
		add("upload")
	}
	if r.OriginalImage != nil || r.SaveWebP != nil || r.Thumbnail != nil || r.MainImageQuality != nil || r.SkipCompressFormats != nil {
		add("image")
	}
	if r.RegistrationEnabled != nil || r.RefererWhiteEnable != nil || r.RefererWhiteList != nil {
		add("security")
	}
	if r.SEOTitle != nil || r.SEODescription != nil || r.SEOKeywords != nil || r.SEOICP != nil || r.PublicSecurity != nil || r.SEOIcon != nil {
		add("seo")
	}
	return result
}

func settingsMap(item models.Settings) map[string]any {
	return map[string]any{
		"video_upload_enabled": item.VideoUploadEnabled, "max_video_file_size": item.MaxVideoFileSize,
		"original_image": item.OriginalImage, "save_webp": item.SaveWebp, "thumbnail": item.Thumbnail,
		"registration_enabled": item.StartRegister, "save_original_name": item.SaveOriginalName,
		"default_storage": item.DefaultStorage, "max_file_size": item.MaxFileSize, "allowed_types": item.AllowedTypes,
		"main_image_quality": item.MainImageQuality, "skip_compress_formats": item.SkipCompressFormat,
		"default_path": item.DefaultPath, "file_name": item.FileName, "cdn_domain": item.CDNDomain,
		"referer_white_enable": item.RefererWhiteEnable, "referer_white_list": item.RefererWhiteList,
		"seo_title": item.SEOTitle, "seo_description": item.SEODescription, "seo_keywords": item.SEOKeywords,
		"seo_icp": item.SEOICP, "public_security": item.PublicSecurity, "seo_icon": item.SEOicon,
	}
}
