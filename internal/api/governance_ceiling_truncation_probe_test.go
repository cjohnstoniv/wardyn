// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F2-sso-to-ceiling PROBE 1 — destination: internal/api/governance_ceiling_truncation_probe_test.go
//
// Package api (white-box), same style as governance_ceiling_test.go. Reuses that
// file's fixtures (govServer, govMemberCtx, govProfile, containsAll) and
// governance_nonescape_test.go's (govSession, newGovEscapeStore, doSSO).
//
// INVARIANT UNDER TEST: a caller whose group snapshot is TRUNCATED (present,
// non-nil, incomplete — GroupsTruncated=true on the cookie / api_tokens row)
// and for whom a group-tier governance assignment EXISTS is REFUSED (403
// groups_snapshot_stale via governance.go writeCeilingError) unless a
// USER-tier row settles their ceiling — never silently served the deployment
// ceiling, and never served a surviving group's row either.
//
// Run (read-only lane: copy in, run, remove — never commit):
//
//	cd /home/cjohn/wt-v07-profiles && \
//	cp local/review-0.7/deep/F2-sso-to-ceiling/governance_ceiling_truncation_probe_test.go internal/api/ && \
//	nice -n 10 GOMAXPROCS=8 go test ./internal/api/ -run 'TestF2_' -count=1 -p 4 -v ; \
//	rm -f internal/api/governance_ceiling_truncation_probe_test.go
package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestF2_TruncatedSnapshotRefusesNotWidens is the table half: every shape a
// truncated snapshot can meet the store in, and which of them may be served.
func TestF2_TruncatedSnapshotRefusesNotWidens(t *testing.T) {
	cases := []struct {
		name      string
		groups    []string
		truncated bool
		st        *capStore
		wantErr   bool   // errGroupsSnapshotStale expected
		wantProf  string // "" = deployment ceiling expected (only when !wantErr)
	}{
		{
			// The load-bearing row: the SURVIVING half of the snapshot would
			// match a group row. Serving it would make the answer depend on
			// alphabetical luck (effectiveCeiling's truncated arm in
			// governance.go) — must refuse.
			name:   "truncated, a surviving group matches a GROUP row, group tier exists",
			groups: []string{"a-team"}, truncated: true,
			st:      &capStore{govProfile: govProfile("a-team-walled"), govTier: types.CapabilitySubjectGroup, govHasGroupTier: true},
			wantErr: true,
		},
		{
			name:   "truncated, only an ALL row matches, group tier exists",
			groups: []string{"a-team"}, truncated: true,
			st:      &capStore{govProfile: govProfile("everyone"), govTier: types.CapabilitySubjectAll, govHasGroupTier: true},
			wantErr: true,
		},
		{
			name:   "truncated, nothing matches on users, group tier exists",
			groups: []string{"a-team"}, truncated: true,
			st:      &capStore{govHasGroupTier: true},
			wantErr: true,
		},
		{
			// PF-25: an explicitly named principal is fully determined.
			name:   "truncated, a USER row matches — served",
			groups: []string{"a-team"}, truncated: true,
			st:       &capStore{govProfile: govProfile("bob-only"), govTier: types.CapabilitySubjectUser, govHasGroupTier: true},
			wantProf: "bob-only",
		},
		{
			// PF-21 scoping: no group row anywhere ⇒ nothing hidden ⇒ serve.
			name:   "truncated, ALL row matches, ZERO group-tier rows — served",
			groups: []string{"a-team"}, truncated: true,
			st:       &capStore{govProfile: govProfile("everyone"), govTier: types.CapabilitySubjectAll, govHasGroupTier: false},
			wantProf: "everyone",
		},
		{
			name:   "truncated, nothing matches, ZERO group-tier rows — deployment ceiling",
			groups: []string{}, truncated: true,
			st:       &capStore{govHasGroupTier: false},
			wantProf: "",
		},
		{
			// Control: the same snapshot, NOT truncated, is served the group row.
			name:   "complete snapshot, group row matches — served (control)",
			groups: []string{"a-team"}, truncated: false,
			st:       &capStore{govProfile: govProfile("a-team-walled"), govTier: types.CapabilitySubjectGroup, govHasGroupTier: true},
			wantProf: "a-team-walled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := govServer(tc.st).effectiveCeiling(govMemberCtx(tc.groups, tc.truncated))
			if tc.wantErr {
				if !errors.Is(err, errGroupsSnapshotStale) {
					prof := "<deployment>"
					if err == nil && got.Profile != nil {
						prof = got.Profile.Name
					}
					t.Fatalf("err = %v (ceiling=%s); want errGroupsSnapshotStale — a truncated snapshot was SERVED", err, prof)
				}
				if ceilingErrorStatus(err) != http.StatusForbidden {
					t.Errorf("ceilingErrorStatus = %d, want 403", ceilingErrorStatus(err))
				}
				w := httptest.NewRecorder()
				writeCeilingError(w, err)
				if w.Code != http.StatusForbidden || !containsAll(w.Body.String(), "groups_snapshot_stale", "sign in again") {
					t.Errorf("writeCeilingError = %d %q; want 403 naming groups_snapshot_stale and the remedy", w.Code, w.Body.String())
				}
				return
			}
			if err != nil {
				t.Fatalf("effectiveCeiling: %v; want served", err)
			}
			gotProf := ""
			if got.Profile != nil {
				gotProf = got.Profile.Name
			}
			if gotProf != tc.wantProf {
				t.Errorf("profile = %q, want %q", gotProf, tc.wantProf)
			}
		})
	}
}

// TestF2_TruncatedSnapshotRefusedAtTheReadSite drives the refusal through HTTP
// at a ROUTED READ site (GET /policies/default) rather than the create path
// governance_nonescape_test.go row 16a already covers — the 403 mapping is one
// function (writeCeilingError) but every routed site has to actually call it.
func TestF2_TruncatedSnapshotRefusedAtTheReadSite(t *testing.T) {
	newSrv := func(t *testing.T, cs *capStore) (*Server, *govEscapeStore) {
		t.Helper()
		st := newGovEscapeStore(cs)
		cfg := baseTestConfig(newHarness(t), st)
		cfg.OIDC = &oidc.Authenticator{}
		cfg.DefaultPolicy = govDeployment()
		return New(cfg), st
	}

	t.Run("SSO cookie with the truncation bit set", func(t *testing.T) {
		srv, _ := newSrv(t, &capStore{govProfile: govProfile("everyone"), govTier: types.CapabilitySubjectAll, govHasGroupTier: true})
		w := doSSO(t, srv, http.MethodGet, "/api/v1/policies/default",
			govSession(t, "sub-many-groups", []string{"a-team"}, true), "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("GET /policies/default (truncated cookie) = %d, want 403: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "groups_snapshot_stale") {
			t.Errorf("403 body does not name the condition: %s", w.Body.String())
		}
		if strings.Contains(w.Body.String(), "wide.example") {
			t.Errorf("the DEPLOYMENT ceiling leaked into a refusal body: %s", w.Body.String())
		}
	})

	// GET /secrets is the site this file's own premise had not been applied to.
	// It resolved the ceiling (memberVisibleOperatorSecretNames) and mapped the
	// status with ceilingErrorStatus — the right 403 — but wrote
	// "list secrets: " + err.Error(), publishing the BARE sentinel. A member
	// read `groups_snapshot_stale` with no remedy, while every sibling seam
	// named one; `wardyn secret list` printed it verbatim and exited 2.
	//
	// Counterfactual: put the hand-pasted prefix back and the remedy assertion
	// fails while the 403 still passes — which is exactly how this shipped.
	t.Run("GET /secrets names the remedy, not just the sentinel", func(t *testing.T) {
		srv, _ := newSrv(t, &capStore{govHasGroupTier: true})
		srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}}
		srv.router = srv.routes() // re-mount with the secret surface enabled

		w := doSSO(t, srv, http.MethodGet, "/api/v1/secrets",
			govSession(t, "sub-many-groups", []string{"a-team"}, true), "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("GET /secrets (truncated cookie) = %d, want 403: %s", w.Code, w.Body.String())
		}
		if !containsAll(w.Body.String(), "groups_snapshot_stale", "sign in again") {
			t.Errorf("403 body = %s; want it to name the condition AND the remedy — a member has no vocabulary "+
				"for the bare sentinel and no documented way to clear it", w.Body.String())
		}
	})

	t.Run("API token with the NULL (pre-0.7) marker, then with the bit set", func(t *testing.T) {
		srv, st := newSrv(t, &capStore{govProfile: govProfile("everyone"), govTier: types.CapabilitySubjectAll, govHasGroupTier: true})
		st.tokenRaw = apiTokenPrefix + "f2probe"
		st.token = &types.APIToken{
			ID: uuid.New(), Principal: "sub-legacy-token", Email: "legacy@corp.example",
			Role: oidc.RoleMember, Groups: []string{"a-team"}, GroupsTruncated: nil, Name: "legacy",
		}
		call := func() *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/policies/default", nil)
			r.Header.Set("Authorization", "Bearer "+st.tokenRaw)
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			return w
		}
		if w := call(); w.Code != http.StatusForbidden {
			t.Fatalf("NULL-marker token = %d, want 403: %s", w.Code, w.Body.String())
		}
		truncated := true
		st.token.GroupsTruncated = &truncated
		if w := call(); w.Code != http.StatusForbidden {
			t.Fatalf("truncated=true token = %d, want 403: %s", w.Code, w.Body.String())
		}
		complete := false
		st.token.GroupsTruncated = &complete
		if w := call(); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"governance_profile_name":"everyone"`) {
			t.Fatalf("complete token = %d, want 200 under profile everyone: %s", w.Code, w.Body.String())
		}
	})
}
