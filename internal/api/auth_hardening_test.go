// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── FIX #6: sign-out actually terminates the OIDC session ───────────────────────

// TestLogoutRouteMountedClearsSession is the regression for FIX #6. The UI POSTs
// /api/v1/auth/logout, but the OIDC logout used to be mounted ONLY as a root
// GET /auth/logout, so the POST hit no route (404), the HttpOnly wardyn_session
// cookie survived, and the next probe silently re-signed the operator in. The
// POST must now be routed (not 404) when OIDC is configured, invoke the OIDC
// LogoutHandler, and clear the session cookie.
//
// A zero-value *oidc.Authenticator is a valid test double here: its Middleware
// finds no session cookie and falls through to the admin-token path, and its
// LogoutHandler only clears the cookie + redirects (no field deref).
func TestLogoutRouteMountedClearsSession(t *testing.T) {
	srv := New(Config{
		Identity:      mustIDP(t),
		Approvals:     newFakeApprovals(),
		Broker:        &fakeBroker{},
		Audit:         &recRecorder{},
		AdminToken:    adminToken,
		OIDC:          &oidc.Authenticator{},
		DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
	})

	w := do(t, srv, http.MethodPost, "/api/v1/auth/logout", adminToken, "")
	if w.Code == http.StatusNotFound {
		t.Fatalf("POST /api/v1/auth/logout = 404; route not mounted (FIX #6 regressed)")
	}
	if w.Code != http.StatusFound {
		t.Fatalf("logout status = %d, want 302 (OIDC LogoutHandler redirect)", w.Code)
	}
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("logout did not clear the HttpOnly wardyn_session cookie")
	}
}

// TestLogoutTokenModeNoOp verifies the nil-OIDC guard: in admin-token / local mode
// there is no server-side session to kill, so the POST is a 204 no-op and never
// panics on a nil OIDC authenticator.
func TestLogoutTokenModeNoOp(t *testing.T) {
	h := newHarness(t) // OIDC nil, AdminToken set
	w := do(t, h.srv, http.MethodPost, "/api/v1/auth/logout", adminToken, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("token-mode logout status = %d, want 204 (no session to kill)", w.Code)
	}
}

// ─── FIX #8: local-mode Host allowlist (DNS-rebinding defense) ────────────────────

// TestLocalModeRejectsNonLoopbackHost is the regression for FIX #8. In local
// no-auth mode humanOrAdminAuth bypasses all auth, so a DNS-rebinding page
// (Origin==Host==attacker.com rebound to 127.0.0.1) could drive the no-auth
// surface with no credential. The bypass must reject any non-loopback Host with
// 403 BEFORE it fires, and accept only loopback Hosts.
func TestLocalModeRejectsNonLoopbackHost(t *testing.T) {
	srv := New(Config{
		Identity:      mustIDP(t),
		Approvals:     newFakeApprovals(),
		Broker:        &fakeBroker{},
		Audit:         &recRecorder{},
		LocalMode:     true,
		LocalOperator: "local:tester",
		DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
	})

	// Non-loopback Host: rejected 403 before the no-auth bypass. (Peer is loopback
	// so this isolates the Host gate — the N1 peer gate is exercised separately.)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Host = "attacker.com"
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-loopback Host: code = %d, want 403", w.Code)
	}

	// Loopback Hosts: pass the gate (200 from handleMe, which needs no Store).
	// Covers the literal name, 127.0.0.0/8, ::1, and the :port forms.
	for _, host := range []string{"127.0.0.1", "127.0.0.1:8080", "localhost:3000", "[::1]:8080", "127.0.0.2"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.Host = host
		req.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("loopback Host %q: code = %d, want 200 (should pass the gate)", host, w.Code)
		}
	}
}

// TestLocalModeRejectsNonLoopbackRemoteAddr is the regression for N1. The Host
// header is forgeable by a direct socket client, so a LAN peer hitting a 0.0.0.0
// bind with "Host: 127.0.0.1" would otherwise reach the auth-bypassed surface. The
// bypass must ALSO require a loopback TCP peer (r.RemoteAddr) — which the server
// sets from the accepted connection and a remote client cannot spoof.
func TestLocalModeRejectsNonLoopbackRemoteAddr(t *testing.T) {
	srv := New(Config{
		Identity:      mustIDP(t),
		Approvals:     newFakeApprovals(),
		Broker:        &fakeBroker{},
		Audit:         &recRecorder{},
		LocalMode:     true,
		LocalOperator: "local:tester",
		DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
	})

	// Non-loopback peer forging a loopback Host: rejected 403 (this is the N1 attack).
	for _, peer := range []string{"192.168.1.50:44444", "10.0.0.7:1234", "[2001:db8::1]:80"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.Host = "127.0.0.1" // forged — passes the Host gate
		req.RemoteAddr = peer
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("non-loopback peer %q with forged loopback Host: code = %d, want 403", peer, w.Code)
		}
	}

	// Loopback peer + loopback Host: passes (the legitimate local UI/CLI path).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("loopback peer + loopback Host: code = %d, want 200 (legit local path)", w.Code)
	}
}

// TestLocalTrustForwarderAllowsGatewayPeer is the regression for the compose fix:
// with LocalTrustForwarder set (the compose topology — wardynd bound 0.0.0.0 behind
// a loopback-only publish, so the peer is always the docker gateway), a non-loopback
// peer must PASS the peer gate, while the Host gate still rejects DNS-rebinding.
func TestLocalTrustForwarderAllowsGatewayPeer(t *testing.T) {
	srv := New(Config{
		Identity:            mustIDP(t),
		Approvals:           newFakeApprovals(),
		Broker:              &fakeBroker{},
		Audit:               &recRecorder{},
		LocalMode:           true,
		LocalTrustForwarder: true,
		LocalOperator:       "local:tester",
		DefaultPolicy:       types.RunPolicySpec{MinConfinementClass: types.CC2},
	})

	// Docker gateway peer + loopback Host (the legit compose host→container path): passes.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "172.18.0.1:44444" // docker bridge gateway, non-loopback
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("trusted-forwarder gateway peer: code = %d, want 200", w.Code)
	}

	// DNS-rebinding still blocked even with the forwarder trusted: the Host gate fires.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Host = "attacker.com"
	req.RemoteAddr = "172.18.0.1:44444"
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("trusted-forwarder + rebinding Host: code = %d, want 403 (Host gate must still fire)", w.Code)
	}
}

// TestIsLoopbackRemoteAddr unit-checks the peer helper: loopback peers pass, LAN/
// public/malformed peers fail closed.
func TestIsLoopbackRemoteAddr(t *testing.T) {
	pass := []string{"127.0.0.1:54321", "127.0.0.2:9", "[::1]:8080"}
	for _, a := range pass {
		if !isLoopbackRemoteAddr(a) {
			t.Errorf("isLoopbackRemoteAddr(%q) = false, want true", a)
		}
	}
	fail := []string{"192.168.1.1:8080", "10.0.0.5:22", "[2001:db8::1]:80", "attacker.com:80", "", "garbage"}
	for _, a := range fail {
		if isLoopbackRemoteAddr(a) {
			t.Errorf("isLoopbackRemoteAddr(%q) = true, want false", a)
		}
	}
}

// TestIsLoopbackHost unit-checks the helper directly: loopback names/IPs pass,
// public names and rebinding tricks (127.0.0.1.attacker.com) fail.
func TestIsLoopbackHost(t *testing.T) {
	pass := []string{"127.0.0.1", "127.0.0.1:8080", "localhost", "localhost:3000", "LocalHost", "::1", "[::1]", "[::1]:8080", "127.0.0.2:9"}
	for _, h := range pass {
		if !isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = false, want true", h)
		}
	}
	fail := []string{"attacker.com", "attacker.com:80", "127.0.0.1.attacker.com", "10.0.0.5", "192.168.1.1:8080", "", "evil-localhost.com"}
	for _, h := range fail {
		if isLoopbackHost(h) {
			t.Errorf("isLoopbackHost(%q) = true, want false", h)
		}
	}
}
