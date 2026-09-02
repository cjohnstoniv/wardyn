// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0
//
// F12-ui-sandbox-origin-isolation — RUNNABLE PROBE (do NOT auto-run in the
// read-only lane; the coordinator runs it serially).
//
// INTENDED DESTINATION (copy here to run):
//   internal/api/uigateway_origin_isolation_probe_test.go
//
// It reuses the in-package harness from internal/api/uigateway_test.go
// (newUIHarness, uiHarness, uiMemStore, okBackend, the sshFakeRunner) and the
// gateway's own exported-to-package symbols (uiCookieName, uiRunPrefix,
// encodeUISession, uiSession). It therefore ONLY compiles when dropped into
// package api alongside uigateway_test.go — it is not standalone.
//
// RUN (no Postgres — the harness fakes the store):
//   cp local/review-0.7/deep/F12-ui-sandbox-origin-isolation/uigateway_origin_isolation_probe_test.go \
//      internal/api/uigateway_origin_isolation_probe_test.go
//   nice -n 10 GOMAXPROCS=8 go test ./internal/api/ -run 'F12Probe' -race -count=1 -v
//   # (no WARDYN_TEST_PG DSN needed: this surface's tests run against the fake store)
//
// WHAT IT PINS (the traced invariant, both halves):
//   1. Origin isolation across runs: a VALID relay cookie minted for run A is
//      REFUSED (403) on run B's live relay path — even though run B exists, is
//      RUNNING, and declares the same app. This is the server-side backstop for
//      the browser's path-scoped cookie: handleUIRelay's `sess.Run != runID`
//      guard (internal/api/uigateway.go:403). If that guard regresses, one run's
//      session reaches another run's sandbox and the test FAILS.
//   2. Cookie-tossing outbound: a sandbox response whose Set-Cookie uses a
//      wardyn_* name (here wardyn_ui_sess, an authentication-attack cookie) is
//      DROPPED before it reaches the operator's browser, while the app's own
//      cookie survives — uiStripOutbound (internal/api/uigateway.go:526). If the
//      strip regresses, the sandbox can overwrite the relay session cookie and
//      the test FAILS.

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestF12Probe_CookieForRunAIsRefusedOnRunBPath is the origin-isolation pin.
// The existing TestUIGateway_RelayRequiresAValidSessionForThisRun covers a
// cookie for a RANDOM (non-existent) run used on the real run's path; this is
// the complementary, higher-value case the campaign should not miss: run B is a
// REAL, RUNNING run that declares the very same app, so the ONLY thing standing
// between run A's cookie and run B's sandbox is the run-id match — exactly the
// invariant.
func TestF12Probe_CookieForRunAIsRefusedOnRunBPath(t *testing.T) {
	h := newUIHarness(t, okBackend())
	runA := h.run.ID

	// A second real run (run B): RUNNING, owned by the same operator, declaring
	// an identical "code" app on the same port. Nothing about run B is special —
	// that is the point: only the run id in the signed cookie differs.
	runB := types.AgentRun{ID: uuid.New(), CreatedBy: h.owner, State: types.RunRunning, SandboxRef: "sandbox-B"}
	h.store.putRun(runB)
	h.store.putEffectivePolicy(runB.ID, types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		UIApps:              []types.UIApp{{Name: "code", Port: uiTestPort, Path: "/ide"}},
	})

	// A fully valid, unexpired, correctly-signed cookie for run A.
	cookieA := &http.Cookie{Name: uiCookieName, Value: h.srv.encodeUISession(uiSession{
		Run: runA, App: "code", Port: uiTestPort, Principal: h.owner, Role: oidc.RoleMember,
		Expires: time.Now().Add(time.Hour).Unix(),
	})}

	// Sanity: the same cookie MUST work on run A's own path, or the negative
	// result below would be meaningless (it must fail for the RIGHT reason).
	reqA := httptest.NewRequest(http.MethodGet, uiRunPrefix+runA.String()+"/ide", nil)
	reqA.AddCookie(cookieA)
	recA := httptest.NewRecorder()
	h.gateway.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("control: run A's cookie on run A's path got %d %s, want 200", recA.Code, recA.Body.String())
	}

	// The probe: run A's cookie on run B's live relay path.
	reqB := httptest.NewRequest(http.MethodGet, uiRunPrefix+runB.ID.String()+"/ide", nil)
	reqB.AddCookie(cookieA)
	recB := httptest.NewRecorder()
	h.gateway.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusForbidden {
		t.Fatalf("ISOLATION BREACH: run A's cookie was honored on run B's path — got %d %s, want 403",
			recB.Code, recB.Body.String())
	}
	if strings.Contains(recB.Body.String(), "sandbox app") {
		t.Fatalf("ISOLATION BREACH: run B's sandbox body was relayed under run A's session")
	}
}

// TestF12Probe_SandboxWardynSetCookieIsStrippedOutbound is the cookie-tossing
// pin. A sandbox app that sets Set-Cookie: wardyn_ui_sess=... is trying to
// pin/overwrite the relay session cookie in the operator's browser — an
// authentication attack. It must never reach the browser; the app's own
// Set-Cookie must survive.
func TestF12Probe_SandboxWardynSetCookieIsStrippedOutbound(t *testing.T) {
	h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "wardyn_ui_sess=attacker-chosen; Path=/")
		w.Header().Add("Set-Cookie", "app_theme=dark; Path=/")
		_, _ = io.WriteString(w, "ok")
	}))
	cookie := h.openSession()

	rec := h.relay("/ide", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("relay: %d %s", rec.Code, rec.Body.String())
	}
	for _, sc := range rec.Header().Values("Set-Cookie") {
		if strings.HasPrefix(strings.ToLower(sc), uiCookiePrefix) {
			t.Fatalf("COOKIE TOSS: sandbox set a wardyn_* cookie in the browser: %q", sc)
		}
	}
	// The app's own cookie must have survived — the strip is a scalpel, not a
	// blanket "drop all Set-Cookie".
	kept := rec.Header().Values("Set-Cookie")
	if len(kept) != 1 || !strings.HasPrefix(kept[0], "app_theme=dark") {
		t.Fatalf("app cookie not preserved: %v", kept)
	}
}
