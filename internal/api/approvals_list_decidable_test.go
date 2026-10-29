// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestListApprovals_DecidableCarriesTheEgressHostGrant pins #2604: GET /approvals stamps each PENDING row with
// `decidable`, the caller's own verdict with the egress_host capability applied, so the console turns Approve and
// Deny off before the first click. A member without the host's grant reads false, one with it true, an admin true,
// and a credential_reauth row false for everyone.
func TestListApprovals_DecidableCarriesTheEgressHostGrant(t *testing.T) {
	const memberSub, adminSub = "sub-member-dec", "sub-admin-dec"
	host := json.RawMessage(`{"host":"registry.npmjs.org"}`)
	allow := types.CapabilityGrant{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: memberSub, Capability: capEgressHost, Value: "registry.npmjs.org", Effect: types.CapabilityAllow}
	enforced := map[string]bool{capEgressHost: true}

	for name, tc := range map[string]struct {
		kind   types.ApprovalKind
		sub    string
		role   string
		grants []types.CapabilityGrant
		enf    map[string]bool
		owner  string
		want   bool
	}{
		"member, host enforced and not granted: false": {types.ApprovalEgressDomain, memberSub, oidc.RoleUser, []types.CapabilityGrant{}, enforced, memberSub, false},
		"member, host granted: true":                   {types.ApprovalEgressDomain, memberSub, oidc.RoleUser, []types.CapabilityGrant{allow}, enforced, memberSub, true},
		"member, capability not enforced: true":        {types.ApprovalEgressDomain, memberSub, oidc.RoleUser, nil, nil, memberSub, true},
		"member, tool_call is admin-only: false":       {types.ApprovalToolCall, memberSub, oidc.RoleUser, nil, nil, memberSub, false},
		"admin, no grant needed: true":                 {types.ApprovalEgressDomain, adminSub, oidc.RoleAdmin, []types.CapabilityGrant{}, enforced, memberSub, true},
		"admin, credential_reauth decides for nobody":  {types.ApprovalCredentialReauth, adminSub, oidc.RoleAdmin, nil, nil, adminSub, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMDFixture(t, tc.kind, host, nil, tc.owner, tc.grants, tc.enf)
			w := doSSO(t, f.srv, http.MethodGet, "/api/v1/approvals?state=PENDING", ssoSession(t, tc.sub, tc.sub+"@corp.example", tc.role), "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET /approvals = %d: %s", w.Code, w.Body.String())
			}
			var rows []struct {
				ID        uuid.UUID `json:"id"`
				Decidable *bool     `json:"decidable"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
				t.Fatalf("decode: %v: %s", err, w.Body.String())
			}
			if len(rows) != 1 || rows[0].ID != f.ap.ID {
				t.Fatalf("rows = %+v, want the one seeded approval", rows)
			}
			if rows[0].Decidable == nil || *rows[0].Decidable != tc.want {
				t.Fatalf("decidable = %v, want %v (body %s)", rows[0].Decidable, tc.want, w.Body.String())
			}
		})
	}
}

// wallsPrefsStore is a store plus the principal_prefs table, which the approvals fixture's store does not keep.
type wallsPrefsStore struct {
	store.Store
	prefs map[string]json.RawMessage
}

func (s *wallsPrefsStore) GetPrincipalPref(_ context.Context, principal, key string) (json.RawMessage, error) {
	if v, ok := s.prefs[principal+"|"+key]; ok {
		return v, nil
	}
	return nil, store.ErrNotFound
}

func (s *wallsPrefsStore) PutPrincipalPref(_ context.Context, principal, key string, value json.RawMessage) error {
	s.prefs[principal+"|"+key] = value
	return nil
}

// TestListApprovals_WallsBlockCarriesTheOwnersWalls: GET /approvals stamps walls_block on a PENDING egress_domain
// row whose host the run owner's walls deny, and leaves a host outside their deny list unmarked.
func TestListApprovals_WallsBlockCarriesTheOwnersWalls(t *testing.T) {
	const adminSub = "sub-admin-walls"
	for host, want := range map[string]bool{"a.evil.example": true, "docs.example.com": false} {
		t.Run(host, func(t *testing.T) {
			var wp *wallsPrefsStore
			f := newMDFixture(t, types.ApprovalEgressDomain, json.RawMessage(`{"host":"`+host+`"}`), nil, wallsSub, nil, nil, func(c *Config) {
				wp = &wallsPrefsStore{Store: c.Store, prefs: map[string]json.RawMessage{}}
				c.Store = wp
			})
			saveWalls(t, wp, wallsSub, types.PersonalWallsSpec{DeniedDomains: wallsList("*.evil.example")})
			w := doSSO(t, f.srv, http.MethodGet, "/api/v1/approvals?state=PENDING", ssoSession(t, adminSub, adminSub+"@corp.example", oidc.RoleAdmin), "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET /approvals = %d: %s", w.Code, w.Body.String())
			}
			var rows []struct {
				ID         uuid.UUID `json:"id"`
				WallsBlock bool      `json:"walls_block"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
				t.Fatalf("decode: %v: %s", err, w.Body.String())
			}
			if len(rows) != 1 || rows[0].ID != f.ap.ID {
				t.Fatalf("rows = %+v, want the one seeded approval", rows)
			}
			if rows[0].WallsBlock != want {
				t.Fatalf("walls_block = %v, want %v (body %s)", rows[0].WallsBlock, want, w.Body.String())
			}
		})
	}
}
