package v1

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"oneimg/backend/models"
	"oneimg/backend/services"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type managedUserDTO struct {
	userDTO
	PasskeyCount int64 `json:"passkey_count"`
}

// userSortFields must match the UserSort enum in api/openapi.yaml.
var userSortFields = map[string]bool{"id": true, "username": true, "created_at": true}

func (s *Server) listUsers(c *gin.Context) {
	page, ok := parsePositiveQuery(c, "page", 1, 0)
	if !ok {
		return
	}
	pageSize, ok := parsePositiveQuery(c, "page_size", 20, 100)
	if !ok {
		return
	}
	sort := c.DefaultQuery("sort", "id")
	if !userSortFields[sort] {
		writeProblem(c, http.StatusUnprocessableEntity, "invalid_query_parameter", "sort 参数无效")
		return
	}
	order := strings.ToLower(c.DefaultQuery("order", "desc"))
	if order != "asc" && order != "desc" {
		writeProblem(c, http.StatusUnprocessableEntity, "invalid_query_parameter", "order 只能是 asc 或 desc")
		return
	}
	var id *int
	if raw, exists := c.GetQuery("id"); exists {
		value, err := strconv.Atoi(raw)
		if err != nil || value <= 0 {
			writeProblem(c, http.StatusUnprocessableEntity, "invalid_query_parameter", "id 必须是正整数")
			return
		}
		id = &value
	}
	var role *int
	if raw, exists := c.GetQuery("role"); exists {
		value, err := strconv.Atoi(raw)
		if err != nil || (value != models.RoleAdmin && value != models.RoleUser) {
			writeProblem(c, http.StatusUnprocessableEntity, "invalid_query_parameter", "role 参数无效")
			return
		}
		role = &value
	}
	items, total, err := s.services.Users.List(services.UserListQuery{Page: page, PageSize: pageSize, Username: c.Query("username"), Role: role, ID: id, Sort: sort, Order: order})
	if err != nil {
		writeProblem(c, http.StatusInternalServerError, "user_list_failed", "读取用户列表失败")
		return
	}
	result := make([]managedUserDTO, 0, len(items))
	for _, item := range items {
		result = append(result, managedUserDTO{userDTO: toUserDTO(item.User), PasskeyCount: item.PasskeyCount})
	}
	totalPages := (total + int64(pageSize) - 1) / int64(pageSize)
	writeData(c, http.StatusOK, result, &Meta{Pagination: &Pagination{Page: page, PageSize: pageSize, Total: total, TotalPages: totalPages}})
}

func (s *Server) createUser(c *gin.Context) {
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     int    `json:"role"`
	}
	if !bindJSON(c, &input) {
		return
	}
	actor, _ := currentUser(c)
	user, err := s.services.Users.Create(*actor, input.Username, input.Password, input.Role)
	if handleUserError(c, err) {
		return
	}
	c.Header("Location", "/api/v1/users/"+itoa(user.ID))
	writeData(c, http.StatusCreated, toUserDTO(user), nil)
}

func (s *Server) updateUser(c *gin.Context) {
	id, ok := parsePositiveID(c, "id")
	if !ok {
		return
	}
	var input struct {
		Username *string `json:"username"`
		Role     *int    `json:"role"`
	}
	if !bindJSON(c, &input) {
		return
	}
	actor, _ := currentUser(c)
	user, err := s.services.Users.Update(*actor, id, input.Username, input.Role)
	if handleUserError(c, err) {
		return
	}
	writeData(c, http.StatusOK, toUserDTO(user), nil)
}

func (s *Server) deleteUser(c *gin.Context) {
	id, ok := parsePositiveID(c, "id")
	if !ok {
		return
	}
	actor, _ := currentUser(c)
	if err := s.services.Users.Delete(*actor, id); handleUserError(c, err) {
		return
	}
	writeNoContent(c)
}

func (s *Server) updateUserPermissions(c *gin.Context) {
	id, ok := parsePositiveID(c, "id")
	if !ok {
		return
	}
	var input struct {
		Codes     []string `json:"codes"`
		BucketIDs []int    `json:"bucket_ids"`
	}
	if !bindJSON(c, &input) {
		return
	}
	actor, _ := currentUser(c)
	user, err := s.services.Users.SetPermissions(*actor, id, models.Permission{Codes: input.Codes, Buckets: input.BucketIDs})
	if handleUserError(c, err) {
		return
	}
	writeData(c, http.StatusOK, toUserDTO(user), nil)
}

func (s *Server) resetUserPassword(c *gin.Context) {
	id, ok := parsePositiveID(c, "id")
	if !ok {
		return
	}
	actor, _ := currentUser(c)
	password, err := s.services.Users.ResetPassword(*actor, id)
	if handleUserError(c, err) {
		return
	}
	writeData(c, http.StatusOK, gin.H{"new_password": password}, nil)
}

func (s *Server) revokeUserPasskeys(c *gin.Context) {
	id, ok := parsePositiveID(c, "id")
	if !ok {
		return
	}
	actor, _ := currentUser(c)
	if err := s.services.Users.RevokePasskeys(*actor, id); handleUserError(c, err) {
		return
	}
	writeNoContent(c)
}

var userDenialDetails = map[string]string{
	"protected_user":             "不能修改超级管理员或当前登录用户",
	"user_not_manageable":        "只能管理权限少于自己的管理员和普通用户",
	"permission_denied":          "当前账号已无此权限",
	"concurrent_update_conflict": "用户数据刚被其他操作修改，请刷新后重试",
}

func writeUserDenial(c *gin.Context, code string, err error) {
	status, detail := http.StatusForbidden, userDenialDetails[code]
	var notHeld *services.PermissionNotHeldError
	switch {
	case code == "concurrent_update_conflict":
		status = http.StatusConflict
	case errors.As(err, &notHeld):
		detail = "不能授予自己没有的权限: " + models.PermissionName(notHeld.Code)
	case errors.Is(err, services.ErrRoleUpdateRequired):
		detail = "缺少权限: " + models.PermissionName("user:role:update")
	}
	writeProblem(c, status, code, detail)
}

func handleUserError(c *gin.Context, err error) bool {
	if err == nil {
		return false
	}
	if code := services.UserErrorCode(err); code != "" {
		writeUserDenial(c, code, err)
		return true
	}
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		writeProblem(c, http.StatusNotFound, "user_not_found", "用户不存在")
	case errors.Is(err, models.ErrPermissionCodeInvalid):
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", err.Error())
	case errors.Is(err, services.ErrUsernameInvalid):
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "用户名长度必须为 3-50 个字符")
	case errors.Is(err, services.ErrPasswordInvalid):
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "密码长度必须为 6-100 个字符")
	case errors.Is(err, services.ErrUserRoleInvalid):
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "角色必须是 1 或 3")
	case errors.Is(err, services.ErrUsernameConflict):
		writeProblem(c, http.StatusConflict, "username_conflict", "用户名已存在")
	case errors.Is(err, services.ErrBucketNotFound):
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "包含不存在的存储桶")
	default:
		writeProblem(c, http.StatusInternalServerError, "user_operation_failed", "用户操作失败")
	}
	return true
}
