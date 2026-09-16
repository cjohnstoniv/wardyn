// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The relay SESSION's own tests: which app a cookie is for (B3-F1 + B3-F8) and
// how long, and under whose authority, it keeps working (B3-F5). The transport
// matrix — header hygiene, launcher exit codes, the connection cap — lives in
// uigateway_test.go and is untouched by these.
package api

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// uiRevocations is an oidc.SessionRevocations double whose answer a test flips
// mid-flight, which is what a real revoke looks like to the gateway: nothing
// about the cookie changes, the STORE's answer does.
type uiRevocations struct {
	mu      sync.Mutex
	revoked bool
	asked   []string
}

func (r *uiRevocations) IsSessionRevoked(_ context.Context, sub, _ string, _ time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, sub)
	return r.revoked, nil
}
func (r *uiRevocations) RevokeSub(context.Context, string) error { return nil }
func (r *uiRevocations) RevokeAll(context.Context) error         { return nil }
func (r *uiRevocations) revoke() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revoked = true
}
func (r *uiRevocations) consulted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asked) > 0
}

var _ oidc.SessionRevocations = (*uiRevocations)(nil)

// errUIRevocations is the store that cannot answer — a Postgres outage, from
// the gateway's point of view.
type errUIRevocations struct{}

func (errUIRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return false, errors.New("revocation store unavailable")
}
func (errUIRevocations) RevokeSub(context.Context, string) error { return nil }
func (errUIRevocations) RevokeAll(context.Context) error         { return nil }

var _ oidc.SessionRevocations = errUIRevocations{}

// closingBackend answers with Connection: close so net/http never pools the
// relay connection — every request therefore reaches uiDial, which is where the
// per-connection re-checks live. Without this a second request would ride the
// first one's idle connection and prove nothing about a NEW one.
func closingBackend(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, body)
	})
}

// echoAfterUpgradeBackend answers ONE request with 101 and then echoes whatever
// the client sends for as long as the test holds it — an editor's live-reload
// socket, in miniature, that a test can prove is still carrying bytes.
func echoAfterUpgradeBackend(t *testing.T, held <-chan struct{}) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("backend ResponseWriter is not a Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("backend hijack: %v", err)
			return
		}
		defer conn.Close()
		if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")); err != nil {
			t.Errorf("backend write 101: %v", err)
			return
		}
		done := make(chan struct{})
		go func() { defer close(done); _, _ = io.Copy(conn, conn) }()
		select {
		case <-held:
		case <-done:
		}
	})
}

// uiEnterApp runs the handoff for one app and returns its Set-Cookie and the
// Location it redirected to.
func uiEnterApp(t *testing.T, h *uiHarness, app string) (*http.Cookie, string) {
	t.Helper()
	rec := h.enter(url.Values{
		"run": {h.run.ID.String()}, "app": {app},
		"ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleMember)},
	})
	if rec.Code != http.StatusFound {
		t.Fatalf("enter %q: %d %s", app, rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == uiCookieName {
			return c, rec.Header().Get("Location")
		}
	}
	t.Fatalf("enter %q set no relay cookie", app)
	return nil, ""
}

// uiGet issues one relay request at an explicit path — these tests are about
// the PATH, so they never go through the harness's path builder.
func uiGet(h *uiHarness, path string, c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.gateway.ServeHTTP(rec, req)
	return rec
}

// ─── B3-F1 + B3-F8: one cookie per app, and a canonical run id ───────────────

// TestUIGateway_SecondAppDoesNotHijackTheFirstApp pins B3-F1. The relay cookie
// was keyed per RUN (one name, Path=/r/<run>/) but pinned ONE app and port, so
// entering a second declared app on the same run OVERWROTE the first app's
// cookie in the browser: the still-open first tab's XHR then dialed the second
// app's port, and ui.open/ui.close named the wrong app. Two cookies can only
// coexist in a browser if their Paths differ — that is the assertion, and the
// server-side half is that one app's session is refused on the other's path.
func TestUIGateway_SecondAppDoesNotHijackTheFirstApp(t *testing.T) {
	const docsPort = 3000
	docs := httptest.NewServer(closingBackend("docs app"))
	defer docs.Close()

	h := newUIHarness(t, closingBackend("code app"))
	h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		UIApps: []types.UIApp{
			{Name: "code", Port: uiTestPort, Path: "/ide"},
			{Name: "docs", Port: docsPort, Path: "/readme"},
		},
	})
	// Route the fake socat by the port it was asked for, so "which app's port
	// did this request reach" is observable.
	var dialedMu sync.Mutex
	var dialed []int
	h.runner.execFn = func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		switch spec.Argv[0] {
		case "sh":
			return &runner.ExecSession{Wait: func() (int, error) { return 0, nil }}, nil
		case "socat":
			target, port := h.backend.URL, uiTestPort
			if strings.Contains(strings.Join(spec.Argv, " "), fmt.Sprintf(":%d", docsPort)) {
				target, port = docs.URL, docsPort
			}
			dialedMu.Lock()
			dialed = append(dialed, port)
			dialedMu.Unlock()
			peer, err := net.Dial("tcp", strings.TrimPrefix(target, "http://"))
			if err != nil {
				return nil, err
			}
			closed := false
			return pipeExecSession(peer, &closed), nil
		}
		return nil, runner.ErrExecStreamUnsupported
	}

	codeCookie, codeLoc := uiEnterApp(t, h, "code")
	docsCookie, docsLoc := uiEnterApp(t, h, "docs")

	// 1. The redirect names the app, so the two apps live in different URL
	//    namespaces instead of sharing /r/<run>/.
	if want := uiRunPrefix + h.run.ID.String() + "/code/ide"; codeLoc != want {
		t.Fatalf("code Location %q, want %q", codeLoc, want)
	}
	if want := uiRunPrefix + h.run.ID.String() + "/docs/readme"; docsLoc != want {
		t.Fatalf("docs Location %q, want %q", docsLoc, want)
	}
	// 2. THE defect: same name + same Path = the browser keeps exactly one.
	if codeCookie.Path == docsCookie.Path {
		t.Fatalf("both apps' cookies are scoped to %q — entering the second app "+
			"replaces the first app's session in the browser, and the first tab's "+
			"requests then dial the second app's port", codeCookie.Path)
	}

	// 3. Replay the FIRST app's request after entering the second: still the
	//    first app's port, still the first app's body.
	rec := uiGet(h, uiRunPrefix+h.run.ID.String()+"/code/ide", codeCookie)
	if rec.Code != http.StatusOK || rec.Body.String() != "code app" {
		t.Fatalf("replayed code request: %d %q, want 200 \"code app\"", rec.Code, rec.Body.String())
	}
	dialedMu.Lock()
	got := append([]int(nil), dialed...)
	dialedMu.Unlock()
	if len(got) == 0 || got[len(got)-1] != uiTestPort {
		t.Fatalf("the replayed code request dialed %v, want it to end at port %d", got, uiTestPort)
	}

	// 4. And one app's session is not a key to the other's path.
	if rec := uiGet(h, uiRunPrefix+h.run.ID.String()+"/docs/readme", codeCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("code's session on docs' path: %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// TestUIGateway_NonCanonicalRunIDIsNotARelayPath pins B3-F8: uuid.Parse accepts
// upper-case and braced spellings, but the prefix trim only ever removed the
// canonical lower-case one — so a non-browser client could make the gateway
// forward the /r/<id> prefix into the app. The path segment must BE the
// canonical id.
func TestUIGateway_NonCanonicalRunIDIsNotARelayPath(t *testing.T) {
	h := newUIHarness(t, okBackend())
	cookie := h.openSession()
	for _, seg := range []string{
		strings.ToUpper(h.run.ID.String()),
		"{" + h.run.ID.String() + "}",
		strings.ReplaceAll(h.run.ID.String(), "-", ""),
	} {
		if rec := uiGet(h, uiRunPrefix+seg+"/code/ide", cookie); rec.Code != http.StatusNotFound {
			t.Fatalf("non-canonical run id %q: %d %s, want 404", seg, rec.Code, rec.Body.String())
		}
	}
}

// ─── B3-F5: the cookie is not a frozen 8h bearer ─────────────────────────────

// signUISessionPayload signs a RAW payload the way encodeUISession does, so a
// test can mint a cookie in a format the current code does not write — here,
// the pre-0.7.4 payload with no issued-at.
func signUISessionPayload(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// TestUIGateway_PreIssuedAtCookieFailsClosed: adding issued-at to the payload is
// a cookie FORMAT change, and the old format carries no issued-at to bound
// staleness or to compare against a revoke cutoff. A correctly-signed, unexpired
// cookie in that format must be refused rather than trusted.
func TestUIGateway_PreIssuedAtCookieFailsClosed(t *testing.T) {
	h := newUIHarness(t, okBackend())
	old := signUISessionPayload(h.srv.cfg.UISessionKey, fmt.Sprintf(
		`{"r":%q,"a":"code","p":%d,"s":%q,"o":%q,"e":%d}`,
		h.run.ID, uiTestPort, h.owner, oidc.RoleMember, time.Now().Add(time.Hour).Unix()))
	rec := uiGet(h, uiRunPrefix+h.run.ID.String()+"/code/ide",
		&http.Cookie{Name: uiCookieName, Value: old})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a pre-issued-at relay cookie was accepted: %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// TestUIGateway_RevokedSessionIsRefusedOnTheNextConnection pins the first half
// of B3-F5: the relay session was a bearer frozen at enter, and uiDial re-checked
// only that the run was alive — so "revoke this human now" (D16), which stops
// their console session on the very next request, left them an editor with an
// in-sandbox terminal for the rest of the session TTL.
func TestUIGateway_RevokedSessionIsRefusedOnTheNextConnection(t *testing.T) {
	h := newUIHarness(t, closingBackend("sandbox app"))
	rev := &uiRevocations{}
	h.srv.cfg.SessionRevocations = rev
	cookie := h.openSession()
	path := uiRunPrefix + h.run.ID.String() + "/code/ide"

	if rec := uiGet(h, path, cookie); rec.Code != http.StatusOK {
		t.Fatalf("control before the revoke: %d %s, want 200", rec.Code, rec.Body.String())
	}
	if !rev.consulted() {
		t.Fatal("the relay opened a connection without consulting SessionRevocations")
	}
	rev.revoke()
	rec := uiGet(h, path, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a revoked human's relay session still opened a connection: %d %s, want 403",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), uiSessionNoLongerAuthorizedMsg) {
		t.Fatalf("refusal body %q does not carry the DRAFT string", rec.Body.String())
	}
}

// TestUIGateway_UnverifiableRevocationFailsClosed: a revocation store that
// cannot answer is the one case where continuing would serve the credential an
// admin may have just cancelled. It is a retryable 503, not a 403 — the human
// did nothing wrong — and never a quiet success.
func TestUIGateway_UnverifiableRevocationFailsClosed(t *testing.T) {
	h := newUIHarness(t, closingBackend("sandbox app"))
	h.srv.cfg.SessionRevocations = errUIRevocations{}
	cookie := h.openSession()
	rec := uiGet(h, uiRelayPrefix(h.run.ID, "code")+"/ide", cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreadable revocation store: %d %s, want 503", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), uiSessionUnverifiableMsg) {
		t.Fatalf("refusal body %q does not carry the DRAFT string", rec.Body.String())
	}
}

// TestUIGateway_OffboardedOwnerIsRefusedOnTheNextConnection pins the second
// half: ownership and role are re-asserted against the FRESHLY loaded run, not
// against what the cookie remembers from enter.
func TestUIGateway_OffboardedOwnerIsRefusedOnTheNextConnection(t *testing.T) {
	h := newUIHarness(t, closingBackend("sandbox app"))
	cookie := h.openSession()
	path := uiRunPrefix + h.run.ID.String() + "/code/ide"

	if rec := uiGet(h, path, cookie); rec.Code != http.StatusOK {
		t.Fatalf("control while alice still owns the run: %d %s", rec.Code, rec.Body.String())
	}
	handed := h.run
	handed.CreatedBy = "bob"
	h.store.putRun(handed)
	rec := uiGet(h, path, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a member's relay session survived losing the run: %d %s, want 403",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), uiSessionNoLongerAuthorizedMsg) {
		t.Fatalf("refusal body %q does not carry the DRAFT string", rec.Body.String())
	}
}

// TestUIGateway_EstablishedConnectionOutlivesTheRevoke is the bound, stated as a
// test: the re-checks run per CONNECTION, so a relayed WebSocket a human already
// holds keeps working until it closes (bounded by uiIdleConnTimeout for pooled
// connections, and by the run's own life for a hijacked one). Killing the run is
// what ends an in-flight session — the same bound attach and both SSH lanes
// publish.
func TestUIGateway_EstablishedConnectionOutlivesTheRevoke(t *testing.T) {
	held := make(chan struct{})
	defer close(held)
	h := newUIHarness(t, echoAfterUpgradeBackend(t, held))
	rev := &uiRevocations{}
	h.srv.cfg.SessionRevocations = rev

	gw := httptest.NewServer(h.gateway)
	defer gw.Close()
	cookie := h.openSession()

	conn, err := net.Dial("tcp", gw.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()
	req, err := http.NewRequest(http.MethodGet, gw.URL+uiRunPrefix+h.run.ID.String()+"/code/ide", nil)
	if err != nil {
		t.Fatalf("build upgrade request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.AddCookie(cookie)
	if err := req.Write(conn); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("relayed upgrade: %d, want 101", resp.StatusCode)
	}

	rev.revoke()
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write over the established relay: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read over the established relay after the revoke: %v", err)
	}
	if strings.TrimSpace(line) != "ping" {
		t.Fatalf("established relay echoed %q, want \"ping\"", strings.TrimSpace(line))
	}
}

// ─── the TTL knob ────────────────────────────────────────────────────────────

// TestUIGateway_SessionTTLIsAnOperatorBound: WARDYN_UI_SANDBOX_SESSION_TTL is
// the relay's sibling of WARDYN_SSH_ROLE_TTL — the operator's bound on how
// stale an already-minted session may be. Shortening it must bind the cookies
// ALREADY in browsers, whose signed Expires was computed under the old bound,
// which is only possible because the payload carries its own issued-at.
func TestUIGateway_SessionTTLIsAnOperatorBound(t *testing.T) {
	h := newUIHarness(t, okBackend())
	h.srv.cfg.UISessionTTL = time.Hour
	cookie := h.openSession()
	path := uiRelayPrefix(h.run.ID, "code") + "/ide"
	if rec := uiGet(h, path, cookie); rec.Code != http.StatusOK {
		t.Fatalf("fresh session under a 1h TTL: %d %s", rec.Code, rec.Body.String())
	}
	if cookie.Expires.IsZero() {
		t.Fatal("the relay cookie carries no browser-side expiry")
	}

	// The operator shortens the bound; the cookie in the browser is unchanged,
	// and its own signed Expires is still an hour away.
	h.srv.cfg.UISessionTTL = time.Second
	h.srv.cfg.Now = func() time.Time { return time.Now().Add(time.Minute) }
	if rec := uiGet(h, path, cookie); rec.Code != http.StatusForbidden {
		t.Fatalf("a session older than the configured TTL was accepted: %d %s, want 403",
			rec.Code, rec.Body.String())
	}
}

// TestUIGateway_DefaultSessionTTLIsTheShippedBound: a Config that never went
// through cmd/wardynd's flags (every test harness, and any embedder) must get
// the shipped 8h posture, not a zero TTL that refuses every session.
func TestUIGateway_DefaultSessionTTLIsTheShippedBound(t *testing.T) {
	if got := New(Config{}).uiSessionTTL(); got != defaultUISessionTTL {
		t.Fatalf("default UI session TTL = %v, want %v", got, defaultUISessionTTL)
	}
}

// ─── parse ───────────────────────────────────────────────────────────────────

// TestParseUIRunPath is the table for the one function that decides what a
// relay path even is — and therefore the one place the canonical-id and
// app-segment requirements are enforced.
func TestParseUIRunPath(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		path string
		app  string
		ok   bool
	}{
		{uiRunPrefix + id.String() + "/code/ide", "code", true},
		{uiRunPrefix + id.String() + "/code/", "code", true},
		{uiRunPrefix + id.String() + "/code", "code", true},
		{uiRunPrefix + id.String() + "/", "", false},
		{uiRunPrefix + id.String(), "", false},
		{uiRunPrefix + strings.ToUpper(id.String()) + "/code/ide", "", false},
		{uiRunPrefix + "{" + id.String() + "}/code", "", false},
		{uiRunPrefix + "not-a-uuid/code", "", false},
		{"/other/" + id.String() + "/code", "", false},
	} {
		gotID, gotApp, ok := parseUIRunPath(tc.path)
		if ok != tc.ok || gotApp != tc.app || (ok && gotID != id) {
			t.Fatalf("parseUIRunPath(%q) = %v, %q, %v; want app %q, ok %v",
				tc.path, gotID, gotApp, ok, tc.app, tc.ok)
		}
	}
}
