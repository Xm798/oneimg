package models

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// 用户模型
type User struct {
	ID                     int        `json:"id" gorm:"type:integer;primaryKey;autoIncrement"`
	Role                   int        `json:"role" gorm:"default:1"`
	Username               string     `json:"username" gorm:"unique;not null"`
	UsernameSearch         string     `json:"-" gorm:"type:text"`
	Password               string     `json:"-" gorm:"not null"`
	AuthVersion            int64      `json:"-" gorm:"not null;default:1"`
	PasswordChangeRequired bool       `json:"password_change_required" gorm:"not null;default:false"`
	BootstrapCodeHash      *string    `json:"-" gorm:"type:text"`
	Avatar                 *string    `json:"-" gorm:"type:text"`
	Permission             Permission `json:"permission" gorm:"type:jsonb"`
	CreatedAt              time.Time  `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt              time.Time  `json:"updated_at" gorm:"autoUpdateTime"`
}

const (
	SuperAdminID = 1
	RoleAdmin    = 1
	RoleUser     = 3
)

var permissionNames = map[string]string{
	"user:list":              "查看用户",
	"user:create":            "添加用户",
	"user:delete":            "删除用户",
	"user:role:update":       "修改角色",
	"user:permission:update": "编辑权限",
	"user:password:reset":    "重置密码",
	"user:passkey:reset":     "撤销 Passkey",
	"tag:create":             "新增标签",
	"tag:update":             "编辑标签",
	"tag:delete":             "删除标签",
	"storage:create":         "新增存储",
	"storage:update":         "编辑存储",
	"storage:delete":         "删除存储",
	"image:delete":           "删除图片",
	"image:tag:add":          "添加图片标签",
	"image:tag:delete":       "删除图片标签",
	"setting:upload":         "存储与上传",
	"setting:image":          "图片处理",
	"setting:security":       "访问安全",
	"setting:seo":            "站点信息",
}

var allPermissionCodes = []string{
	"user:list", "user:create", "user:delete", "user:role:update", "user:permission:update", "user:password:reset", "user:passkey:reset",
	"tag:create", "tag:update", "tag:delete",
	"storage:create", "storage:update", "storage:delete",
	"image:delete", "image:tag:add", "image:tag:delete",
	"setting:upload", "setting:image", "setting:security", "setting:seo",
}

// Permission 保存后台功能权限及用户可使用的存储桶。
type Permission struct {
	Codes   []string `json:"codes" gorm:"default:[]"`
	Buckets []int    `json:"buckets" gorm:"default:[]"`
}

func (p Permission) Value() (driver.Value, error) {
	return json.Marshal(p)
}

func (p *Permission) Scan(src any) error {
	if src == nil {
		p.Codes = []string{}
		p.Buckets = []int{}
		return nil
	}
	var data []byte
	switch v := src.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		return errors.New("invalid json source for Permission")
	}
	if len(data) == 0 || string(data) == "null" || string(data) == "[]" {
		p.Codes = []string{}
		p.Buckets = []int{}
		return nil
	}
	if err := json.Unmarshal(data, p); err != nil {
		return err
	}
	if p.Codes == nil {
		p.Codes = []string{}
	}
	if p.Buckets == nil {
		p.Buckets = []int{}
	}
	return nil
}

func AllPermissionCodes() []string {
	return append([]string(nil), allPermissionCodes...)
}

func PermissionName(code string) string {
	if name, ok := permissionNames[code]; ok {
		return name
	}
	return "未知权限"
}

var ErrPermissionCodeInvalid = errors.New("非法的权限码")

func ValidatePermissionCodes(codes []string) error {
	for _, code := range codes {
		if _, ok := permissionNames[code]; !ok {
			return fmt.Errorf("%w: %s", ErrPermissionCodeInvalid, code)
		}
	}
	return nil
}

func FilterPermissionCodes(codes []string) []string {
	seen := make(map[string]struct{}, len(codes))
	result := make([]string, 0, len(codes))
	for _, allowed := range allPermissionCodes {
		for _, code := range codes {
			if code != allowed {
				continue
			}
			if _, ok := seen[code]; !ok {
				seen[code] = struct{}{}
				result = append(result, code)
			}
		}
	}
	return result
}

// EffectiveCodes is the single source of a user's permission codes, in
// canonical order. Legacy "*" grants every code and unknown stored codes grant
// nothing; plain users hold no codes whatever their stored permission says.
func EffectiveCodes(u User) []string {
	if u.ID == SuperAdminID {
		return AllPermissionCodes()
	}
	if u.Role != RoleAdmin {
		return []string{}
	}
	for _, code := range u.Permission.Codes {
		if code == "*" {
			return AllPermissionCodes()
		}
	}
	return FilterPermissionCodes(u.Permission.Codes)
}

// Can matches EffectiveCodes without allocating, as request middleware calls
// it on every authorised request.
func Can(u User, code string) bool {
	if u.ID == SuperAdminID {
		return true
	}
	if u.Role != RoleAdmin {
		return false
	}
	if _, known := permissionNames[code]; !known {
		return false
	}
	for _, current := range u.Permission.Codes {
		if current == code || current == "*" {
			return true
		}
	}
	return false
}

// Covers reports whether actor may manage target. An admin target must hold a
// strict subset of the actor's codes: equal peers cannot manage each other, so
// demoting or trimming a peer is never available as a first step towards
// resetting it.
func Covers(actor, target User) bool {
	if target.ID == SuperAdminID {
		return false
	}
	if actor.ID == SuperAdminID || target.Role == RoleUser {
		return true
	}
	if target.Role != RoleAdmin {
		return false
	}
	// EffectiveCodes deduplicates, so a subset with fewer codes is strict.
	targetCodes := EffectiveCodes(target)
	actorCodes := EffectiveCodes(actor)
	return UnheldCode(targetCodes, actorCodes) == "" && len(targetCodes) < len(actorCodes)
}

// UnheldCode returns the first of codes missing from held, or "" when held
// contains them all.
func UnheldCode(codes, held []string) string {
	for _, code := range codes {
		if !slices.Contains(held, code) {
			return code
		}
	}
	return ""
}

func IntSliceContains(arr []int, target int) bool {
	for _, v := range arr {
		if v == target {
			return true
		}
	}
	return false
}
