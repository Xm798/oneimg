package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"oneimg/backend/app"
	"oneimg/backend/config"
	"oneimg/backend/models"
	"oneimg/backend/services"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

var hierarchyUserCodes = []string{"user:list", "user:create", "user:delete", "user:role:update", "user:permission:update", "user:password:reset", "user:passkey:reset"}

func codes(base []string, extra ...string) []string {
	return append(append([]string(nil), base...), extra...)
}

type hierarchyEnv struct {
	t      *testing.T
	router *gin.Engine
	system *app.System
	users  map[string]models.User
	tokens map[string]string
	audit  []services.UserAuditEvent
}

// newHierarchyEnv seeds the fixtures named in the user-management hierarchy
// rule: A is the actor; H holds more, P the same, I an incomparable set, and
// L, L?, Z less than A.
func newHierarchyEnv(t *testing.T) *hierarchyEnv {
	t.Helper()
	cfg := &config.Config{SqlitePath: filepath.Join(t.TempDir(), "oneimg.db"), SessionSecret: "test-session-secret", ConfigSecret: "test-config-secret-with-enough-bytes"}
	router, system := setupTestRouter(t, cfg)
	env := &hierarchyEnv{t: t, router: router, system: system, users: map[string]models.User{}, tokens: map[string]string{}}
	system.Services.Users.SetAuditor(func(event services.UserAuditEvent) { env.audit = append(env.audit, event) })
	hash, err := bcrypt.GenerateFromPassword([]byte("fixture-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	a := codes(hierarchyUserCodes, "tag:create")
	for _, fixture := range []struct {
		name  string
		role  int
		codes []string
	}{
		{"S", models.RoleAdmin, []string{}},
		{"A", models.RoleAdmin, a},
		{"H", models.RoleAdmin, codes(a, "storage:delete")},
		{"P", models.RoleAdmin, a},
		{"Pstar", models.RoleAdmin, []string{"*"}},
		{"ALL", models.RoleAdmin, models.AllPermissionCodes()},
		{"I", models.RoleAdmin, codes(hierarchyUserCodes, "storage:delete")},
		{"L", models.RoleAdmin, []string{"user:list"}},
		{"Lunknown", models.RoleAdmin, []string{"user:list", "setting:api"}},
		{"Z", models.RoleAdmin, []string{}},
		{"U", models.RoleUser, []string{}},
		{"R", models.RoleUser, []string{"storage:delete"}},
		{"C", models.RoleAdmin, []string{"user:create"}},
		{"X", models.RoleAdmin, []string{"user:create", "user:role:update"}},
		{"W", models.RoleAdmin, []string{"*"}},
	} {
		user := models.User{Username: "fixture-" + strings.ToLower(fixture.name), Password: string(hash), Role: fixture.role, Permission: models.Permission{Codes: fixture.codes, Buckets: []int{}}}
		if err := system.Database.DB.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		env.users[fixture.name] = user
	}
	if env.users["S"].ID != models.SuperAdminID {
		t.Fatalf("super admin fixture id = %d", env.users["S"].ID)
	}
	return env
}

func (e *hierarchyEnv) token(actor string, scopes ...string) string {
	e.t.Helper()
	if len(scopes) == 0 {
		scopes = []string{"users:read", "users:write"}
	}
	key := actor + "|" + strings.Join(scopes, ",")
	if plain, ok := e.tokens[key]; ok {
		return plain
	}
	user := e.fresh(actor)
	created, err := e.system.Services.Tokens.Create(user.ID, services.CreateTokenInput{AuthVersion: user.AuthVersion, Name: key, Scopes: scopes})
	if err != nil {
		e.t.Fatal(err)
	}
	e.tokens[key] = created.Plain
	return created.Plain
}

func (e *hierarchyEnv) fresh(name string) models.User {
	e.t.Helper()
	var user models.User
	if err := e.system.Database.DB.First(&user, e.users[name].ID).Error; err != nil {
		e.t.Fatal(err)
	}
	return user
}

func (e *hierarchyEnv) id(name string) string { return strconv.Itoa(e.users[name].ID) }

type hierarchyResponse struct {
	status int
	code   string
	body   string
}

func (e *hierarchyEnv) do(token, method, path, body string) hierarchyResponse {
	e.t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	e.router.ServeHTTP(recorder, request)
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &problem)
	return hierarchyResponse{status: recorder.Code, code: problem.Code, body: recorder.Body.String()}
}

// op runs a named user-management operation by actor against target.
func (e *hierarchyEnv) op(actor, op, target string) hierarchyResponse {
	e.t.Helper()
	token := e.token(actor)
	base := "/api/v1/users/" + e.id(target)
	switch op {
	case "rename":
		return e.do(token, http.MethodPatch, base, `{"username":"renamed-`+strings.ToLower(target)+`"}`)
	case "demote":
		return e.do(token, http.MethodPatch, base, `{"role":3}`)
	case "trim":
		return e.do(token, http.MethodPut, base+"/permissions", `{"codes":["user:list"],"bucket_ids":[]}`)
	case "reset":
		return e.do(token, http.MethodPost, base+"/password-reset", "")
	case "delete":
		return e.do(token, http.MethodDelete, base, "")
	case "revoke":
		return e.do(token, http.MethodPost, base+"/passkeys/revoke", "")
	}
	e.t.Fatalf("unknown op %s", op)
	return hierarchyResponse{}
}

var allHierarchyOps = []string{"rename", "demote", "trim", "reset", "revoke", "delete"}

func (e *hierarchyEnv) putCodes(actor, target string, codes ...string) hierarchyResponse {
	e.t.Helper()
	body, _ := json.Marshal(map[string]any{"codes": codes, "bucket_ids": []int{}})
	return e.do(e.token(actor), http.MethodPut, "/api/v1/users/"+e.id(target)+"/permissions", string(body))
}

func expect(t *testing.T, got hierarchyResponse, status int, code string) {
	t.Helper()
	if got.status != status || (code != "" && got.code != code) {
		t.Fatalf("status = %d code = %q, want %d %q; body = %s", got.status, got.code, status, code, got.body)
	}
}

func (e *hierarchyEnv) assertUnchanged(name string) {
	e.t.Helper()
	before, after := e.users[name], e.fresh(name)
	if after.Username != before.Username || after.Role != before.Role || after.Password != before.Password || after.AuthVersion != before.AuthVersion || !reflect.DeepEqual(after.Permission.Codes, before.Permission.Codes) {
		e.t.Fatalf("%s changed: before %+v after %+v", name, before, after)
	}
}

func TestLimitedAdminCannotGrantCodesItLacks(t *testing.T) {
	env := newHierarchyEnv(t)
	expect(t, env.putCodes("A", "L", "user:list", "storage:delete"), http.StatusForbidden, "permission_not_held")
	env.assertUnchanged("L")
	last := env.audit[len(env.audit)-1]
	if last.Outcome != "denied" || last.Code != "permission_not_held" {
		t.Fatalf("audit = %+v", last)
	}

	expect(t, env.putCodes("A", "L", "user:list", "tag:create"), http.StatusOK, "")
	if got := env.fresh("L").Permission.Codes; !reflect.DeepEqual(got, []string{"user:list", "tag:create"}) {
		t.Fatalf("L codes = %v", got)
	}
	expect(t, env.putCodes("A", "L", "bogus"), http.StatusUnprocessableEntity, "validation_error")
}

func TestPlainUserTargetsKeepOnlyBuckets(t *testing.T) {
	env := newHierarchyEnv(t)
	expect(t, env.putCodes("A", "Z", "tag:create"), http.StatusOK, "")
	expect(t, env.putCodes("A", "U", "storage:delete"), http.StatusOK, "")
	if got := env.fresh("U").Permission.Codes; len(got) != 0 {
		t.Fatalf("plain user codes = %v", got)
	}
}

func TestAdminCreationRequiresRoleUpdate(t *testing.T) {
	env := newHierarchyEnv(t)
	create := func(actor, name string, role int) hierarchyResponse {
		return env.do(env.token(actor), http.MethodPost, "/api/v1/users", `{"username":"`+name+`","password":"secret-password","role":`+strconv.Itoa(role)+`}`)
	}
	expect(t, create("C", "made-by-c", models.RoleAdmin), http.StatusForbidden, "permission_denied")
	var count int64
	env.system.Database.DB.Model(&models.User{}).Where("username = ?", "made-by-c").Count(&count)
	if count != 0 {
		t.Fatal("rejected admin was created")
	}
	expect(t, create("C", "plain-by-c", models.RoleUser), http.StatusCreated, "")

	created := create("X", "admin-by-x", models.RoleAdmin)
	expect(t, created, http.StatusCreated, "")
	var puppet models.User
	if err := env.system.Database.DB.Where("username = ?", "admin-by-x").First(&puppet).Error; err != nil {
		t.Fatal(err)
	}
	if len(puppet.Permission.Codes) != 0 {
		t.Fatalf("new admin codes = %v", puppet.Permission.Codes)
	}

	// A puppet raised to A's full set becomes A's peer: neither can act on the other.
	env.users["Puppet"] = puppet
	expect(t, env.putCodes("A", "Puppet", models.EffectiveCodes(env.users["A"])...), http.StatusOK, "")
	expect(t, env.op("A", "reset", "Puppet"), http.StatusForbidden, "user_not_manageable")
	expect(t, env.op("Puppet", "reset", "A"), http.StatusForbidden, "user_not_manageable")
}

func TestLimitedAdminCannotManageHigherEqualOrIncomparableAdmins(t *testing.T) {
	for _, pair := range [][2]string{{"A", "H"}, {"A", "P"}, {"A", "Pstar"}, {"A", "I"}, {"I", "A"}, {"W", "Pstar"}, {"W", "ALL"}} {
		for _, op := range allHierarchyOps {
			t.Run(pair[0]+" "+op+" "+pair[1], func(t *testing.T) {
				env := newHierarchyEnv(t)
				if err := env.system.Database.DB.Create(&models.PasskeyCredential{UserID: env.users[pair[1]].ID, Name: "key", CredentialID: "cred-" + op, CredentialData: "data"}).Error; err != nil {
					t.Fatal(err)
				}
				got := env.op(pair[0], op, pair[1])
				expect(t, got, http.StatusForbidden, "user_not_manageable")
				if strings.Contains(got.body, "new_password") {
					t.Fatal("denied reset returned a password")
				}
				env.assertUnchanged(pair[1])
				var passkeys int64
				env.system.Database.DB.Model(&models.PasskeyCredential{}).Where("user_id = ?", env.users[pair[1]].ID).Count(&passkeys)
				if passkeys != 1 {
					t.Fatal("denied operation removed passkeys")
				}
			})
		}
	}
}

func TestPeerCannotBeDemotedOrTrimmedBeforeReset(t *testing.T) {
	env := newHierarchyEnv(t)
	expect(t, env.op("A", "demote", "P"), http.StatusForbidden, "user_not_manageable")
	expect(t, env.op("A", "trim", "P"), http.StatusForbidden, "user_not_manageable")
	expect(t, env.op("A", "reset", "P"), http.StatusForbidden, "user_not_manageable")
	env.assertUnchanged("P")
}

func TestLimitedAdminManagesLowerAdminsAndPlainUsers(t *testing.T) {
	for _, target := range []string{"L", "Lunknown", "Z", "U"} {
		for _, op := range allHierarchyOps {
			t.Run(op+" "+target, func(t *testing.T) {
				env := newHierarchyEnv(t)
				got := env.op("A", op, target)
				want := map[string]int{"rename": 200, "demote": 200, "trim": 200, "reset": 200, "revoke": 204, "delete": 204}[op]
				expect(t, got, want, "")
			})
		}
	}
}

func TestWildcardActorCoversExplicitLowerAdmins(t *testing.T) {
	env := newHierarchyEnv(t)
	expect(t, env.putCodes("W", "H", "user:list", "storage:delete"), http.StatusOK, "")
}

func TestSuperAdminManagesEveryAdmin(t *testing.T) {
	for _, target := range []string{"H", "P", "Pstar", "ALL", "W"} {
		for _, op := range allHierarchyOps {
			t.Run(op+" "+target, func(t *testing.T) {
				env := newHierarchyEnv(t)
				got := env.op("S", op, target)
				if got.status >= 300 {
					t.Fatalf("status = %d body = %s", got.status, got.body)
				}
			})
		}
	}
}

func TestProtectedUsersAndMissingTargets(t *testing.T) {
	env := newHierarchyEnv(t)
	for _, op := range allHierarchyOps {
		expect(t, env.op("A", op, "S"), http.StatusForbidden, "protected_user")
		expect(t, env.op("A", op, "A"), http.StatusForbidden, "protected_user")
		expect(t, env.op("S", op, "S"), http.StatusForbidden, "protected_user")
	}
	env.users["Missing"] = models.User{ID: 9999}
	for _, op := range allHierarchyOps {
		expect(t, env.op("A", op, "Missing"), http.StatusNotFound, "user_not_found")
	}
}

func TestTokenScopeStillLimitsUserManagement(t *testing.T) {
	env := newHierarchyEnv(t)
	got := env.do(env.token("A", "users:read"), http.MethodPut, "/api/v1/users/"+env.id("L")+"/permissions", `{"codes":["user:list"],"bucket_ids":[]}`)
	expect(t, got, http.StatusForbidden, "insufficient_scope")
}

func TestRoleChangesClearOrKeepCodes(t *testing.T) {
	env := newHierarchyEnv(t)
	expect(t, env.op("A", "demote", "L"), http.StatusOK, "")
	if got := env.fresh("L").Permission.Codes; len(got) != 0 {
		t.Fatalf("demoted codes = %v", got)
	}

	expect(t, env.do(env.token("A"), http.MethodPatch, "/api/v1/users/"+env.id("R"), `{"role":3,"username":"still-plain"}`), http.StatusOK, "")
	if got := env.fresh("R").Permission.Codes; !reflect.DeepEqual(got, []string{"storage:delete"}) {
		t.Fatalf("unchanged plain role touched codes: %v", got)
	}
	expect(t, env.do(env.token("A"), http.MethodPatch, "/api/v1/users/"+env.id("R"), `{"role":1}`), http.StatusOK, "")
	var raw string
	if err := env.system.Database.DB.Raw("SELECT CAST(permission AS TEXT) FROM users WHERE id = ?", env.users["R"].ID).Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ReplaceAll(raw, " ", ""), `"codes":[]`) {
		t.Fatalf("promoted permission = %s", raw)
	}

	expect(t, env.do(env.token("A"), http.MethodPatch, "/api/v1/users/"+env.id("Z"), `{"role":1,"username":"still-admin"}`), http.StatusOK, "")
	expect(t, env.putCodes("A", "Z", "tag:create"), http.StatusOK, "")
	expect(t, env.do(env.token("A"), http.MethodPatch, "/api/v1/users/"+env.id("Z"), `{"role":1}`), http.StatusOK, "")
	if got := env.fresh("Z").Permission.Codes; !reflect.DeepEqual(got, []string{"tag:create"}) {
		t.Fatalf("unchanged role cleared codes: %v", got)
	}
}

func TestPasswordResetIsAFullAccountReset(t *testing.T) {
	env := newHierarchyEnv(t)
	target := env.users["L"]
	if err := env.system.Database.DB.Create(&models.PasskeyCredential{UserID: target.ID, Name: "key", CredentialID: "cred", CredentialData: "data"}).Error; err != nil {
		t.Fatal(err)
	}
	targetToken := env.token("L", "users:read")
	got := env.op("A", "reset", "L")
	expect(t, got, http.StatusOK, "")
	var payload struct {
		Data struct {
			NewPassword string `json:"new_password"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(got.body), &payload); err != nil || payload.Data.NewPassword == "" {
		t.Fatalf("reset body = %s", got.body)
	}
	reset := env.fresh("L")
	if !reset.PasswordChangeRequired {
		t.Fatal("reset did not require a password change")
	}
	var passkeys int64
	env.system.Database.DB.Model(&models.PasskeyCredential{}).Where("user_id = ?", target.ID).Count(&passkeys)
	if passkeys != 0 {
		t.Fatal("reset kept passkeys")
	}
	expect(t, env.do(targetToken, http.MethodGet, "/api/v1/users", ""), http.StatusUnauthorized, "")
	last := env.audit[len(env.audit)-1]
	if last != (services.UserAuditEvent{Op: "user.password_reset", ActorID: env.users["A"].ID, TargetID: target.ID, Outcome: "ok"}) {
		t.Fatalf("audit = %+v", last)
	}
	if strings.Contains(strings.Join([]string{last.Op, last.Outcome, last.Code}, " "), payload.Data.NewPassword) {
		t.Fatal("audit contains the temporary password")
	}

	login := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"`+target.Username+`","password":"`+payload.Data.NewPassword+`"}`))
	request.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(login, request)
	if login.Code != http.StatusOK {
		t.Fatalf("temporary password login = %d %s", login.Code, login.Body.String())
	}
	session := func(method, path string) int {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		for _, cookie := range login.Result().Cookies() {
			req.AddCookie(cookie)
			if cookie.Name == "oneimg-csrf" {
				req.Header.Set("X-OneImg-CSRF", cookie.Value)
			}
		}
		env.router.ServeHTTP(recorder, req)
		return recorder.Code
	}
	if session(http.MethodGet, "/api/v1/me") != http.StatusOK || session(http.MethodGet, "/api/v1/images") != http.StatusForbidden || session(http.MethodPost, "/api/v1/me/tokens") != http.StatusForbidden {
		t.Fatal("temporary password session is not confined to the account page")
	}
}
