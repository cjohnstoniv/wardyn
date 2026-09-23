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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCSRFGuard_LocalModeSiblingPortRefused (lens-S S-1).
//
// The LocalMode arm accepted ANY loopback Origin, so a page served by another
// process on 127.0.0.1:<other-port> could drive every mutating public-API route
// of a LocalMode daemon with no credential at all. Fetch Metadata does not
// close it — ports are not part of a SITE, so the browser labels that request
// same-site, not cross-site — and the POST needs no preflight (no handler reads
// Content-Type, so text/plain with a JSON body is a simple request).
//
// A loopback Origin must name THIS listener: host AND port.
func TestCSRFGuard_LocalModeSiblingPortRefused(t *testing.T) {
	for _, origin := range []string{
		"http://127.0.0.1:9999", // another process, another port
		"http://localhost:9999",
		"http://[::1]:9999",
		"http://127.0.0.1", // the same host on the scheme's default port
	} {
		t.Run(origin, func(t *testing.T) {
			w := csrfDo(t, csrfLocalServer(t), http.MethodPost, "/api/v1/auth/logout",
				csrfLocalHost, origin, "", "", nil)
			if w.Code != http.StatusForbidden {
				t.Fatalf("code = %d, want 403 — a loopback SIBLING origin is not this listener\nbody: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), csrfRefusedBody) {
				t.Errorf("body = %q, want the CSRF guard's own sentence", w.Body.String())
			}
		})
	}
	// The console's own fetches still pass: same host, same port.
	w := csrfDo(t, csrfLocalServer(t), http.MethodPost, "/api/v1/auth/logout",
		csrfLocalHost, "http://"+csrfLocalHost, "", "", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("same-origin local POST: code = %d, want 204\nbody: %s", w.Code, w.Body.String())
	}
}

// TestSameOriginOrRefuse_ASCIIFoldOnly (lens-S S-12). strings.EqualFold is
// Unicode SIMPLE CASE FOLDING: U+212A KELVIN SIGN folds equal to "k", so a host
// comparison written with it answers yes to a host that is not ours. Not
// browser-reachable (an Origin header is ASCII/punycode) — but a security
// comparison must do exactly what it says.
func TestSameOriginOrRefuse_ASCIIFoldOnly(t *testing.T) {
	post := func(host, origin string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		r.Host = host
		r.Header.Set("Origin", origin)
		return r
	}
	srv := &Server{}
	if err := srv.sameOriginOrRefuse(post("k.example", "https://K.example")); err == nil {
		t.Error("an Origin host differing from r.Host by a UNICODE case fold was accepted")
	}
	// The second accepted host takes the same comparison.
	srv2 := &Server{cfg: Config{OIDCRedirectURL: "https://k.example/auth/callback"}}
	if err := srv2.sameOriginOrRefuse(post("wardyn.internal:8080", "https://K.example")); err == nil {
		t.Error("the redirect-host arm accepted a UNICODE case fold")
	}
	// ASCII case-insensitivity is unchanged — a browser may send either case.
	if err := srv.sameOriginOrRefuse(post("K.EXAMPLE", "https://k.example")); err != nil {
		t.Errorf("ASCII case fold refused: %v", err)
	}
}

// TestAttachOrigin_RedirectHostIsNotAGlob (lens-S S-6).
//
// coder/websocket's OriginPatterns are path.Match GLOBS, not literals
// (authenticateOrigin: path.Match(lower(pattern), lower(u.Host))). Any *, ? or
// [ in the configured redirect host becomes a metacharacter — and an
// IPv6-LITERAL redirect URL always contains [...], so `https://[2001:db8::1]:8443`
// became a character class that ADMITTED the foreign, browser-reachable origin
// host `0:8443` and REFUSED the ingress host it was configured for.
func TestAttachOrigin_RedirectHostIsNotAGlob(t *testing.T) {
	const ingress = "https://[2001:db8::1]:8443/auth/callback"
	srv := csrfOIDCServer(t, ingress)
	attach := func(origin string) bool {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/x/attach", nil)
		r.Host = csrfOIDCHost
		r.Header.Set("Origin", origin)
		return srv.attachOriginRefused(r)
	}
	for _, foreign := range []string{"http://0:8443", "http://1:8443", "http://8:8443"} {
		if !attach(foreign) {
			t.Errorf("foreign origin %q accepted — the redirect host is being read as a glob", foreign)
		}
	}
	// And the configured ingress host itself still works, which the glob broke.
	if attach("https://[2001:db8::1]:8443") {
		t.Error("the configured IPv6 ingress host is refused by the attach socket")
	}
	// The same metacharacter question, without IPv6.
	star := csrfOIDCServer(t, "https://*.wardyn.example/auth/callback")
	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/x/attach", nil)
	r.Host = csrfOIDCHost
	r.Header.Set("Origin", "https://evil.wardyn.example")
	if !star.attachOriginRefused(r) {
		t.Error("a redirect host containing * admitted a whole subdomain")
	}
}

// TestAttachOrigin_LocalModeHasNoSecondName (review R-6). attachOriginRefused
// and the LocalMode REST arm must make the SAME decision, which is what
// originNamesThisDeployment's doc promises ("so 'which origins are us' cannot
// diverge"). LocalMode has no ingress and no IdP in front of it, so the
// redirect host is not a name it answers to — and a deployment running
// LocalMode WITH OIDC configured would otherwise let the attach socket accept
// an origin every REST route refuses.
func TestAttachOrigin_LocalModeHasNoSecondName(t *testing.T) {
	local := New(Config{
		Identity:      mustIDP(t),
		Approvals:     newFakeApprovals(),
		Broker:        &fakeBroker{},
		Audit:         &recRecorder{},
		LocalMode:     true,
		LocalOperator: "local:tester",
		DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
	})
	local.cfg.OIDCRedirectURL = csrfRedirectURL

	attach := func(srv *Server, origin string) bool {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/x/attach", nil)
		r.Host = csrfLocalHost
		r.Header.Set("Origin", origin)
		return srv.attachOriginRefused(r)
	}
	rest := func(srv *Server, origin string) bool {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
		r.Host = csrfLocalHost
		return !srv.originIsRequestHost(r, origin)
	}
	for _, origin := range []string{csrfRedirectHost, "http://127.0.0.1:9999", "https://evil.example"} {
		if a, b := attach(local, origin), rest(local, origin); a != b || !a {
			t.Errorf("Origin %q: attach refused=%v, REST refused=%v; want both true", origin, a, b)
		}
	}
	// The listener's own origin still works on both.
	if attach(local, "http://"+csrfLocalHost) {
		t.Error("LocalMode attach refused its own origin")
	}
}

// TestCSRFGuard_RefusalNamesTheCSRFBoundary (lens-S S-11). The auth.failed row's
// Actor names WHICH boundary refused (AUDIT-ACTIONS.md). adminAuth did not
// refuse this one — the session was VALID and the CSRF guard short-circuits
// above it — so the row claimed the wrong boundary.
func TestCSRFGuard_RefusalNamesTheCSRFBoundary(t *testing.T) {
	audit := &recRecorder{}
	srv := New(Config{
		Identity:      mustIDP(t),
		Approvals:     newFakeApprovals(),
		Broker:        &fakeBroker{},
		Audit:         audit,
		LocalMode:     true,
		LocalOperator: "local:tester",
		DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
	})
	if w := csrfDo(t, srv, http.MethodPost, "/api/v1/auth/logout", csrfLocalHost,
		"https://evil.example", "", "", nil); w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", w.Code)
	}
	for _, ev := range audit.events {
		if ev.Action != "auth.failed" {
			continue
		}
		if ev.Actor != "wardyn/csrf" {
			t.Errorf("auth.failed Actor = %q, want wardyn/csrf — adminAuth did not refuse this", ev.Actor)
		}
		return
	}
	t.Fatal("no auth.failed row for a CSRF refusal")
}

// TestAttachWS_CrossOriginRefusalIsAudited. The attach socket is the most
// dangerous cookie-authenticated capability in the product, so its
// cross-origin refusal must not be SILENT (403 and nothing in the trail): the
// REST guard emits auth.failed/cross_origin_refused/wardyn/csrf at both of
// its arms, and so does this one. Driven through the real router on the
// ?ticket= lane, which is the browser's own.
func TestAttachWS_CrossOriginRefusalIsAudited(t *testing.T) {
	srv, _, _, audit, run := holderTestServer(t)
	srv.cfg.OIDCRedirectURL = csrfRedirectURL

	tok, err := mintAttachTicket(context.Background(), srv.cfg.Store, run.ID,
		types.ActorHuman, holderOwner, oidc.RoleAdmin, time.Now())
	if err != nil {
		t.Fatalf("mint ticket: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet,
		"/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, nil)
	r.Host = csrfOIDCHost
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 (a cross-origin upgrade must not reach Accept)\nbody: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), csrfRefusedBody) {
		t.Errorf("body = %q, want the CSRF guard's own sentence", w.Body.String())
	}
	for _, ev := range audit.snapshot() {
		if ev.Action != "auth.failed" {
			continue
		}
		if ev.Actor != csrfActor {
			t.Errorf("auth.failed Actor = %q, want %q", ev.Actor, csrfActor)
		}
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("decode audit data: %v", err)
		}
		if data["reason"] != csrfAuditReason {
			t.Errorf("reason = %v, want %q", data["reason"], csrfAuditReason)
		}
		return
	}
	t.Fatalf("the attach socket refused a cross-origin upgrade and recorded NOTHING; events = %+v", audit.snapshot())
}

// TestAttachWS_CrossOriginIsRefusedBeforeTheStoreRead (V1-r2 lens-S2 S2-10).
//
// The origin check sat AFTER getRunOr404, the ticket re-check and the run-state
// checks, so a cross-origin upgrade still cost a store read and could emit
// authz.denied on its way to being refused for a different reason entirely. It
// is a decision about the CALLER, not about the run: it belongs beside the other
// caller gates, above everything that reads state. An unknown run id proves the
// ordering — 403, not 404.
func TestAttachWS_CrossOriginIsRefusedBeforeTheStoreRead(t *testing.T) {
	srv, _, _, audit, _ := holderTestServer(t)
	srv.cfg.OIDCRedirectURL = csrfRedirectURL

	r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+uuid.New().String()+"/attach", nil)
	r.Host = csrfOIDCHost
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Authorization", "Bearer "+adminToken)
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 — a cross-origin upgrade must be refused before the run is read\nbody: %s", w.Code, w.Body.String())
	}
	for _, ev := range audit.snapshot() {
		if ev.Action == "authz.denied" {
			t.Errorf("a cross-origin upgrade emitted %s before the origin was judged", ev.Action)
		}
	}
}
