// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The install-side onboarding mark. Three browser flags carried this state
// before and each shipped a real bug (a wiped database kept skipping its own
// funnel; 127.0.0.1 and localhost disagreed about whether onboarding had
// happened), so the contract under test is exactly the two properties the
// server version exists for: the mark lives in the STORE, and nothing a
// client round-trips can erase it.

func TestOnboardingComplete_MarksInstallOnce(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, audit := newSiteConfigHarness(t, fake)

	w := do(t, srv, http.MethodPost, "/api/v1/setup/onboarding-complete", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen == nil || fake.putSeen.OnboardingCompletedAt == nil {
		t.Fatalf("completion must persist OnboardingCompletedAt via the store, got %+v", fake.putSeen)
	}
	first := *fake.putSeen.OnboardingCompletedAt
	if len(audit.events) != 1 || audit.events[0].Action != "setup.onboarding.completed" {
		t.Fatalf("want exactly one setup.onboarding.completed audit event, got %+v", audit.events)
	}

	// Idempotent: the FIRST completion is the fact of record. A re-finish
	// answers 200 without moving the timestamp or re-auditing.
	fake.cfg.OnboardingCompletedAt = &first
	fake.putSeen = nil
	w = do(t, srv, http.MethodPost, "/api/v1/setup/onboarding-complete", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("second call: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen != nil {
		t.Fatalf("second call must not write, got %+v", fake.putSeen)
	}
	if len(audit.events) != 1 {
		t.Fatalf("second call must not re-audit, got %d events", len(audit.events))
	}
}

func TestPutSiteConfig_CannotTouchOnboardingState(t *testing.T) {
	stamped := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{OnboardingCompletedAt: &stamped}}
	srv, _ := newSiteConfigHarness(t, fake)

	// A client that tries to SET it is told why, not silently ignored.
	w := do(t, srv, http.MethodPut, "/api/v1/site-config",
		adminToken, `{"onboarding_completed_at":"2026-01-01T00:00:00Z"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("client-supplied mark: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}

	// The Integrations footgun, replayed for this field: an older client GETs a
	// config, strips fields it does not know, PUTs it back — the stored mark
	// must survive the round-trip.
	w = do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"scm_hosts":["gitlab.corp"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("round-trip PUT: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen == nil || fake.putSeen.OnboardingCompletedAt == nil ||
		!fake.putSeen.OnboardingCompletedAt.Equal(stamped) {
		t.Fatalf("round-trip PUT must carry the stored mark forward, got %+v", fake.putSeen)
	}

	// And the status wire reports the flattened bit. setup/status touches more
	// of store.Store than the site-config fake implements (the embedded-nil
	// panic the store's own interface docs warn test doubles about), so this
	// leg gets a fake that also answers ListRuns.
	full := &fakeOnboardingStatusStore{fakeSiteConfigStore{cfg: types.SiteConfig{OnboardingCompletedAt: &stamped}}}
	h := newHarness(t)
	srv2 := New(baseTestConfig(h, full))
	var st struct {
		OnboardingComplete bool `json:"onboarding_complete"`
	}
	w = do(t, srv2, http.MethodGet, "/api/v1/setup/status", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("setup/status: code = %d; body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.OnboardingComplete {
		t.Fatalf("setup/status must report onboarding_complete=true, body=%s", w.Body.String())
	}
}

// fakeOnboardingStatusStore = the site-config fake plus the one extra method
// GET /setup/status reaches on this path. Kept beside the test that needs it
// rather than widening the shared fake for everyone.
type fakeOnboardingStatusStore struct {
	fakeSiteConfigStore
}

func (s *fakeOnboardingStatusStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	return nil, nil
}

// The permissions-posture row's guard checks err AFTER the call, so an
// unimplemented embedded method panics before the guard can skip it.
func (s *fakeOnboardingStatusStore) GetCapabilityEnforcement(context.Context) (map[string]bool, error) {
	return nil, context.Canceled
}
