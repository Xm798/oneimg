package models

import (
	"errors"
	"reflect"
	"testing"
)

func admin(id int, codes ...string) User {
	return User{ID: id, Role: RoleAdmin, Permission: Permission{Codes: codes}}
}

var userCodes = []string{"user:list", "user:create", "user:delete", "user:role:update", "user:permission:update", "user:password:reset", "user:passkey:reset"}

func withCodes(base []string, extra ...string) []string {
	return append(append([]string(nil), base...), extra...)
}

func TestEffectiveCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		user User
		want []string
	}{
		{"super admin", User{ID: SuperAdminID, Role: RoleAdmin}, AllPermissionCodes()},
		{"admin keeps canonical order", admin(2, "tag:create", "user:list", "tag:create"), []string{"user:list", "tag:create"}},
		{"wildcard expands", admin(2, "*"), AllPermissionCodes()},
		{"unknown codes are ignored", admin(2, "user:list", "setting:api"), []string{"user:list"}},
		{"plain user has none", User{ID: 3, Role: RoleUser, Permission: Permission{Codes: []string{"storage:delete"}}}, []string{}},
		{"admin without codes", admin(2), []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := EffectiveCodes(test.user); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("EffectiveCodes = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCanMatchesLegacyRoleAndCodeCheck(t *testing.T) {
	legacy := func(u User, code string) bool {
		if u.ID == SuperAdminID {
			return true
		}
		if u.Role != RoleAdmin {
			return false
		}
		for _, current := range u.Permission.Codes {
			if current == code || current == "*" {
				return true
			}
		}
		return false
	}
	fixtures := []User{
		{ID: SuperAdminID, Role: RoleAdmin},
		admin(2, userCodes...),
		admin(3, "*"),
		admin(4, "user:list", "setting:api"),
		admin(5),
		{ID: 6, Role: RoleUser, Permission: Permission{Codes: []string{"storage:delete"}}},
	}
	for _, user := range fixtures {
		for _, code := range AllPermissionCodes() {
			if got, want := Can(user, code), legacy(user, code); got != want {
				t.Fatalf("Can(user %d, %s) = %v, want %v", user.ID, code, got, want)
			}
		}
	}
	if Can(admin(4, "user:list", "setting:api"), "setting:api") {
		t.Fatal("unknown stored code must not grant anything")
	}
}

func TestCovers(t *testing.T) {
	actor := admin(2, withCodes(userCodes, "tag:create")...)
	higher := admin(3, withCodes(userCodes, "tag:create", "storage:delete")...)
	peer := admin(4, withCodes(userCodes, "tag:create")...)
	incomparable := admin(5, withCodes(userCodes, "storage:delete")...)
	lower := admin(6, "user:list")
	zero := admin(7)
	plain := User{ID: 8, Role: RoleUser}
	wildcard := admin(9, "*")
	explicitAll := admin(10, AllPermissionCodes()...)
	super := User{ID: SuperAdminID, Role: RoleAdmin}
	for _, test := range []struct {
		name          string
		actor, target User
		want          bool
	}{
		{"strictly lower admin", actor, lower, true},
		{"zero-code admin", actor, zero, true},
		{"plain user", actor, plain, true},
		{"higher admin", actor, higher, false},
		{"equal peer", actor, peer, false},
		{"incomparable admin", actor, incomparable, false},
		{"incomparable reverse", incomparable, actor, false},
		{"wildcard target equals all", actor, wildcard, false},
		{"wildcard actor over lower admin", wildcard, higher, true},
		{"wildcard actor over explicit all", wildcard, explicitAll, false},
		{"super admin over all", super, wildcard, true},
		{"super admin never covered", wildcard, super, false},
		{"super admin over itself", super, super, false},
		{"plain actor over plain user", User{ID: 11, Role: RoleUser}, plain, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := Covers(test.actor, test.target); got != test.want {
				t.Fatalf("Covers = %v, want %v", got, test.want)
			}
		})
	}
}

func TestValidatePermissionCodesReturnsSentinel(t *testing.T) {
	if err := ValidatePermissionCodes([]string{"user:list", "bogus"}); !errors.Is(err, ErrPermissionCodeInvalid) {
		t.Fatalf("err = %v", err)
	}
	if err := ValidatePermissionCodes(AllPermissionCodes()); err != nil {
		t.Fatal(err)
	}
}
