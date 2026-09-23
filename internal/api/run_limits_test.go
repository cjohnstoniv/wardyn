// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunLimitsRefusal is the profile write boundary on the seven run limits
// (#567): every duration is 0..100 years, and a default may not sit past its
// own max. The 400 names the field.
func TestRunLimitsRefusal(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{govLimitsBody(`"max_end_ahead_sec":-1`), "limits.max_end_ahead_sec"},
		{govLimitsBody(`"default_end_sec":-1`), "limits.default_end_sec"},
		{govLimitsBody(`"max_wait_sec":-60`), "limits.max_wait_sec"},
		{govLimitsBody(`"default_wait_sec":-1`), "limits.default_wait_sec"},
		{govLimitsBody(`"pause_idle_after_sec":-1`), "limits.pause_idle_after_sec"},
		{govLimitsBody(`"max_end_ahead_sec":9223372036`), "limits.max_end_ahead_sec"},
		{govLimitsBody(`"max_end_ahead_sec":3600,"default_end_sec":7200`), "limits.default_end_sec: 7200 is past limits.max_end_ahead_sec (3600)"},
		{govLimitsBody(`"max_wait_sec":600,"default_wait_sec":601`), "limits.default_wait_sec: 601 is past limits.max_wait_sec (600)"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/governance/profiles", strings.NewReader(tc.body))
		if _, msg := decodeGovernanceProfileRequest(httptest.NewRecorder(), r); !strings.Contains(msg, tc.want) {
			t.Errorf("body %s: msg = %q, want it to contain %q", tc.body, msg, tc.want)
		}
	}

	// The controls: zero is unset, a default with no max is fine, and the seven
	// keys decode flat on `limits` (the embedded RunLimits) through the strict
	// decoder.
	for _, body := range []string{
		govLimitsBody(`"max_end_ahead_sec":0,"default_end_sec":0,"max_wait_sec":0,"default_wait_sec":0,"pause_idle_after_sec":0`),
		govLimitsBody(`"default_end_sec":86400,"default_wait_sec":3600`),
		govLimitsBody(`"max_end_ahead_sec":2592000,"default_end_sec":86400,"allow_no_end":true,` +
			`"max_wait_sec":28800,"default_wait_sec":28800,"user_changes_limits":true,"pause_idle_after_sec":1800`),
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/governance/profiles", strings.NewReader(body))
		req, msg := decodeGovernanceProfileRequest(httptest.NewRecorder(), r)
		if msg != "" {
			t.Errorf("body %s refused with %q", body, msg)
		}
		if strings.Contains(body, "user_changes_limits") && (!req.Limits.UserChangesLimits || req.Limits.MaxEndAheadSec != 2592000) {
			t.Errorf("body %s decoded to %+v; the flat keys did not reach RunLimits", body, req.Limits.RunLimits)
		}
	}
}

// TestRunWaitSec is the captured wait: the profile's default, else its max,
// else the deployment's approval expiry, never past the deployment's.
func TestRunWaitSec(t *testing.T) {
	const day = 24 * time.Hour
	for _, tc := range []struct {
		name       string
		l          types.RunLimits
		deployment time.Duration
		want       int
	}{
		{"no profile wait: the deployment's", types.RunLimits{}, day, 86400},
		{"the default", types.RunLimits{MaxWaitSec: 8 * 3600, DefaultWaitSec: 3600}, day, 3600},
		{"no default: the max", types.RunLimits{MaxWaitSec: 8 * 3600}, day, 8 * 3600},
		{"a max past the deployment folds under it", types.RunLimits{MaxWaitSec: 7 * 86400}, day, 86400},
		{"a default past the deployment folds under it", types.RunLimits{DefaultWaitSec: 3 * 86400}, day, 86400},
		{"no deployment expiry known: the profile's", types.RunLimits{DefaultWaitSec: 3 * 86400}, 0, 3 * 86400},
		{"nothing known", types.RunLimits{}, 0, 0},
	} {
		if got := runWaitSec(tc.l, tc.deployment); got != tc.want {
			t.Errorf("%s: runWaitSec = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// runLimitsFixture is quotaFixture with the store handed back, so a test can
// read the run row a create wrote, and the deployment's approval expiry set.
func runLimitsFixture(t *testing.T, cs *capStore) (*Server, *govEscapeStore) {
	t.Helper()
	h := newHarness(t)
	st := newGovEscapeStore(cs)
	cfg := baseTestConfig(h, st)
	cfg.Audit = &recRecorder{}
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{govCorpSecret: []byte("v")}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	cfg.ApprovalExpiryAfter = 24 * time.Hour
	return New(cfg), st
}

// createdRun posts one run and returns the row the store was handed.
func createdRun(t *testing.T, srv *Server, st *govEscapeStore, cookie *http.Cookie) types.AgentRun {
	t.Helper()
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, `{"agent":"claude-code","task":"t"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	var out types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.runs[out.ID]
}

// TestCreateRunCapturesRunLimits pins the capture at create (#567): the run row
// carries the OWNER's profile run limits, the profile id, an end at
// created_at + the default end and the wait folded under the deployment. A
// security admin is bounded like any member; only a super admin is exempt.
func TestCreateRunCapturesRunLimits(t *testing.T) {
	limits := types.RunLimits{
		MaxEndAheadSec: 30 * 86400, DefaultEndSec: 86400,
		MaxWaitSec: 8 * 3600, DefaultWaitSec: 3600, UserChangesLimits: true,
	}
	profile := limitsProfile("leased", types.GovernanceLimits{RunLimits: limits})
	wantBounded := func(t *testing.T, run types.AgentRun) {
		t.Helper()
		if run.RunLimits != limits {
			t.Errorf("run_limits = %+v, want the profile's %+v", run.RunLimits, limits)
		}
		if run.GovernanceProfileID == nil || *run.GovernanceProfileID != profile.ID {
			t.Errorf("governance_profile_id = %v, want %s", run.GovernanceProfileID, profile.ID)
		}
		if want := run.CreatedAt.Add(24 * time.Hour); run.EndsAt == nil || !run.EndsAt.Equal(want) {
			t.Errorf("ends_at = %v, want created_at + default_end_sec = %v", run.EndsAt, want)
		}
		if run.WaitBudgetSec != 3600 {
			t.Errorf("wait_budget_sec = %d, want the profile default 3600", run.WaitBudgetSec)
		}
	}
	wantDeploymentOnly := func(t *testing.T, run types.AgentRun) {
		t.Helper()
		if run.EndsAt != nil || run.GovernanceProfileID != nil || run.RunLimits != (types.RunLimits{}) {
			t.Errorf("run = ends %v profile %v limits %+v; want no end, no profile, no limits",
				run.EndsAt, run.GovernanceProfileID, run.RunLimits)
		}
		if run.WaitBudgetSec != 86400 {
			t.Errorf("wait_budget_sec = %d, want the deployment's approval expiry (86400)", run.WaitBudgetSec)
		}
	}

	t.Run("a member under a profile", func(t *testing.T) {
		srv, st := runLimitsFixture(t, assignedStore(profile))
		wantBounded(t, createdRun(t, srv, st, govSession(t, govMemberSub, []string{"eng"}, false)))
	})

	t.Run("a security admin under a profile is bounded too", func(t *testing.T) {
		srv, st := runLimitsFixture(t, &capStore{govProfile: profile, govTier: types.CapabilitySubjectUser})
		wantBounded(t, createdRun(t, srv, st,
			ssoSession(t, "sub-secadmin", "secadmin@corp.example", oidc.RoleSecurityAdmin)))
	})

	t.Run("a super admin is exempt", func(t *testing.T) {
		srv, st := runLimitsFixture(t, assignedStore(profile))
		wantDeploymentOnly(t, createdRun(t, srv, st,
			ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)))
	})

	t.Run("an unassigned member keeps today: no end, the deployment's wait", func(t *testing.T) {
		srv, st := runLimitsFixture(t, &capStore{})
		wantDeploymentOnly(t, createdRun(t, srv, st, govSession(t, "sub-plain", []string{"eng"}, false)))
	})

	t.Run("a max with no default ends the run at the max", func(t *testing.T) {
		p := limitsProfile("max-only", types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: 7 * 86400}})
		srv, st := runLimitsFixture(t, assignedStore(p))
		run := createdRun(t, srv, st, govSession(t, govMemberSub, []string{"eng"}, false))
		if want := run.CreatedAt.Add(7 * 24 * time.Hour); run.EndsAt == nil || !run.EndsAt.Equal(want) {
			t.Errorf("ends_at = %v, want created_at + max_end_ahead_sec = %v", run.EndsAt, want)
		}
	})
}
