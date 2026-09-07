// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

	// A client that tries to SET it does not get to: the write succeeds (the
	// document's other fields are the point of the request) and the submitted
	// mark is dropped, never persisted. The invariant is "cannot SET, CLEAR or
	// MOVE the mark", which the carry-forward enforces — the 400 that used to
	// stand here enforced nothing extra and broke two documented recovery flows
	// (TestPutSiteConfig_IgnoresASubmittedMarkOnTheRecoveryFlows).
	w := do(t, srv, http.MethodPut, "/api/v1/site-config",
		adminToken, `{"onboarding_completed_at":"2026-01-01T00:00:00Z"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("client-supplied mark: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen == nil || fake.putSeen.OnboardingCompletedAt == nil ||
		!fake.putSeen.OnboardingCompletedAt.Equal(stamped) {
		t.Fatalf("a client-supplied mark must never reach the store; the stored one must be carried forward, got %+v", fake.putSeen)
	}
	if !putSiteConfigIgnoredMark(t, w) {
		t.Fatalf("a dropped mark must be REPORTED, not silently swallowed; body=%s", w.Body.String())
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

// putSiteConfigIgnoredMark reads PUT /site-config's onboarding_completed_at_ignored
// signal off a recorded response (siteConfigPutResponse, site_config.go).
func putSiteConfigIgnoredMark(t *testing.T, w *httptest.ResponseRecorder) bool {
	t.Helper()
	var body struct {
		OnboardingCompletedAtIgnored bool `json:"onboarding_completed_at_ignored"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode PUT response: %v; body=%s", err, w.Body.String())
	}
	return body.OnboardingCompletedAtIgnored
}

// TestPutSiteConfig_IgnoresASubmittedMarkOnTheRecoveryFlows pins the residual an
// "echo the stored value or be refused" gate left behind. The stamp is
// idempotent and never moves (handleOnboardingComplete returns early once a mark
// exists), so a client echoing its own GET can never present a different
// instant: the ONLY bodies that named one were the two documented recovery
// flows, and both were 400ed.
//
//  1. The MDM-delivered /etc/wardyn/site-config.json (docs/DESKTOP.md), which
//     deploy/desktop/wardyn-desktop.sh re-applies on EVERY boot: it carries the
//     mark of the machine it was captured from, and the moment the laptop
//     finishes its own funnel that instant differs — from that boot on, the
//     corporate proxy/redirect config silently stopped re-applying (the script
//     warns and continues).
//  2. capture / `make reset` / re-onboard / apply, where the captured baseline
//     names the install's PREVIOUS mark.
//
// Both must be accepted, must leave the server's own mark exactly where it was,
// and must SAY that the file's copy was dropped.
func TestPutSiteConfig_IgnoresASubmittedMarkOnTheRecoveryFlows(t *testing.T) {
	captured := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	held := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	body := `{"scm_hosts":["gitlab.corp"],"onboarding_completed_at":"2026-08-30T12:00:00Z"}`

	for _, c := range []struct{ name, body string }{
		{"the MDM file re-applied on a laptop that finished its own funnel", body},
		{"the pre-reset baseline applied after re-onboarding", body},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeSiteConfigStore{cfg: types.SiteConfig{OnboardingCompletedAt: &held}}
			srv, _ := newSiteConfigHarness(t, fake)
			w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, c.body)
			if w.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if fake.putSeen == nil || fake.putSeen.OnboardingCompletedAt == nil ||
				!fake.putSeen.OnboardingCompletedAt.Equal(held) {
				t.Fatalf("the install's OWN mark must be carried forward untouched, got %+v", fake.putSeen)
			}
			if len(fake.putSeen.ScmHosts) != 1 || fake.putSeen.ScmHosts[0] != "gitlab.corp" {
				t.Fatalf("the corporate config in the file must still land, got %+v", fake.putSeen)
			}
			if !putSiteConfigIgnoredMark(t, w) {
				t.Fatalf("the dropped mark must be reported (the CLI prints it as a warning); body=%s", w.Body.String())
			}
		})
	}

	// Sub-second drift is the same case wearing a disguise: a client that
	// round-trips through a serialiser with microsecond precision echoes a
	// TRUNCATED copy of a nanosecond-precision mark. Accepted, dropped, and
	// reported — never refused.
	t.Run("a truncated echo of a nanosecond-precision mark", func(t *testing.T) {
		nanos := time.Date(2026, 8, 30, 12, 0, 0, 123456789, time.UTC)
		fake := &fakeSiteConfigStore{cfg: types.SiteConfig{OnboardingCompletedAt: &nanos}}
		srv, _ := newSiteConfigHarness(t, fake)
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
			`{"onboarding_completed_at":"2026-08-30T12:00:00.123456Z"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if fake.putSeen == nil || fake.putSeen.OnboardingCompletedAt == nil ||
			!fake.putSeen.OnboardingCompletedAt.Equal(nanos) {
			t.Fatalf("the stored mark must survive to the nanosecond, got %+v", fake.putSeen)
		}
	})

	// An exact echo is not a dropped value, so it must NOT raise the warning —
	// otherwise every ordinary console save (which spreads the GET document)
	// would print one.
	t.Run("an exact echo reports nothing", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: types.SiteConfig{OnboardingCompletedAt: &captured}}
		srv, _ := newSiteConfigHarness(t, fake)
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, body)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if putSiteConfigIgnoredMark(t, w) {
			t.Fatalf("an echo of the stored mark drops nothing and must report nothing; body=%s", w.Body.String())
		}
	})
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

// TestPutSiteConfig_GetBodyRoundTripsVerbatim is the contract handlePutSiteConfig's
// own doc states — "the caller must round-trip a GET first to preserve fields it
// does not intend to change" — asserted on the ONE body that must always work:
// the exact bytes GET emitted. onboarding_completed_at is a plain field of the
// same types.SiteConfig GET serialises, so on any install whose operator has
// finished the funnel that key IS in the GET body, and refusing it refused
// `wardyn site-config get > f` / `wardyn site-config apply f` (the documented
// disaster-recovery round-trip, docs/OPERATIONS.md) and every console save,
// which builds its PUT by spreading the GET document. Echoing the stored value
// is not an attempt to set it — and neither is naming a different instant: the
// carry-forward makes any submitted value inert, so the leg above asserts 200
// plus onboarding_completed_at_ignored rather than a 400.
func TestPutSiteConfig_GetBodyRoundTripsVerbatim(t *testing.T) {
	stamped := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{
		ScmHosts:              []string{"gitlab.corp"},
		OnboardingCompletedAt: &stamped,
	}}
	srv, _ := newSiteConfigHarness(t, fake)

	g := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
	if g.Code != http.StatusOK {
		t.Fatalf("GET: code = %d, want 200; body=%s", g.Code, g.Body.String())
	}
	captured := g.Body.String()
	if !strings.Contains(captured, "onboarding_completed_at") {
		t.Fatalf("GET must emit onboarding_completed_at on a completed install (that is the whole footgun); body=%s", captured)
	}

	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, captured)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT of the GET body verbatim: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fake.putSeen == nil || fake.putSeen.OnboardingCompletedAt == nil ||
		!fake.putSeen.OnboardingCompletedAt.Equal(stamped) {
		t.Fatalf("the round-trip must carry the stored mark forward, got %+v", fake.putSeen)
	}
	if len(fake.putSeen.ScmHosts) != 1 || fake.putSeen.ScmHosts[0] != "gitlab.corp" {
		t.Fatalf("the round-trip must persist the rest of the document, got %+v", fake.putSeen)
	}

	// The console's save shape: spread the GET document, change one field, PUT.
	var spread map[string]any
	if err := json.Unmarshal([]byte(captured), &spread); err != nil {
		t.Fatal(err)
	}
	spread["upstream_proxy_url"] = "http://proxy.corp.internal:3128"
	edited, err := json.Marshal(spread)
	if err != nil {
		t.Fatal(err)
	}
	w = do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, string(edited))
	if w.Code != http.StatusOK {
		t.Fatalf("console-shaped save: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// The CROSS-RESET leg, which is the flow the round-trip exists for and the
	// one an echo-only gate still refused: `make reset` takes the site config
	// with the volume, so the captured document is applied against a store whose
	// mark is NIL — as is the MDM-delivered /etc/wardyn/site-config.json landing
	// on a fresh machine (docs/DESKTOP.md). Same bytes, empty store.
	fresh := &fakeSiteConfigStore{}
	srvFresh, _ := newSiteConfigHarness(t, fresh)
	w = do(t, srvFresh, http.MethodPut, "/api/v1/site-config", adminToken, captured)
	if w.Code != http.StatusOK {
		t.Fatalf("apply of the captured document after a reset: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if fresh.putSeen == nil || fresh.putSeen.OnboardingCompletedAt != nil {
		t.Fatalf("a fresh install must not inherit the captured mark (the setup flow owns it), got %+v", fresh.putSeen)
	}
	if len(fresh.putSeen.ScmHosts) != 1 || fresh.putSeen.ScmHosts[0] != "gitlab.corp" {
		t.Fatalf("the corporate baseline itself must land after a reset, got %+v", fresh.putSeen)
	}
}
