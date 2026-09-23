// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// User-type derivation (0.8, user-types design §2.4): which type a sign-in
// carries, when it is refused over one, and that no type value ever reaches an
// admin tier.
package oidc_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeUserTypeSource is an in-memory writoidc.UserTypeSource.
type fakeUserTypeSource struct {
	list []types.UserType
	err  error
}

func (f *fakeUserTypeSource) ListUserTypes(context.Context) ([]types.UserType, error) {
	return f.list, f.err
}

// orgTypes is a deployment with three custom types: two share priority 10.
var orgTypes = []types.UserType{
	{ID: types.UserTypeStandard, Name: "Standard user", BuiltIn: true},
	{ID: "portfolio-manager", Name: "Portfolio manager", Priority: 10},
	{ID: "analyst", Name: "Analyst", Priority: 10},
	{ID: "developer", Name: "Developer", Priority: 5},
	{ID: "intern", Name: "Intern", Priority: 0},
}

func TestDeriveUserType(t *testing.T) {
	roleMap := map[string]string{
		"wardyn.admin":    writoidc.RoleAdmin,
		"wardyn.security": writoidc.RoleSecurityAdmin,
		"wardyn.member":   writoidc.RoleUser, // a migrated =member line
		"pm-group":        "portfolio-manager",
		"quant-group":     "analyst",
		"eng-group":       "developer",
		"eng-group-2":     "developer",
		"ghost-group":     "contractor", // no such type
		"intern-group":    "intern",     // priority 0, the built-in type's own
	}
	cases := []struct {
		name         string
		groups       []string
		defaultRole  string
		wantRole     string
		wantType     string
		wantDenial   string
		wantTied     []string
		wantUnknown  []string
		wantDefMatch bool
	}{
		{name: "a user value is the built-in type", groups: []string{"wardyn.member"}, wantRole: writoidc.RoleUser, wantType: types.UserTypeStandard},
		{name: "a type value is the user tier on that type", groups: []string{"pm-group"}, wantRole: writoidc.RoleUser, wantType: "portfolio-manager"},
		{name: "the built-in type never wins against a custom one", groups: []string{"wardyn.member", "eng-group"}, wantRole: writoidc.RoleUser, wantType: "developer"},
		{name: "the built-in type loses even at equal priority", groups: []string{"wardyn.member", "intern-group"}, wantRole: writoidc.RoleUser, wantType: "intern"},
		{name: "the higher priority wins", groups: []string{"eng-group", "pm-group"}, wantRole: writoidc.RoleUser, wantType: "portfolio-manager"},
		{name: "one type named twice is not a tie", groups: []string{"eng-group", "eng-group-2"}, wantRole: writoidc.RoleUser, wantType: "developer"},
		{name: "two custom types at the top priority refuse", groups: []string{"pm-group", "quant-group", "eng-group", "wardyn.member"},
			wantDenial: writoidc.DenialUserTypeAmbiguous, wantTied: []string{"analyst", "portfolio-manager"}},
		{name: "a missing type refuses", groups: []string{"ghost-group"}, wantDenial: writoidc.DenialUserTypeUnknown, wantUnknown: []string{"contractor"}},
		{name: "a missing type refuses beside a real one", groups: []string{"pm-group", "ghost-group"}, wantDenial: writoidc.DenialUserTypeUnknown, wantUnknown: []string{"contractor"}},
		{name: "a missing type refuses even with an admin default", groups: []string{"ghost-group"}, defaultRole: writoidc.RoleAdmin,
			wantDenial: writoidc.DenialUserTypeUnknown, wantUnknown: []string{"contractor"}},
		{name: "an admin records their type too", groups: []string{"wardyn.admin", "pm-group"}, wantRole: writoidc.RoleAdmin, wantType: "portfolio-manager"},
		{name: "an admin with no type is on the built-in one", groups: []string{"wardyn.admin"}, wantRole: writoidc.RoleAdmin, wantType: types.UserTypeStandard},
		{name: "an admin's tie falls to the built-in type", groups: []string{"wardyn.admin", "pm-group", "quant-group"},
			wantRole: writoidc.RoleAdmin, wantType: types.UserTypeStandard, wantTied: []string{"analyst", "portfolio-manager"}},
		{name: "an admin's missing type falls to the built-in type", groups: []string{"wardyn.admin", "ghost-group"},
			wantRole: writoidc.RoleAdmin, wantType: types.UserTypeStandard, wantUnknown: []string{"contractor"}},
		{name: "an admin under a default naming a missing type is on the built-in type", groups: []string{"wardyn.admin"}, defaultRole: "contractor",
			wantRole: writoidc.RoleAdmin, wantType: types.UserTypeStandard, wantUnknown: []string{"contractor"}},
		{name: "a security admin's tie refuses", groups: []string{"wardyn.security", "pm-group", "quant-group"},
			wantDenial: writoidc.DenialUserTypeAmbiguous, wantTied: []string{"analyst", "portfolio-manager"}},
		{name: "a security admin under a default naming a missing type refuses", groups: []string{"wardyn.security"}, defaultRole: "contractor",
			wantDenial: writoidc.DenialUserTypeUnknown, wantUnknown: []string{"contractor"}},
		{name: "a default role may name a type", groups: []string{"nobody"}, defaultRole: "developer", wantRole: writoidc.RoleUser, wantType: "developer", wantDefMatch: true},
		{name: "a default user is the built-in type", groups: []string{"nobody"}, defaultRole: writoidc.RoleUser, wantRole: writoidc.RoleUser, wantType: types.UserTypeStandard, wantDefMatch: true},
		{name: "a default naming a missing type refuses", groups: []string{"nobody"}, defaultRole: "contractor",
			wantDenial: writoidc.DenialUserTypeUnknown, wantUnknown: []string{"contractor"}},
		{name: "a matched tier with no type takes the default's type", groups: []string{"wardyn.admin"}, defaultRole: "developer", wantRole: writoidc.RoleAdmin, wantType: "developer"},
		{name: "a matched user value keeps the built-in type over the default's", groups: []string{"wardyn.member"}, defaultRole: "developer", wantRole: writoidc.RoleUser, wantType: types.UserTypeStandard},
		{name: "nothing matched and no default refuses", groups: []string{"nobody"}, wantDenial: writoidc.DenialNoRole},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := writoidc.DeriveForTest(nil, c.groups, "", roleMap, nil, c.defaultRole, orgTypes)
			if d.Denial != c.wantDenial || d.Role != c.wantRole || d.UserType != c.wantType {
				t.Fatalf("derivation = %+v, want role %q type %q denial %q", d, c.wantRole, c.wantType, c.wantDenial)
			}
			if d.OK() != (c.wantDenial == "") {
				t.Errorf("OK() = %v with denial %q", d.OK(), d.Denial)
			}
			if !slices.Equal(d.Tied, c.wantTied) || !slices.Equal(d.Unknown, c.wantUnknown) {
				t.Errorf("tied = %v unknown = %v, want %v and %v", d.Tied, d.Unknown, c.wantTied, c.wantUnknown)
			}
			if c.wantDefMatch && (len(d.Matches) != 1 || d.Matches[0].Source != writoidc.MatchSourceDefaultRole || d.Matches[0].UserType != c.wantType) {
				t.Errorf("matches = %+v, want the one default-role match on %q", d.Matches, c.wantType)
			}
		})
	}
}

// TestDeriveUserTypeArmOne: with no role map every sign-in is on the built-in
// type, whatever the store holds — arm 1 is unchanged by types.
func TestDeriveUserTypeArmOne(t *testing.T) {
	d := writoidc.DeriveForTest(nil, []string{"pm-group"}, "a@corp.example", nil, nil, "", orgTypes)
	if !d.OK() || d.Role != writoidc.RoleAdmin || d.UserType != types.UserTypeStandard {
		t.Errorf("no map, no allowlist = %+v, want admin on standard", d)
	}
	d = writoidc.DeriveForTest(nil, []string{"pm-group"}, "a@corp.example", nil, []string{"ops@corp.example"}, "developer", orgTypes)
	if !d.OK() || d.Role != writoidc.RoleUser || d.UserType != types.UserTypeStandard {
		t.Errorf("allowlist only, unlisted = %+v, want user on standard", d)
	}
}

// TestUserTypeValuesNeverReachAnAdminTier is the nonescape half: whatever a
// type value is spelled as, it is the user tier or nothing. A value that folds,
// pads or abbreviates to a tier word is not a type id at all, and a reserved
// word is never one.
func TestUserTypeValuesNeverReachAnAdminTier(t *testing.T) {
	for _, v := range []string{"Admin", "ADMIN", " admin", "admin ", "security-admin", "Security_Admin", "administrator",
		"member", "denied", "user", "standard", "portfolio-manager", "a", "admin-", "-admin", "admin--x", "adminé"} {
		role, userType, ok := writoidc.SplitMappingTarget(v)
		if !ok {
			continue
		}
		if role != writoidc.RoleUser || userType == "" {
			t.Errorf("SplitMappingTarget(%q) = (%q, %q), want the user tier on a type", v, role, userType)
		}
	}
	for _, v := range []string{writoidc.RoleAdmin, writoidc.RoleSecurityAdmin, writoidc.RoleUser, writoidc.LegacyRoleMember, "denied"} {
		if !writoidc.UserTypeIDReserved(v) {
			t.Errorf("UserTypeIDReserved(%q) = false; a type with that id could be read as a tier", v)
		}
	}
	// The retired tier word and the People page's no-role word are never a
	// type: "member" is resolved where configuration is parsed, never here.
	for _, v := range []string{writoidc.LegacyRoleMember, "denied"} {
		if role, userType, ok := writoidc.SplitMappingTarget(v); ok {
			t.Errorf("SplitMappingTarget(%q) = (%q, %q, true), want not a mapping target", v, role, userType)
		}
	}

	// A console row is a tier plus a type; a type field spelling a tier on a
	// user row, or any type on an admin row, drops the row — it never
	// becomes an admin value in the merged map.
	rows := []writoidc.RoleMapping{
		{Value: "g1", Role: writoidc.RoleUser, UserType: writoidc.RoleAdmin},
		{Value: "g2", Role: writoidc.RoleUser, UserType: writoidc.RoleSecurityAdmin},
		{Value: "g3", Role: writoidc.RoleAdmin, UserType: "portfolio-manager"},
		{Value: "g4", Role: writoidc.RoleUser, UserType: "Admin"},
		{Value: "g5", Role: writoidc.RoleUser, UserType: "portfolio-manager"},
	}
	merged, shadowed := writoidc.MergeRoleMapsForTest(nil, nil, rows)
	if len(merged) != 1 || merged["g5"] != "portfolio-manager" {
		t.Errorf("merged = %v, want only g5 => portfolio-manager", merged)
	}
	if !slices.Equal(shadowed, []string{"g1", "g2", "g3", "g4"}) {
		t.Errorf("shadowed = %v, want g1..g4 dropped", shadowed)
	}
	d := writoidc.DeriveForTest(nil, []string{"g1", "g2", "g3", "g4", "g5"}, "", merged, nil, "", orgTypes)
	if d.Role != writoidc.RoleUser {
		t.Errorf("derivation = %+v, want the user tier", d)
	}

	// A store row named like a tier cannot exist (the table CHECKs it), but
	// even one that did changes nothing: a tier word is resolved before any
	// type lookup.
	withAdminType := append(slices.Clone(orgTypes), types.UserType{ID: writoidc.RoleAdmin, Priority: 1000})
	d = writoidc.DeriveForTest(nil, []string{"pm-group"}, "", map[string]string{"pm-group": "portfolio-manager"}, nil, "", withAdminType)
	if d.Role != writoidc.RoleUser || d.UserType != "portfolio-manager" {
		t.Errorf("derivation with a type row named admin = %+v, want user on portfolio-manager", d)
	}
}

// TestCallbackStampsTheDerivedUserType: the session carries the type the
// sign-in derived, and the context publishes it beside the tier.
func TestCallbackStampsTheDerivedUserType(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleMappingAuth(t, map[string]string{"pm-group": "portfolio-manager", "wardyn.member": writoidc.RoleUser}, "", nil, nil,
		func(c *writoidc.Config) { c.UserTypes = &fakeUserTypeSource{list: orgTypes} })

	_, sess := doRoleCallback(t, env, auth, "pat@corp.example", nil, []string{"pm-group", "wardyn.member"})
	if sess.Role != writoidc.RoleUser || sess.UserType != "portfolio-manager" {
		t.Errorf("session = role %q type %q, want user on portfolio-manager", sess.Role, sess.UserType)
	}
}

// TestCallbackRefusesOverTheUserType: an ambiguous or unknown type refuses the
// sign-in with its own auth_error code, clears the cookie, and is reported to
// the denial hook; a store error refuses it as a role check that could not run.
func TestCallbackRefusesOverTheUserType(t *testing.T) {
	roleMap := map[string]string{"pm-group": "portfolio-manager", "quant-group": "analyst", "ghost-group": "contractor"}
	cases := []struct {
		name, want, reported string
		groups               []string
		src                  *fakeUserTypeSource
	}{
		{"ambiguous", writoidc.DenialUserTypeAmbiguous, writoidc.DenialUserTypeAmbiguous, []string{"pm-group", "quant-group"}, &fakeUserTypeSource{list: orgTypes}},
		{"unknown", writoidc.DenialUserTypeUnknown, writoidc.DenialUserTypeUnknown, []string{"ghost-group"}, &fakeUserTypeSource{list: orgTypes}},
		{"store error", "role_check_unavailable", "", []string{"pm-group"}, &fakeUserTypeSource{err: errors.New("pg: connection refused")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newIdPEnv(t)
			auth := env.newRoleMappingAuth(t, roleMap, writoidc.RoleAdmin, nil, nil, func(cfg *writoidc.Config) { cfg.UserTypes = c.src })
			var reported []string
			env.buildIDTokenWithRoles(t, "sub-role", "x@corp.example", nil, c.groups, roleCallbackNonce, time.Now().Add(time.Hour))
			w, sess := doCallbackVia(t, auth, auth.CallbackHandlerWithDenials(func(_ *http.Request, reason string) {
				reported = append(reported, reason)
			}))
			if loc := w.Result().Header.Get("Location"); !containsAuthError(loc, c.want) {
				t.Errorf("Location = %q, want auth_error=%s", loc, c.want)
			}
			if sess.Role != "" {
				t.Errorf("a session was issued (role %q) for a refused sign-in", sess.Role)
			}
			assertSessionCookieCleared(t, w)
			var want []string
			if c.reported != "" {
				want = []string{c.reported}
			}
			if !slices.Equal(reported, want) {
				t.Errorf("reported = %v, want %v", reported, want)
			}
		})
	}
}

// TestCallbackAdminNeverRefusedOverTheUserType: an operator-allowlist admin
// whose values tie, under a default naming a missing type, still signs in, on
// the built-in type, and nothing is reported as a type refusal.
func TestCallbackAdminNeverRefusedOverTheUserType(t *testing.T) {
	env := newIdPEnv(t)
	roleMap := map[string]string{"pm-group": "portfolio-manager", "quant-group": "analyst"}
	auth := env.newRoleMappingAuth(t, roleMap, "contractor", []string{"ops@corp.example"}, nil,
		func(c *writoidc.Config) { c.UserTypes = &fakeUserTypeSource{list: orgTypes} })
	var reported []string
	env.buildIDTokenWithRoles(t, "sub-ops", "ops@corp.example", nil, []string{"pm-group", "quant-group"}, roleCallbackNonce, time.Now().Add(time.Hour))
	_, sess := doCallbackVia(t, auth, auth.CallbackHandlerWithDenials(func(_ *http.Request, reason string) {
		reported = append(reported, reason)
	}))
	if sess.Role != writoidc.RoleAdmin || sess.UserType != types.UserTypeStandard {
		t.Errorf("session = role %q type %q, want admin on standard", sess.Role, sess.UserType)
	}
	if len(reported) != 0 {
		t.Errorf("reported = %v, want no type refusal", reported)
	}
}

// TestPreviewRoleReadsUserTypes: the People page's preview derives against the
// store's types, and a failed read is the preview's error, never a guess.
func TestPreviewRoleReadsUserTypes(t *testing.T) {
	env := newIdPEnv(t)
	src := &fakeUserTypeSource{list: orgTypes}
	auth := env.newRoleMappingAuth(t, nil, "", nil,
		&fakeRoleMappingSource{rows: []writoidc.RoleMapping{
			{Value: "pm-group", Role: writoidc.RoleUser, UserType: "portfolio-manager"},
			{Value: "quant-group", Role: writoidc.RoleUser, UserType: "analyst"},
			{Value: "wardyn.member", Role: writoidc.RoleUser, UserType: types.UserTypeStandard},
		}},
		func(c *writoidc.Config) { c.UserTypes = src })

	d, err := auth.PreviewRole(context.Background(), nil, []string{"pm-group", "wardyn.member"}, "")
	if err != nil || !d.OK() || d.UserType != "portfolio-manager" {
		t.Fatalf("preview = %+v, %v; want portfolio-manager", d, err)
	}
	if len(d.Matches) != 2 {
		t.Errorf("matches = %+v, want both rows, the losing built-in one included", d.Matches)
	}
	d, err = auth.PreviewRole(context.Background(), nil, []string{"pm-group", "quant-group"}, "")
	if err != nil || d.Denial != writoidc.DenialUserTypeAmbiguous || !slices.Equal(d.Tied, []string{"analyst", "portfolio-manager"}) {
		t.Errorf("tied preview = %+v, %v; want the ambiguity named", d, err)
	}

	src.err = errors.New("pg: connection refused")
	if d, err = auth.PreviewRole(context.Background(), nil, []string{"pm-group"}, ""); err == nil || d.OK() {
		t.Errorf("preview with an unreadable type store = %+v, %v; want the error", d, err)
	}
}
