// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The relay SESSION's own tests: which app a cookie is for and
// how long, and under whose authority, it keeps working. The transport
// matrix — header hygiene, launcher exit codes, the connection cap — lives in
// uigateway_test.go and is untouched by these.
package api

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// helpers

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
func (r *uiRevocations) askedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.asked)
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
		"ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleUser)},
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

// one cookie per app, and a canonical run id

// TestUIGateway_SecondAppDoesNotHijackTheFirstApp pins the following. The relay cookie
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

// TestUIGateway_NonCanonicalRunIDIsNotARelayPath pins uuid.Parse accepts
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

// the cookie is not a frozen 8h bearer

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
		h.run.ID, uiTestPort, h.owner, oidc.RoleUser, time.Now().Add(time.Hour).Unix()))
	rec := uiGet(h, uiRunPrefix+h.run.ID.String()+"/code/ide",
		&http.Cookie{Name: uiCookieName, Value: old})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a pre-issued-at relay cookie was accepted: %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// TestUIGateway_RevokedSessionIsRefusedOnTheNextConnection pins the first half
// of the relay session was a bearer frozen at enter, and uiDial re-checked
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
	// The store answers at redemption, then stops: redemption itself fails
	// closed on an unreadable store (TestUIGateway_UnverifiableRevocationRefusesRedemption).
	h.srv.cfg.SessionRevocations = &uiRevocations{}
	cookie := h.openSession()
	h.srv.cfg.SessionRevocations = errUIRevocations{}
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
// test: an ALREADY-ESTABLISHED relayed WebSocket is one hijacked connection with
// no further requests to check, so it keeps working until it closes or the run
// ends. Ordinary pooled connections are not in this hole — uiReassertRelay
// re-checks those on the request path (TestUIGateway_PooledConnectionIsReassertedOnAnInterval);
// a hijacked upgrade has no request path left. Killing the run is what ends an
// in-flight session — the same bound attach and both SSH lanes publish.
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

// the TTL knob

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
	h.clock.advance(time.Minute)
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

// parse

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

// UG-1: a reused pooled connection is re-checked too

// countingSocat wraps the harness's fake runner so a test can see how many
// relay DIALS actually happened. net/http pools the relay connection, so the
// dial count is exactly "how many times uiDial ran" — which is the difference
// between a per-connection check and a per-request one.
func countingSocat(h *uiHarness, n *int32) {
	inner := h.runner.execFn
	h.runner.execFn = func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		if spec.Argv[0] == "socat" {
			atomic.AddInt32(n, 1)
		}
		return inner(spec)
	}
}

// TestUIGateway_PooledConnectionIsReassertedOnAnInterval pins UG-1. The
// re-assert lived only in uiDial, and net/http calls that ONLY when the pool
// has no reusable connection — and uiIdleConnTimeout resets on every reuse. A
// revoked human whose editor polls faster than the idle window therefore rode
// one warm connection, was never re-checked, and kept working until the session
// TTL: the lane's own live gate measured 20 relayed requests over 2 connections,
// i.e. 18 requests with no check at all.
//
// The relay now re-asserts on the REQUEST path too, debounced to
// uiReassertInterval so it stays a bounded number of store reads and does NOT
// turn the pooled connection back into a dial per request.
func TestUIGateway_PooledConnectionIsReassertedOnAnInterval(t *testing.T) {
	h := newUIHarness(t, okBackend()) // keep-alive: the relay connection is POOLED
	var dials int32
	countingSocat(h, &dials)
	rev := &uiRevocations{}
	h.srv.cfg.SessionRevocations = rev

	cookie := h.openSession()
	path := uiRelayPrefix(h.run.ID, "code") + "/ide"
	if rec := uiGet(h, path, cookie); rec.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", rec.Code, rec.Body.String())
	}
	opened := atomic.LoadInt32(&dials)
	if opened != 1 {
		t.Fatalf("first request made %d dial(s), want exactly 1", opened)
	}

	// Revoked, but still inside the debounce window: served on the warm
	// connection, and — the property that must not regress — WITHOUT dialing
	// again. Bounded staleness is the design; unbounded staleness was the bug.
	rev.revoke()
	if rec := uiGet(h, path, cookie); rec.Code != http.StatusOK {
		t.Fatalf("request inside the re-assert window: %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt32(&dials); got != opened {
		t.Fatalf("the re-assert turned a pooled request into a dial (%d → %d) — "+
			"the connection pool is a resource bound, not an optimisation", opened, got)
	}

	// Past the window, on that SAME pooled connection: refused.
	h.clock.advance(uiReassertInterval + time.Second)
	rec := uiGet(h, path, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a revoked human kept being served on a warm pooled connection: %d %s, want 403",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), uiSessionNoLongerAuthorizedMsg) {
		t.Fatalf("refusal body %q does not carry the DRAFT string", rec.Body.String())
	}
	if got := atomic.LoadInt32(&dials); got != opened {
		t.Fatalf("a refused request still dialed the sandbox (%d → %d)", opened, got)
	}
}

// TestUIGateway_ReassertIsDebouncedPerSession: the re-assert costs one GetRun
// plus one revocation lookup, so it must not run per request. One request per
// millisecond over a window must ask the revocation store once, not once each.
func TestUIGateway_ReassertIsDebouncedPerSession(t *testing.T) {
	h := newUIHarness(t, okBackend())
	rev := &uiRevocations{}
	h.srv.cfg.SessionRevocations = rev
	cookie := h.openSession()
	path := uiRelayPrefix(h.run.ID, "code") + "/ide"
	redeemed := rev.askedCount() // the redemption's own check (#1474)

	if rec := uiGet(h, path, cookie); rec.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", rec.Code, rec.Body.String())
	}
	// Two on the FIRST request, by design: the request path checks, and the
	// dial that request triggers checks again. A new connection is always
	// re-checked — that guarantee is not debounced away.
	opening := rev.askedCount()
	if opening-redeemed != 2 {
		t.Fatalf("the first relayed request asked the revocation store %d time(s), want 2 "+
			"(once on the request path, once on the connection it opened)", opening-redeemed)
	}

	for i := range 9 {
		h.clock.advance(time.Millisecond)
		if rec := uiGet(h, path, cookie); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d %s", i+2, rec.Code, rec.Body.String())
		}
	}
	if got := rev.askedCount(); got != opening {
		t.Fatalf("9 further relayed requests inside the window added %d revocation lookup(s), want 0 — "+
			"the re-assert is not debounced, and a pair of store reads per relayed request is not a check, "+
			"it is a load generator", got-opening)
	}
}

// TestUIGateway_ReassertRefusesWhenTheRunCannotBeLoaded: the request-path
// re-assert needs the run to check ownership against, so a store that cannot
// hand it over fails CLOSED with the same retryable 503 an unreadable
// revocation store gets. Serving on because the check could not run is the one
// outcome this whole item exists to remove.
func TestUIGateway_ReassertRefusesWhenTheRunCannotBeLoaded(t *testing.T) {
	h := newUIHarness(t, okBackend())
	cookie := h.openSession()
	path := uiRelayPrefix(h.run.ID, "code") + "/ide"
	if rec := uiGet(h, path, cookie); rec.Code != http.StatusOK {
		t.Fatalf("control: %d %s", rec.Code, rec.Body.String())
	}

	h.store.dropRun(h.run.ID)
	h.clock.advance(uiReassertInterval + time.Second)
	rec := uiGet(h, path, cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("relay over a warm connection with the run unreadable: %d %s, want 503",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), uiSessionUnverifiableMsg) {
		t.Fatalf("refusal body %q does not carry the DRAFT string", rec.Body.String())
	}
}

// UG-2: a refused re-check is in the audit trail

// TestUIGateway_RefusedReassertIsAuditedWithItsReason: "someone is driving a
// revoked relay credential" has to be visible. Each refusal arm writes one
// ui.authorize/denied naming which arm it was — the same shape every other refusal
// on this listener already uses.
func TestUIGateway_RefusedReassertIsAuditedWithItsReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		arm    func(*uiHarness)
		reason string
	}{
		{"revoked", func(h *uiHarness) {
			rev := &uiRevocations{}
			rev.revoke()
			h.srv.cfg.SessionRevocations = rev
		}, uiDeniedReasonRevoked},
		{"lost the run", func(h *uiHarness) {
			handed := h.run
			handed.CreatedBy = "bob"
			h.store.putRun(handed)
		}, uiDeniedReasonNotAuthorized},
		{"revocation store down", func(h *uiHarness) {
			h.srv.cfg.SessionRevocations = errUIRevocations{}
		}, uiDeniedReasonRevocationUnavailable},
		{"run unreadable", func(h *uiHarness) {
			h.store.dropRun(h.run.ID)
		}, reasonRunUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUIHarness(t, closingBackend("sandbox app"))
			cookie := h.openSession()
			tc.arm(h)
			if rec := uiGet(h, uiRelayPrefix(h.run.ID, "code")+"/ide", cookie); rec.Code == http.StatusOK {
				t.Fatalf("the refusal arm did not refuse: %d", rec.Code)
			}
			if got := strings.Join(h.audit.actions(), " "); !strings.Contains(got, "ui.authorize/denied") {
				t.Fatalf("audit %q has no ui.authorize/denied row for a refused relay connection", got)
			}
			if !h.audit.hasDataValue("reason", tc.reason) {
				t.Fatalf("no audit row carries reason=%q; rows: %s", tc.reason, h.audit.dataReasons())
			}
		})
	}
}

// auditRows returns the recorded rows with the given action and outcome.
func (r *safeRecorder) auditRows(action, outcome string) []types.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []types.AuditEvent
	for _, ev := range r.events {
		if ev.Action == action && ev.Outcome == outcome {
			out = append(out, ev)
		}
	}
	return out
}

// setCookies lists the cookies a response set under the relay cookie's name.
func setUICookies(rec *httptest.ResponseRecorder) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == uiCookieName {
			out = append(out, c)
		}
	}
	return out
}

// the ticket carries its authority (#1474)

// TestUIGateway_TicketMintedBeforeCutoffIsRefused: a ticket admitted before a
// revoke must not mint a relay session stamped with the redemption time, which
// is after the cutoff and would pass every later check. The refusal is the
// bad-ticket one byte for byte, sets no cookie, and writes one denied row.
func TestUIGateway_TicketMintedBeforeCutoffIsRefused(t *testing.T) {
	h := newUIHarness(t, closingBackend("sandbox terminal reachable"))
	rev := newCutoffRevocations()
	rev.nowFunc = h.clock.now
	h.srv.cfg.SessionRevocations = rev
	ticket := h.ticket(h.run.ID, h.owner, oidc.RoleUser)
	h.clock.advance(3 * time.Second)
	if err := rev.RevokeSub(context.Background(), h.owner); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(3 * time.Second)

	rec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {ticket}})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "invalid, expired, or already-used attach ticket") {
		t.Fatalf("a ticket minted before the cutoff: %d %s, want the 403 bad-ticket body", rec.Code, rec.Body.String())
	}
	if cs := setUICookies(rec); len(cs) != 0 {
		t.Fatalf("a refused ticket set a relay cookie: %+v", cs)
	}
	rows := h.audit.auditRows("ui.authorize", "denied")
	if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"reason":"revoked"`) || rows[0].Actor != h.owner {
		t.Fatalf("denied rows = %+v, want exactly one naming %q with reason revoked", rows, h.owner)
	}
}

// TestUIGateway_PortalRevokeEndsDerivedSession: a relay session opened through a
// portal ends with the portal's grant. Revoking the portal is not undone by the
// session's own eight-hour cookie.
func TestUIGateway_PortalRevokeEndsDerivedSession(t *testing.T) {
	h := newUIHarness(t, closingBackend("portal still reaches sandbox"))
	ds := newFakeDelegateStore()
	h.srv.cfg.Store = &uiDelegateStore{h.store, ds}
	raw, via := seedDelegation(t, ds, h.owner)
	minted := do(t, h.srv, http.MethodPost, "/api/v1/runs/"+h.run.ID.String()+"/attach-ticket", raw, "")
	if minted.Code != http.StatusOK {
		t.Fatalf("ticket route = %d %s", minted.Code, minted.Body.String())
	}
	var body struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(minted.Body.Bytes(), &body); err != nil || body.Ticket == "" {
		t.Fatalf("ticket response: %v %s", err, minted.Body.String())
	}
	rec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {body.Ticket}})
	cs := setUICookies(rec)
	if rec.Code != http.StatusFound || len(cs) != 1 {
		t.Fatalf("enter = %d %s, want a redirect with the relay cookie", rec.Code, rec.Body.String())
	}
	cookie := cs[0]
	path := uiRelayPrefix(h.run.ID, "code") + "/"
	if r := uiGet(h, path, cookie); r.Code != http.StatusOK {
		t.Fatalf("control before the revoke: %d %s", r.Code, r.Body.String())
	}
	if _, err := ds.RevokeDelegate(context.Background(), via.Delegate, h.clock.now()); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(uiReassertInterval + time.Second)

	relay := uiGet(h, path, cookie)
	if relay.Code != http.StatusForbidden || !strings.Contains(relay.Body.String(), uiSessionNoLongerAuthorizedMsg) {
		t.Fatalf("relay after the portal revoke: %d %s, want the not-authorized 403", relay.Code, relay.Body.String())
	}
	rows := h.audit.auditRows("ui.authorize", "denied")
	if len(rows) == 0 || !strings.Contains(string(rows[0].Data), `"via"`) {
		t.Fatalf("first denied row = %+v, want data.via", rows)
	}
}

// uiDelegateStore is the UI harness's run and ticket store with the portal
// tables beside it.
type uiDelegateStore struct {
	*uiMemStore
	*fakeDelegateStore
}

// ticket authority: negatives (#1474)

// enterWith drives the browser hand-off for ticket on h's run and app.
func (h *uiHarness) enterWith(ticket string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {ticket}})
}

// mintFor mints a ticket the way the mint route does for a verified human: the
// email rides the context, and now is the admission time.
func (h *uiHarness) mintFor(principal, email, role string) string {
	h.t.Helper()
	ctx := withOIDCEmail(context.Background(), email)
	tok, err := mintAttachTicket(ctx, h.store, h.run.ID, types.ActorHuman, principal, role, h.clock.now())
	if err != nil {
		h.t.Fatalf("mint ticket: %v", err)
	}
	return tok
}

// TestUIGateway_EmailNamedRevokeReachesATicketSession: a revoke that names the
// person's email stops a relay session opened from a ticket, at redemption and
// at the next re-assert. The ticket used to carry no email, so only a revoke by
// subject or all:true could.
func TestUIGateway_EmailNamedRevokeReachesATicketSession(t *testing.T) {
	const email = "alice@corp.example"
	t.Run("at redemption", func(t *testing.T) {
		h := newUIHarness(t, closingBackend("sandbox app"))
		rev := newCutoffRevocations()
		rev.nowFunc = h.clock.now
		h.srv.cfg.SessionRevocations = rev
		ticket := h.mintFor(h.owner, email, oidc.RoleUser)
		h.clock.advance(time.Second)
		if err := rev.RevokeSub(context.Background(), email); err != nil {
			t.Fatal(err)
		}
		h.clock.advance(time.Second)
		if rec := h.enterWith(ticket); rec.Code != http.StatusForbidden || len(setUICookies(rec)) != 0 {
			t.Fatalf("enter after an email-named revoke: %d %s, want 403 and no cookie", rec.Code, rec.Body.String())
		}
	})
	t.Run("at the next re-assert", func(t *testing.T) {
		h := newUIHarness(t, closingBackend("sandbox app"))
		rev := newCutoffRevocations()
		rev.nowFunc = h.clock.now
		h.srv.cfg.SessionRevocations = rev
		rec := h.enterWith(h.mintFor(h.owner, email, oidc.RoleUser))
		cs := setUICookies(rec)
		if rec.Code != http.StatusFound || len(cs) != 1 {
			t.Fatalf("enter: %d %s", rec.Code, rec.Body.String())
		}
		path := uiRelayPrefix(h.run.ID, "code") + "/"
		if r := uiGet(h, path, cs[0]); r.Code != http.StatusOK {
			t.Fatalf("control: %d %s", r.Code, r.Body.String())
		}
		h.clock.advance(time.Second)
		if err := rev.RevokeSub(context.Background(), email); err != nil {
			t.Fatal(err)
		}
		h.clock.advance(uiReassertInterval + time.Second)
		if r := uiGet(h, path, cs[0]); r.Code != http.StatusForbidden || !strings.Contains(r.Body.String(), uiSessionNoLongerAuthorizedMsg) {
			t.Fatalf("relay after an email-named revoke: %d %s, want the not-authorized 403", r.Code, r.Body.String())
		}
	})
}

// TestUIGateway_UnverifiableRevocationRefusesRedemption: a revocation store that
// cannot answer at redemption is a retryable 503 with no cookie, never reported
// as a bad ticket and never a session.
func TestUIGateway_UnverifiableRevocationRefusesRedemption(t *testing.T) {
	h := newUIHarness(t, okBackend())
	h.srv.cfg.SessionRevocations = errUIRevocations{}
	rec := h.enterWith(h.ticket(h.run.ID, h.owner, oidc.RoleUser))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), uiSessionUnverifiableMsg) ||
		!strings.Contains(rec.Body.String(), `"reason":"revocation_unavailable"`) {
		t.Fatalf("enter with an unreadable revocation store: %d %s, want 503 revocation_unavailable", rec.Code, rec.Body.String())
	}
	if cs := setUICookies(rec); len(cs) != 0 {
		t.Fatalf("a refused redemption set a cookie: %+v", cs)
	}
}

// TestUIGateway_TicketWithoutAuthorityIsRefused: a row written before the
// authority columns existed reads a zero authority time. Zero never means
// exempt, so it is refused with the bad-ticket body and no cookie.
func TestUIGateway_TicketWithoutAuthorityIsRefused(t *testing.T) {
	h := newUIHarness(t, okBackend())
	h.store.mu.Lock()
	h.store.tickets["legacy"] = store.AttachTicket{RunID: h.run.ID, ActorType: types.ActorHuman, Principal: h.owner, Role: oidc.RoleUser}
	h.store.mu.Unlock()
	rec := h.enterWith("legacy")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "invalid, expired, or already-used attach ticket") ||
		len(setUICookies(rec)) != 0 {
		t.Fatalf("a ticket with no authority time: %d %s, want the 403 bad-ticket body and no cookie", rec.Code, rec.Body.String())
	}
}

// TestUIGateway_CookieWithoutAuthorityFailsClosed: a relay cookie from before
// 0.8.5 carries no authority time, so the revoke check cannot be made against
// it. It is refused on first use, like the pre-0.7.4 format.
func TestUIGateway_CookieWithoutAuthorityFailsClosed(t *testing.T) {
	h := newUIHarness(t, okBackend())
	old := signUISessionPayload(h.srv.cfg.UISessionKey, fmt.Sprintf(
		`{"r":%q,"a":"code","p":%d,"s":%q,"o":%q,"e":%d,"i":%d}`,
		h.run.ID, uiTestPort, h.owner, oidc.RoleUser, time.Now().Add(time.Hour).Unix(), time.Now().Unix()))
	rec := uiGet(h, uiRunPrefix+h.run.ID.String()+"/code/ide", &http.Cookie{Name: uiCookieName, Value: old})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a cookie with no authority time was accepted: %d %s, want 403", rec.Code, rec.Body.String())
	}
}

// TestUIGateway_AdminTokenTicketStillOpensASession: the admin token and local
// seat carry no email; "" is valid, and a revoke of someone else does not reach
// it.
func TestUIGateway_AdminTokenTicketStillOpensASession(t *testing.T) {
	h := newUIHarness(t, okBackend())
	rev := newCutoffRevocations()
	h.srv.cfg.SessionRevocations = rev
	op := h.run
	op.CreatedBy, op.OperatorOwned = "admin-token", true
	h.store.putRun(op)
	if err := rev.RevokeSub(context.Background(), "someone-else"); err != nil {
		t.Fatal(err)
	}
	rec := h.enterWith(h.ticket(h.run.ID, "admin-token", oidc.RoleAdmin))
	if rec.Code != http.StatusFound || len(setUICookies(rec)) != 1 {
		t.Fatalf("admin-token ticket: %d %s, want a session", rec.Code, rec.Body.String())
	}
}

// the portal grant (#1475)

// portalHarness is a UI harness whose store has the portal tables, with a live
// grant for the run's owner.
func portalHarness(t *testing.T) (*uiHarness, *fakeDelegateStore, string, types.DelegationVia) {
	t.Helper()
	h := newUIHarness(t, closingBackend("portal sandbox app"))
	ds := newFakeDelegateStore()
	h.srv.cfg.Store = &uiDelegateStore{h.store, ds}
	raw, via := seedDelegation(t, ds, h.owner)
	return h, ds, raw, via
}

// portalTicket mints a ticket through the portal, as the delegated route does.
func (h *uiHarness) portalTicket(via types.DelegationVia) string {
	h.t.Helper()
	tok, err := mintAttachTicket(audit.WithDelegation(context.Background(), via), h.store, h.run.ID,
		types.ActorHuman, h.owner, oidc.RoleUser, h.clock.now())
	if err != nil {
		h.t.Fatalf("mint ticket: %v", err)
	}
	return tok
}

func TestUIGateway_PortalGrant(t *testing.T) {
	const body = "invalid, expired, or already-used attach ticket"

	t.Run("redemption after a portal revoke is refused, naming the portal", func(t *testing.T) {
		h, ds, _, via := portalHarness(t)
		ticket := h.portalTicket(via)
		if _, err := ds.RevokeDelegate(context.Background(), via.Delegate, h.clock.now()); err != nil {
			t.Fatal(err)
		}
		rec := h.enterWith(ticket)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), body) || len(setUICookies(rec)) != 0 {
			t.Fatalf("enter after a portal revoke: %d %s, want the bad-ticket 403 and no cookie", rec.Code, rec.Body.String())
		}
		rows := h.audit.auditRows("ui.authorize", "denied")
		if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"reason":"delegation_ended"`) || !strings.Contains(string(rows[0].Data), `"via"`) {
			t.Fatalf("denied rows = %+v, want one delegation_ended carrying via", rows)
		}
	})

	t.Run("grant expiry with no revoke refuses redemption and the open session", func(t *testing.T) {
		h, _, _, via := portalHarness(t)
		rec := h.enterWith(h.portalTicket(via))
		cs := setUICookies(rec)
		if rec.Code != http.StatusFound || len(cs) != 1 {
			t.Fatalf("enter: %d %s", rec.Code, rec.Body.String())
		}
		h.clock.advance(delegatedTokenTTL + time.Second) // past the grant, with no revoke
		if r := uiGet(h, uiRelayPrefix(h.run.ID, "code")+"/", cs[0]); r.Code != http.StatusForbidden {
			t.Fatalf("relay after the grant expired: %d %s, want 403", r.Code, r.Body.String())
		}
		if rec := h.enterWith(h.portalTicket(via)); rec.Code != http.StatusForbidden {
			t.Fatalf("redemption after the grant expired: %d %s, want 403", rec.Code, rec.Body.String())
		}
	})

	t.Run("the cookie never outlives the grant", func(t *testing.T) {
		h, ds, _, via := portalHarness(t)
		rec := h.enterWith(h.portalTicket(via))
		cs := setUICookies(rec)
		if rec.Code != http.StatusFound || len(cs) != 1 {
			t.Fatalf("enter: %d %s", rec.Code, rec.Body.String())
		}
		grant, err := ds.GetDelegatedTokenByID(context.Background(), via.Grant, h.clock.now())
		if err != nil {
			t.Fatal(err)
		}
		if cs[0].Expires.After(grant.ExpiresAt) {
			t.Fatalf("cookie expires %s, after the grant's %s", cs[0].Expires, grant.ExpiresAt)
		}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(cs[0])
		sess, ok := h.srv.decodeUISession(req, h.clock.now())
		if !ok || sess.Expires > grant.ExpiresAt.Unix() {
			t.Fatalf("signed session expiry %d, want <= grant %d (ok=%v)", sess.Expires, grant.ExpiresAt.Unix(), ok)
		}
	})

	t.Run("a swept grant row ends the session", func(t *testing.T) {
		h, ds, _, via := portalHarness(t)
		cs := setUICookies(h.enterWith(h.portalTicket(via)))
		if len(cs) != 1 {
			t.Fatal("no session")
		}
		ds.mu.Lock()
		clear(ds.tokens)
		ds.mu.Unlock()
		h.clock.advance(uiReassertInterval + time.Second)
		if r := uiGet(h, uiRelayPrefix(h.run.ID, "code")+"/", cs[0]); r.Code != http.StatusForbidden {
			t.Fatalf("relay after the grant row was swept: %d %s, want 403", r.Code, r.Body.String())
		}
	})

	t.Run("a grant under another portal is refused", func(t *testing.T) {
		h, _, _, via := portalHarness(t)
		other := types.DelegationVia{Delegate: uuid.New(), Grant: via.Grant}
		if rec := h.enterWith(h.portalTicket(other)); rec.Code != http.StatusForbidden || len(setUICookies(rec)) != 0 {
			t.Fatalf("a ticket naming another portal's grant: %d %s, want 403 and no cookie", rec.Code, rec.Body.String())
		}
	})

	t.Run("a grant store that errors is a 503", func(t *testing.T) {
		h, ds, _, via := portalHarness(t)
		h.srv.cfg.Store = &delegationErrStore{&uiDelegateStore{h.store, ds}}
		rec := h.enterWith(h.portalTicket(via))
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"reason":"delegation_unavailable"`) ||
			len(setUICookies(rec)) != 0 {
			t.Fatalf("enter with an unreadable grant store: %d %s, want 503 delegation_unavailable and no cookie", rec.Code, rec.Body.String())
		}
	})

	t.Run("a store that is not a DelegateStore is refused, not waved through", func(t *testing.T) {
		h := newUIHarness(t, okBackend())
		via := types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}
		rec := h.enterWith(h.portalTicket(via))
		if rec.Code != http.StatusServiceUnavailable || len(setUICookies(rec)) != 0 {
			t.Fatalf("a portal ticket against a store with no portal capability: %d %s, want 503 and no cookie", rec.Code, rec.Body.String())
		}
	})

	t.Run("the revoke reaches a session with no SessionRevocations configured", func(t *testing.T) {
		h, ds, _, via := portalHarness(t)
		if h.srv.cfg.SessionRevocations != nil {
			t.Fatal("the harness must not configure SessionRevocations")
		}
		cs := setUICookies(h.enterWith(h.portalTicket(via)))
		if len(cs) != 1 {
			t.Fatal("no session")
		}
		if _, err := ds.RevokeDelegate(context.Background(), via.Delegate, h.clock.now()); err != nil {
			t.Fatal(err)
		}
		h.clock.advance(uiReassertInterval + time.Second)
		if r := uiGet(h, uiRelayPrefix(h.run.ID, "code")+"/", cs[0]); r.Code != http.StatusForbidden {
			t.Fatalf("relay after a portal revoke with no revocation store: %d %s, want 403", r.Code, r.Body.String())
		}
	})

	t.Run("a personal session is unaffected by a portal revoke", func(t *testing.T) {
		h, ds, _, via := portalHarness(t)
		cs := setUICookies(h.enterWith(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
		if len(cs) != 1 {
			t.Fatal("no session")
		}
		if _, err := ds.RevokeDelegate(context.Background(), via.Delegate, h.clock.now()); err != nil {
			t.Fatal(err)
		}
		h.clock.advance(uiReassertInterval + time.Second)
		if r := uiGet(h, uiRelayPrefix(h.run.ID, "code")+"/", cs[0]); r.Code != http.StatusOK {
			t.Fatalf("a personal session after a portal revoke: %d %s, want 200", r.Code, r.Body.String())
		}
	})
}

// delegationErrStore is a store whose grant lookup cannot answer.
type delegationErrStore struct{ *uiDelegateStore }

func (*delegationErrStore) GetDelegatedTokenByID(context.Context, uuid.UUID, time.Time) (types.DelegatedToken, error) {
	return types.DelegatedToken{}, errors.New("store unreachable")
}
