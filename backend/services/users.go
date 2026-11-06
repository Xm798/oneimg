package services

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"oneimg/backend/database"
	"oneimg/backend/models"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrUsernameInvalid  = errors.New("username must contain between 3 and 50 characters")
	ErrPasswordInvalid  = errors.New("password must contain between 6 and 100 characters")
	ErrUserRoleInvalid  = errors.New("invalid user role")
	ErrProtectedUser    = errors.New("protected user cannot be modified")
	ErrUsernameConflict = errors.New("username already exists")
	ErrBucketNotFound   = errors.New("storage bucket does not exist")

	ErrUserNotManageable  = errors.New("target user holds permissions the actor lacks")
	ErrRoleUpdateRequired = errors.New("creating an admin requires the role update permission")
	ErrActorNotPermitted  = errors.New("actor no longer holds the required permission")
	// ErrConcurrentUpdate means the operation rolled back without effect because
	// another transaction changed the same rows; retrying after a refresh is safe.
	ErrConcurrentUpdate = errors.New("user changed concurrently")
)

type PermissionNotHeldError struct{ Code string }

func (e *PermissionNotHeldError) Error() string {
	return "permission code is not held by the actor: " + e.Code
}

// UserAuditEvent never carries usernames or passwords. Code is the problem
// code of a non-ok outcome.
type UserAuditEvent struct {
	Op                string
	ActorID, TargetID int
	Outcome, Code     string
}

type UserAuditor func(UserAuditEvent)

func logUserAudit(event UserAuditEvent) {
	log.Printf("audit op=%s actor=%d target=%d outcome=%s code=%q", event.Op, event.ActorID, event.TargetID, event.Outcome, event.Code)
}

type UserService struct {
	db      *gorm.DB
	avatars *AvatarService
	audit   UserAuditor
}

type UserListQuery struct {
	Page, PageSize int
	Username       string
	Role           *int
	ID             *int
	Sort, Order    string
}

type UserRecord struct {
	User         models.User
	PasskeyCount int64
}

func NewUserService(db *gorm.DB) *UserService {
	return &UserService{db: db, audit: logUserAudit}
}

func (s *UserService) SetAuditor(auditor UserAuditor) { s.audit = auditor }

func (s *UserService) List(input UserListQuery) ([]UserRecord, int64, error) {
	query := s.db.Model(&models.User{})
	if input.ID != nil {
		query = query.Where("id = ?", *input.ID)
	}
	if strings.TrimSpace(input.Username) != "" {
		query = query.Where(database.SearchLike(s.db, "username_search"), "%"+models.SearchKey(strings.TrimSpace(input.Username))+"%")
	}
	if input.Role != nil {
		query = query.Where("role = ?", *input.Role)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var users []models.User
	orderFields := map[string]string{"id": "id", "username": database.BinaryOrder(s.db, "username"), "created_at": "created_at"}
	if err := query.Order(orderFields[input.Sort] + " " + input.Order).Offset((input.Page - 1) * input.PageSize).Limit(input.PageSize).Find(&users).Error; err != nil {
		return nil, 0, err
	}
	ids := make([]int, 0, len(users))
	for _, user := range users {
		ids = append(ids, user.ID)
	}
	type row struct {
		UserID int
		Count  int64
	}
	var rows []row
	if len(ids) > 0 {
		if err := s.db.Model(&models.PasskeyCredential{}).Select("user_id, COUNT(*) count").Where("user_id IN ?", ids).Group("user_id").Scan(&rows).Error; err != nil {
			return nil, 0, err
		}
	}
	counts := make(map[int]int64, len(rows))
	for _, item := range rows {
		counts[item.UserID] = item.Count
	}
	result := make([]UserRecord, 0, len(users))
	for _, user := range users {
		result = append(result, UserRecord{User: user, PasskeyCount: counts[user.ID]})
	}
	return result, total, nil
}

func validateAccount(username, password string, role int) (string, error) {
	username = strings.TrimSpace(username)
	if utf8.RuneCountInString(username) < 3 || utf8.RuneCountInString(username) > 50 {
		return "", ErrUsernameInvalid
	}
	if len(password) < 6 || len(password) > 100 {
		return "", ErrPasswordInvalid
	}
	if role != models.RoleAdmin && role != models.RoleUser {
		return "", ErrUserRoleInvalid
	}
	return username, nil
}

// lockUsersForUpdate orders by id so every writer of users, including bucket
// deletion, takes PostgreSQL row locks in the same order. SQLite has no row
// locks: a conflicting writer makes the transaction fail with SQLITE_BUSY.
func lockUsersForUpdate(tx *gorm.DB) *gorm.DB {
	query := tx.Order("id")
	if tx.Dialector.Name() == "postgres" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return query
}

func lockUsers(tx *gorm.DB, ids ...int) (map[int]models.User, error) {
	var users []models.User
	if err := lockUsersForUpdate(tx).Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil, err
	}
	result := make(map[int]models.User, len(users))
	for _, user := range users {
		result[user.ID] = user
	}
	return result, nil
}

func loadActor(rows map[int]models.User, actorID int, routeCode string) (models.User, error) {
	actor, ok := rows[actorID]
	if !ok || !models.Can(actor, routeCode) {
		return models.User{}, ErrActorNotPermitted
	}
	return actor, nil
}

// loadManaged re-reads the actor and target inside tx and requires the actor to
// still hold routeCode and to cover the target.
func loadManaged(tx *gorm.DB, actorID, targetID int, routeCode string) (actor, target models.User, err error) {
	rows, err := lockUsers(tx, actorID, targetID)
	if err != nil {
		return models.User{}, models.User{}, err
	}
	if actor, err = loadActor(rows, actorID, routeCode); err != nil {
		return models.User{}, models.User{}, err
	}
	target, ok := rows[targetID]
	if !ok {
		return models.User{}, models.User{}, gorm.ErrRecordNotFound
	}
	if !models.Covers(actor, target) {
		return models.User{}, models.User{}, ErrUserNotManageable
	}
	return actor, target, nil
}

func checkProtected(actorID, id int) error {
	if id == models.SuperAdminID || id == actorID {
		return ErrProtectedUser
	}
	return nil
}

// UserErrorCode returns the problem code of a user-management denial or
// conflict, shared by the audit log and the API response; "" for other errors.
func UserErrorCode(err error) string {
	var notHeld *PermissionNotHeldError
	switch {
	case errors.Is(err, ErrProtectedUser):
		return "protected_user"
	case errors.Is(err, ErrUserNotManageable):
		return "user_not_manageable"
	case errors.As(err, &notHeld):
		return "permission_not_held"
	case errors.Is(err, ErrRoleUpdateRequired), errors.Is(err, ErrActorNotPermitted):
		return "permission_denied"
	case errors.Is(err, ErrConcurrentUpdate):
		return "concurrent_update_conflict"
	}
	return ""
}

// finish classifies err for callers and the audit log. Write conflicts and a
// concurrently rotated auth version become ErrConcurrentUpdate.
func (s *UserService) finish(op string, actorID, targetID int, err error) error {
	if err != nil && (database.IsWriteConflict(err) || errors.Is(err, ErrAuthVersion)) {
		err = fmt.Errorf("%w: %v", ErrConcurrentUpdate, err)
	}
	event := UserAuditEvent{Op: op, ActorID: actorID, TargetID: targetID, Outcome: "ok", Code: UserErrorCode(err)}
	switch {
	case err == nil:
	case event.Code == "concurrent_update_conflict":
		event.Outcome = "conflict"
	case event.Code != "":
		event.Outcome = "denied"
	default:
		event.Outcome = "error"
	}
	if s.audit != nil {
		s.audit(event)
	}
	return err
}

func (s *UserService) Create(actor models.User, username, password string, role int) (models.User, error) {
	user, err := s.create(actor, username, password, role)
	return user, s.finish("user.create", actor.ID, user.ID, err)
}

func (s *UserService) create(actor models.User, username, password string, role int) (models.User, error) {
	username, err := validateAccount(username, password, role)
	if err != nil {
		return models.User{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return models.User{}, err
	}
	user := models.User{Username: username, Password: string(hash), Role: role, Permission: models.Permission{Codes: []string{}, Buckets: []int{}}}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		rows, err := lockUsers(tx, actor.ID)
		if err != nil {
			return err
		}
		fresh, err := loadActor(rows, actor.ID, "user:create")
		if err != nil {
			return err
		}
		if role == models.RoleAdmin && !models.Can(fresh, "user:role:update") {
			return ErrRoleUpdateRequired
		}
		return tx.Create(&user).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return models.User{}, ErrUsernameConflict
	}
	if err != nil {
		return models.User{}, err
	}
	return user, nil
}

func (s *UserService) Update(actor models.User, id int, username *string, role *int) (models.User, error) {
	user, err := s.update(actor, id, username, role)
	return user, s.finish("user.update", actor.ID, id, err)
}

func (s *UserService) update(actor models.User, id int, username *string, role *int) (models.User, error) {
	updates := map[string]any{}
	if username != nil {
		name := strings.TrimSpace(*username)
		if utf8.RuneCountInString(name) < 3 || utf8.RuneCountInString(name) > 50 {
			return models.User{}, ErrUsernameInvalid
		}
		updates["username"] = name
		updates["username_search"] = models.SearchKey(name)
	}
	if role != nil && *role != models.RoleAdmin && *role != models.RoleUser {
		return models.User{}, ErrUserRoleInvalid
	}
	if username == nil && role == nil {
		return models.User{}, errors.New("no user fields supplied")
	}
	if err := checkProtected(actor.ID, id); err != nil {
		return models.User{}, err
	}
	var user models.User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		_, target, err := loadManaged(tx, actor.ID, id, "user:role:update")
		if err != nil {
			return err
		}
		if role != nil {
			updates["role"] = *role
			// A real role change starts from an empty code set: a demoted user
			// keeps no admin codes, and codes stored on a plain user's row were
			// never effective, so promotion grants none of them.
			if *role != target.Role {
				target.Permission.Codes = []string{}
				updates["permission"] = target.Permission
			}
		}
		if err := tx.Model(&models.User{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return err
		}
		return tx.First(&user, id).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return models.User{}, ErrUsernameConflict
	}
	if err != nil {
		return models.User{}, err
	}
	return user, nil
}

func (s *UserService) Delete(actor models.User, id int) error {
	return s.finish("user.delete", actor.ID, id, s.delete(actor, id))
}

func (s *UserService) delete(actor models.User, id int) error {
	if err := checkProtected(actor.ID, id); err != nil {
		return err
	}
	if s.avatars != nil {
		s.avatars.mu.Lock()
		defer s.avatars.mu.Unlock()
	}
	var user models.User
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		if _, user, err = loadManaged(tx, actor.ID, id, "user:delete"); err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", id).Delete(&models.PasskeyCredential{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", id).Delete(&models.PersonalAccessToken{}).Error; err != nil {
			return err
		}
		return tx.Delete(&user).Error
	}); err != nil {
		return err
	}
	// 头像文件只在数据库删除提交后清理，失败只记录日志，不回滚已删除的用户。
	if s.avatars != nil && user.Avatar != nil {
		s.avatars.removeFile(*user.Avatar)
	}
	return nil
}

func (s *UserService) SetPermissions(actor models.User, id int, permission models.Permission) (models.User, error) {
	user, err := s.setPermissions(actor, id, permission)
	return user, s.finish("user.permissions_update", actor.ID, id, err)
}

func (s *UserService) setPermissions(actor models.User, id int, permission models.Permission) (models.User, error) {
	if err := models.ValidatePermissionCodes(permission.Codes); err != nil {
		return models.User{}, err
	}
	permission.Codes = models.FilterPermissionCodes(permission.Codes)
	permission.Buckets = uniqueInts(permission.Buckets)
	if err := checkProtected(actor.ID, id); err != nil {
		return models.User{}, err
	}
	var user models.User
	err := s.db.Transaction(func(tx *gorm.DB) error {
		fresh, target, err := loadManaged(tx, actor.ID, id, "user:permission:update")
		if err != nil {
			return err
		}
		// Plain users hold buckets only; their submitted codes are dropped
		// before the grant check, as no code takes effect on them.
		if target.Role != models.RoleAdmin {
			permission.Codes = []string{}
		}
		if code := models.UnheldCode(permission.Codes, models.EffectiveCodes(fresh)); code != "" {
			return &PermissionNotHeldError{Code: code}
		}
		if len(permission.Buckets) > 0 {
			var count int64
			if err := tx.Model(&models.Buckets{}).Where("id IN ?", permission.Buckets).Count(&count).Error; err != nil {
				return err
			}
			if count != int64(len(permission.Buckets)) {
				return ErrBucketNotFound
			}
		}
		if err := tx.Model(&models.User{}).Where("id = ?", id).Update("permission", permission).Error; err != nil {
			return err
		}
		user = target
		user.Permission = permission
		return nil
	})
	if err != nil {
		return models.User{}, err
	}
	return user, nil
}

// ResetPassword hands the account over to whoever receives the temporary
// password, so it also revokes every credential the previous holder could keep
// using: sessions and PATs through the auth version, and passkeys.
func (s *UserService) ResetPassword(actor models.User, id int) (string, error) {
	plain, err := s.resetPassword(actor, id)
	return plain, s.finish("user.password_reset", actor.ID, id, err)
}

func (s *UserService) resetPassword(actor models.User, id int) (string, error) {
	if err := checkProtected(actor.ID, id); err != nil {
		return "", err
	}
	plain, err := randomAlphaNumeric(16)
	if err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error {
		_, target, err := loadManaged(tx, actor.ID, id, "user:password:reset")
		if err != nil {
			return err
		}
		if err := rotatePassword(tx, &target, string(hash), map[string]any{"password_change_required": true}); err != nil {
			return err
		}
		return tx.Where("user_id = ?", id).Delete(&models.PasskeyCredential{}).Error
	}); err != nil {
		return "", err
	}
	return plain, nil
}

func (s *UserService) RevokePasskeys(actor models.User, id int) error {
	return s.finish("user.passkeys_revoke", actor.ID, id, s.revokePasskeys(actor, id))
}

func (s *UserService) revokePasskeys(actor models.User, id int) error {
	if err := checkProtected(actor.ID, id); err != nil {
		return err
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if _, _, err := loadManaged(tx, actor.ID, id, "user:passkey:reset"); err != nil {
			return err
		}
		return tx.Where("user_id = ?", id).Delete(&models.PasskeyCredential{}).Error
	})
}

func randomAlphaNumeric(length int) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	for i := range bytes {
		bytes[i] = alphabet[int(bytes[i])%len(alphabet)]
	}
	return string(bytes), nil
}
