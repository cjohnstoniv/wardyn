// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAPITokenCarriesTheMintingSessionUserType: a token is stamped with the
// user type of the session that minted it, and every request it authenticates
// publishes that stamp, never the built-in type and never nothing.
func TestAPITokenCarriesTheMintingSessionUserType(t *testing.T) {
	srv, st, h := apiTokenTestServer(t)
	sess := ssoSessionOfType(t, tokenMemberSub, tokenMemberMail, oidc.RoleUser, "portfolio-manager")
	raw, created := mintToken(t, srv, sess, "ci")

	if created.UserType != "portfolio-manager" {
		t.Errorf("minted user_type = %q, want the session's portfolio-manager", created.UserType)
	}
	if row := st.byID[created.ID]; row.UserType != "portfolio-manager" {
		t.Errorf("stored user_type = %q, want portfolio-manager", row.UserType)
	}
	var data map[string]any
	creates := auditActions(h, "token.create")
	if len(creates) != 1 || json.Unmarshal(creates[0].Data, &data) != nil || data["user_type"] != "portfolio-manager" {
		t.Errorf("token.create rows = %+v, want one naming user_type portfolio-manager", creates)
	}

	w := do(t, srv, http.MethodGet, "/api/v1/me", raw, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /me with the token = %d %s", w.Code, w.Body.String())
	}
	var me map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if ut, _ := me["user_type"].(map[string]any); ut["id"] != "portfolio-manager" {
		t.Errorf("/me user_type through the token = %v, want portfolio-manager — the token lane dropped the type", me["user_type"])
	}
}

// TestRoleMappingTypeChangeRevokesTokensCarryingTheOldType is the §7 rule
// (owner decision D8): a People-page edit that changes the type a value
// derives revokes every live token stamped with the OLD type that names the
// value, or whose group snapshot cannot say whether it does. Tokens on another
// type, and tokens that provably do not name the value, keep working.
func TestRoleMappingTypeChangeRevokesTokensCarryingTheOldType(t *testing.T) {
	const pmGroup = "pm-group"
	complete := false
	now := time.Now().UTC()
	fixture := func() []types.APIToken {
		tok := func(sub, userType string, groups []string) types.APIToken {
			tk := types.APIToken{ID: uuid.New(), Principal: sub, Email: sub + "@corp.example", Role: oidc.RoleUser,
				UserType: userType, Groups: groups, CreatedAt: now}
			if groups != nil {
				tk.GroupsTruncated = &complete
			}
			return tk
		}
		return []types.APIToken{
			tok("sub-alice", "portfolio-manager", []string{pmGroup}), // names the value, old type: revoked
			tok("sub-bob", "portfolio-manager", []string{"desk-b"}),  // old type, provably another group: kept
			tok("sub-carol", types.UserTypeStandard, []string{pmGroup}),
			tok("sub-dan", "portfolio-manager", nil), // unanswerable snapshot, old type: revoked
			tok("sub-erin", "analyst", nil),          // unanswerable, another type: kept
		}
	}
	newSrv := func(t *testing.T) (*Server, *roleMapTokenStore, uuid.UUID) {
		t.Helper()
		id := uuid.New()
		st := &roleMapTokenStore{toks: fixture()}
		st.rows = []types.RoleMapping{{ID: id, Value: pmGroup, Role: oidc.RoleUser, UserType: "portfolio-manager"}}
		st.userTypes = accessOrgTypes
		auth := newAccessAuth(t, map[string]string{"chart-admin": oidc.RoleAdmin}, oidc.RoleUser, nil, &st.roleMapStore)
		cfg := baseTestConfig(newHarness(t), st)
		cfg.OIDC = auth
		return New(cfg), st, id
	}
	assertRevoked := func(t *testing.T, st *roleMapTokenStore, want []string) {
		t.Helper()
		live := st.liveRoles()
		for _, p := range []string{"sub-alice", "sub-bob", "sub-carol", "sub-dan", "sub-erin"} {
			_, isLive := live[p]
			if revoked := slices.Contains(want, p); revoked == isLive {
				t.Errorf("%s live = %v, want revoked = %v (live set %v)", p, isLive, revoked, live)
			}
		}
	}
	tokensRevoked := func(t *testing.T, body []byte) int {
		t.Helper()
		var got struct {
			TokensRevoked int `json:"tokens_revoked"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		return got.TokensRevoked
	}

	t.Run("changing the row's type revokes the old type's tokens", func(t *testing.T) {
		srv, st, _ := newSrv(t)
		w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken,
			`{"value":"`+pmGroup+`","role":"user","user_type":"analyst","acknowledge_access_change":true}`)
		if w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("upsert = %d %s", w.Code, w.Body.String())
		}
		assertRevoked(t, st, []string{"sub-alice", "sub-dan"})
		if n := tokensRevoked(t, w.Body.Bytes()); n != 2 {
			t.Errorf("tokens_revoked = %d, want 2", n)
		}
	})

	t.Run("deleting the row moves its people to the default type and revokes too", func(t *testing.T) {
		srv, st, id := newSrv(t)
		w := do(t, srv, http.MethodDelete, "/api/v1/access/mappings/"+id.String()+"?acknowledge_access_change=true", adminToken, "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("delete = %d %s", w.Code, w.Body.String())
		}
		assertRevoked(t, st, []string{"sub-alice", "sub-dan"})
	})

	t.Run("re-saving the same type revokes nothing", func(t *testing.T) {
		srv, st, _ := newSrv(t)
		w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken,
			`{"value":"`+pmGroup+`","role":"user","user_type":"portfolio-manager","acknowledge_access_change":true}`)
		if w.Code != http.StatusOK && w.Code != http.StatusCreated {
			t.Fatalf("upsert = %d %s", w.Code, w.Body.String())
		}
		assertRevoked(t, st, nil)
	})
}
