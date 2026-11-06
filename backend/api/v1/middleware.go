package v1

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"oneimg/backend/models"
	"oneimg/backend/services"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	contextRequestID = "api_request_id"
	contextPrincipal = "api_principal"
	csrfCookieName   = "oneimg-csrf"
	csrfHeaderName   = "X-OneImg-CSRF"
)

type Principal struct {
	User       *models.User
	Token      *models.PersonalAccessToken
	AuthMethod string
}

func (p *Principal) IsSession() bool { return p != nil && p.AuthMethod == "session" }
func (p *Principal) IsToken() bool   { return p != nil && p.AuthMethod == "bearer" }

func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if id == "" || len(id) > 128 {
			id = uuid.NewString()
		}
		c.Set(contextRequestID, id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

func (s *Server) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		authorization := strings.TrimSpace(c.GetHeader("Authorization"))
		if authorization != "" {
			parts := strings.SplitN(authorization, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
				c.Header("WWW-Authenticate", "Bearer")
				writeProblem(c, http.StatusUnauthorized, "invalid_bearer_token", "Authorization 头必须使用 Bearer Token")
				c.Abort()
				return
			}
			token, user, err := s.services.Tokens.Authenticate(strings.TrimSpace(parts[1]))
			if err != nil {
				c.Header("WWW-Authenticate", "Bearer")
				writeProblem(c, http.StatusUnauthorized, "invalid_bearer_token", "Token 无效、已过期或已撤销")
				c.Abort()
				return
			}
			principal := &Principal{User: user, Token: token, AuthMethod: "bearer"}
			setPrincipal(c, principal)
			c.Next()
			return
		}

		session := sessions.Default(c)
		loggedIn, _ := session.Get("logged_in").(bool)
		userID, ok := session.Get("user_id").(int)
		if !loggedIn || !ok || userID <= 0 {
			writeProblem(c, http.StatusUnauthorized, "authentication_required", "请先登录")
			c.Abort()
			return
		}
		user, err := s.services.Accounts.GetActive(userID)
		version, versionOK := session.Get("auth_version").(int64)
		if err != nil || !versionOK || version <= 0 || user.AuthVersion != version {
			session.Clear()
			_ = session.Save()
			writeProblem(c, http.StatusUnauthorized, "invalid_session", "登录会话已失效")
			c.Abort()
			return
		}
		setPrincipal(c, &Principal{User: &user, AuthMethod: "session"})
		if user.PasswordChangeRequired && !((c.Request.URL.Path == "/api/v1/me" && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodPatch)) || (c.Request.URL.Path == "/api/v1/auth/logout" && c.Request.Method == http.MethodPost)) {
			writeProblem(c, http.StatusForbidden, "password_change_required", "请先完成密码修改及本地校验")
			c.Abort()
			return
		}
		c.Next()
	}
}

func setPrincipal(c *gin.Context, principal *Principal) {
	c.Set(contextPrincipal, principal)
	c.Set("current_user", principal.User)
	c.Set("user_id", principal.User.ID)
	c.Set("user_role", principal.User.Role)
	c.Set("username", principal.User.Username)
}

func principalFrom(c *gin.Context) (*Principal, bool) {
	value, ok := c.Get(contextPrincipal)
	if !ok {
		return nil, false
	}
	principal, ok := value.(*Principal)
	return principal, ok && principal != nil && principal.User != nil
}

func requireSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok || !principal.IsSession() {
			writeProblem(c, http.StatusForbidden, "session_required", "此操作必须使用浏览器登录会话")
			c.Abort()
			return
		}
		c.Next()
	}
}

func requireScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			writeProblem(c, http.StatusUnauthorized, "authentication_required", "请先登录")
			c.Abort()
			return
		}
		if principal.IsToken() && (principal.Token == nil || !principal.Token.HasScope(scope)) {
			writeProblem(c, http.StatusForbidden, "insufficient_scope", "Token 缺少所需 scope: "+scope)
			c.Abort()
			return
		}
		c.Next()
	}
}

func requirePermission(code string) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			writeProblem(c, http.StatusUnauthorized, "authentication_required", "请先登录")
			c.Abort()
			return
		}
		user := principal.User
		if !models.Can(*user, code) {
			writeProblem(c, http.StatusForbidden, "permission_denied", "缺少权限: "+models.PermissionName(code))
			c.Abort()
			return
		}
		c.Next()
	}
}

func requireAnyPermission(codes ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			writeProblem(c, http.StatusUnauthorized, "authentication_required", "请先登录")
			c.Abort()
			return
		}
		for _, code := range codes {
			if models.Can(*principal.User, code) {
				c.Next()
				return
			}
		}
		writeProblem(c, http.StatusForbidden, "permission_denied", "没有执行此操作的权限")
		c.Abort()
	}
}

func (s *Server) csrfProtection() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok || !principal.IsSession() || isSafeMethod(c.Request.Method) {
			c.Next()
			return
		}
		cookie, err := c.Cookie(csrfCookieName)
		header := c.GetHeader(csrfHeaderName)
		if err != nil || header == "" || subtle.ConstantTimeCompare([]byte(cookie), []byte(header)) != 1 || !s.validateCSRF(cookie) {
			writeProblem(c, http.StatusForbidden, "csrf_validation_failed", "CSRF Token 无效或已过期")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Server) issueCSRFCookie(c *gin.Context) error {
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	payload := base64.RawURLEncoding.EncodeToString(random)
	mac := hmac.New(sha256.New, []byte(s.services.Config.SessionSecret))
	_, _ = mac.Write([]byte(payload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	value := payload + "." + signature
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(csrfCookieName, value, 24*60*60, "/", "", strings.HasPrefix(strings.ToLower(s.services.Config.AppURL), "https://"), false)
	return nil
}

func (s *Server) validateCSRF(value string) bool {
	payload, signature, ok := strings.Cut(value, ".")
	if !ok || payload == "" || signature == "" {
		return false
	}
	provided, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.services.Config.SessionSecret))
	_, _ = mac.Write([]byte(payload))
	return hmac.Equal(provided, mac.Sum(nil))
}

func (s *Server) OriginProtection() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.HasPrefix(c.Request.URL.Path, "/api") {
			c.Next()
			return
		}
		origin := strings.TrimSpace(c.GetHeader("Origin"))
		if origin == "" {
			c.Next()
			return
		}
		if !sameOrigin(origin, s.services.Config.AppURL) && !(isLocalhostOrigin(origin) && isLocalhostOrigin(s.services.Config.AppURL)) {
			writeProblem(c, http.StatusForbidden, "origin_not_allowed", "请求来源不受信任")
			c.Abort()
			return
		}
		c.Next()
	}
}

func sameOrigin(left, right string) bool {
	a, errA := url.Parse(strings.TrimSpace(left))
	b, errB := url.Parse(strings.TrimSpace(right))
	return errA == nil && errB == nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func isLocalhostOrigin(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}

func isSafeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func currentUser(c *gin.Context) (*models.User, error) {
	principal, ok := principalFrom(c)
	if !ok {
		return nil, errors.New("principal missing")
	}
	return principal.User, nil
}

func tokenServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, services.ErrAuthVersion):
		writeProblem(c, http.StatusUnauthorized, "invalid_session", "登录会话已失效，请重新登录")
	case errors.Is(err, services.ErrPasswordChangeRequired):
		writeProblem(c, http.StatusForbidden, "password_change_required", "请先修改密码")
	case errors.Is(err, services.ErrInvalidTokenName):
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "Token 名称长度必须为 1-50 个字符")
	case errors.Is(err, services.ErrTokenScopesRequired), errors.Is(err, services.ErrInvalidTokenScope):
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", err.Error())
	case errors.Is(err, services.ErrInvalidTokenExpiration):
		writeProblem(c, http.StatusUnprocessableEntity, "validation_error", "过期时间只能是 30、90、365 天或永不过期")
	default:
		writeProblem(c, http.StatusInternalServerError, "internal_error", "Token 操作失败")
	}
}
