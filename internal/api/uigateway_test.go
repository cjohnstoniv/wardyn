// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The UI-sandbox gateway's security matrix. Everything here runs in process
// against a fake Runner whose "socat" dials a real httptest backend — so the
// relay is exercised end to end (ticket → cookie → proxied request → response)
// with no sandbox, no docker and no listener.
//
// What these tests exist to prevent, in order of how bad it would be:
//   - the gateway's routes appearing on the CONSOLE origin, where relayed
//     sandbox code could read the console session
//   - a relay session obtained without redeeming a valid, owner-or-admin,
//     single-use ticket for THAT run
//   - a wardyn_* cookie or an Authorization header reaching a sandbox
//   - a sandbox setting a wardyn_* cookie in the operator's browser
//   - a port that no policy declared being relayed at all
package api

import (
	"context"
	"encoding/json"
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
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

// uiMemStore extends the package's run fake (sshMemStore) with the two things
// the gateway needs beyond a run row: the attach-ticket table it redeems, and
// the audit tail it reads a run's EFFECTIVE policy back out of.
type uiMemStore struct {
	*sshMemStore
	mu      sync.Mutex
	tickets map[string]store.AttachTicket
	events  []types.AuditEvent
	touched bool
}

func newUIMemStore() *uiMemStore {
	return &uiMemStore{sshMemStore: newSSHMemStore(), tickets: map[string]store.AttachTicket{}}
}

func (s *uiMemStore) MintAttachTicket(_ context.Context, token string, t store.AttachTicket, _, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tickets[token] = t
	return nil
}

// ConsumeAttachTicket burns the row on ANY redemption attempt, exactly as the
// SQL does (DELETE ... RETURNING) — so a probe against a guessed run id spends
// the ticket.
func (s *uiMemStore) ConsumeAttachTicket(_ context.Context, token string, _ time.Time) (store.AttachTicket, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[token]
	delete(s.tickets, token)
	return t, ok, nil
}

// TouchRun records that the idle clock was reset — the relay must keep a run
// an operator is actively using out of the reaper's way.
func (s *uiMemStore) TouchRun(context.Context, uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched = true
	return nil
}

func (s *uiMemStore) wasTouched() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.touched
}

func (s *uiMemStore) QueryAuditEvents(_ context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []types.AuditEvent{}
	for _, ev := range s.events {
		if ev.RunID != nil && *ev.RunID == runID && len(out) < limit {
			out = append(out, ev)
		}
	}
	return out, nil
}

// putEffectivePolicy seeds the run.policy.effective envelope dispatch writes —
// the gateway's only source for which apps a run really declared.
func (s *uiMemStore) putEffectivePolicy(runID uuid.UUID, spec types.RunPolicySpec) {
	data, _ := json.Marshal(spec)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, types.AuditEvent{
		ID: uuid.New(), Time: time.Now(), RunID: &runID,
		Action: "run.policy.effective", Outcome: "success", Data: data,
	})
}

// safeRecorder is recRecorder with a mutex: ui.close is written from the
// connection's own goroutine, so an unguarded slice would race under -race.
type safeRecorder struct {
	mu     sync.Mutex
	events []types.AuditEvent
}

func (r *safeRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *safeRecorder) actions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.Action+"/"+ev.Outcome)
	}
	return out
}

// uiHarness is one gateway under test plus the backend its "sandbox app" runs
// on and the knobs each test needs.
type uiHarness struct {
	t        *testing.T
	srv      *Server
	store    *uiMemStore
	runner   *sshFakeRunner
	audit    *safeRecorder
	gateway  http.Handler
	backend  *httptest.Server
	run      types.AgentRun
	owner    string
	launcher int // exit code the in-sandbox launcher probe reports
}

const uiTestPort = 8080

// newUIHarness builds a RUNNING run owned by "alice" whose policy declares one
// app ("code" on 8080), a fake runner whose socat dials the backend, and the
// gateway handler.
func newUIHarness(t *testing.T, backend http.Handler) *uiHarness {
	t.Helper()
	h := &uiHarness{
		t: t, store: newUIMemStore(), runner: &sshFakeRunner{}, audit: &safeRecorder{},
		owner: "alice", backend: httptest.NewServer(backend),
	}
	t.Cleanup(h.backend.Close)

	h.run = types.AgentRun{
		ID: uuid.New(), CreatedBy: h.owner, State: types.RunRunning, SandboxRef: "sandbox-1",
	}
	h.store.putRun(h.run)
	h.store.putEffectivePolicy(h.run.ID, types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		UIApps:              []types.UIApp{{Name: "code", Port: uiTestPort, Path: "/ide"}},
	})

	h.runner.execFn = func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		switch spec.Argv[0] {
		case "sh": // the launcher probe
			code := h.launcher
			return &runner.ExecSession{Wait: func() (int, error) { return code, nil }}, nil
		case "socat": // the relay dial
			peer, err := net.Dial("tcp", strings.TrimPrefix(h.backend.URL, "http://"))
			if err != nil {
				return nil, err
			}
			closed := false
			return pipeExecSession(peer, &closed), nil
		}
		return nil, runner.ErrExecStreamUnsupported
	}

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	h.srv = New(Config{
		Store: h.store, Runner: h.runner, Audit: h.audit,
		UIListenAddr: ":8081", UIAdvertiseURL: "https://ui.example.com", UISessionKey: key,
		BaseCtx: context.Background(),
	})
	h.gateway = h.srv.UIGatewayHandler()
	if h.gateway == nil {
		t.Fatal("UIGatewayHandler is nil with the gateway configured")
	}
	return h
}

// ticket mints a single-use attach ticket the way POST /runs/{id}/attach-ticket
// does, for the given principal and role.
func (h *uiHarness) ticket(runID uuid.UUID, principal, role string) string {
	h.t.Helper()
	tok, err := mintAttachTicket(context.Background(), h.store, runID, types.ActorHuman, principal, role, time.Now())
	if err != nil {
		h.t.Fatalf("mint ticket: %v", err)
	}
	return tok
}

// enter drives GET /__wardyn/enter with the given query and returns the
// response recorder.
func (h *uiHarness) enter(q url.Values) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, uiEnterPath+"?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	h.gateway.ServeHTTP(rec, req)
	return rec
}

// openSession runs the whole handoff and returns the relay cookie.
func (h *uiHarness) openSession() *http.Cookie {
	h.t.Helper()
	rec := h.enter(url.Values{
		"run": {h.run.ID.String()}, "app": {"code"},
		"ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleMember)},
	})
	if rec.Code != http.StatusFound {
		h.t.Fatalf("enter: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == uiCookieName {
			return c
		}
	}
	h.t.Fatal("no relay cookie set")
	return nil
}

// relay issues one request through the gateway on the run's relay path.
func (h *uiHarness) relay(path string, cookie *http.Cookie, mutate func(*http.Request)) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, uiRunPrefix+h.run.ID.String()+path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.gateway.ServeHTTP(rec, req)
	return rec
}

func okBackend() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "sandbox app")
	})
}

// ─── the gateway exists only on its own origin ───────────────────────────────

// TestUIGateway_OffMeansNoHandlerAndNoHealthzBlock: empty
// WARDYN_UI_SANDBOX_LISTEN is off — no handler for cmd/wardynd to serve, and
// /healthz's ui_sandbox is null rather than a block claiming a disabled
// feature.
func TestUIGateway_OffMeansNoHandlerAndNoHealthzBlock(t *testing.T) {
	srv := New(Config{})
	if h := srv.UIGatewayHandler(); h != nil {
		t.Fatal("UIGatewayHandler returned a handler with the gateway disabled")
	}
	if srv.uiSandboxHealthz() != nil {
		t.Fatal("ui_sandbox healthz block present with the gateway disabled")
	}
	// A listen address without a signing key must ALSO stay off: a relay cookie
	// that cannot be signed must never be issued.
	if New(Config{UIListenAddr: ":8081"}).UIGatewayHandler() != nil {
		t.Fatal("gateway enabled with no session key")
	}
}

// TestUIGateway_HealthzPublishesEnterTemplate: the console gets exactly one
// field to build its Open link from, on the gateway's own origin.
func TestUIGateway_HealthzPublishesEnterTemplate(t *testing.T) {
	h := newUIHarness(t, okBackend())
	block := h.srv.uiSandboxHealthz()
	tmpl, _ := block["enter_url_template"].(string)
	for _, want := range []string{"https://ui.example.com", uiEnterPath, "{run}", "{app}", "{ticket}"} {
		if !strings.Contains(tmpl, want) {
			t.Fatalf("enter_url_template %q missing %q", tmpl, want)
		}
	}
	if block["host_mode"] != false {
		t.Fatalf("host_mode = %v, want false without an origin template", block["host_mode"])
	}
}

// TestUIGateway_ConsoleOriginHasNoRelayRoutes is the origin-separation test: a
// relayed page is the sandbox's own code, so if these paths answered on the
// console router, that code would run on the console origin.
func TestUIGateway_ConsoleOriginHasNoRelayRoutes(t *testing.T) {
	h := newUIHarness(t, okBackend())
	for _, path := range []string{
		uiEnterPath + "?run=" + h.run.ID.String() + "&app=code",
		uiRunPrefix + h.run.ID.String() + "/ide",
	} {
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("console router answered %s with %d, want 404", path, rec.Code)
		}
	}
}

// ─── enter: the ticket is the only way in ────────────────────────────────────

// TestUIGateway_EnterRejectsBadTickets covers every way a ticket can fail to
// authorize: absent, garbage, already used, and minted for another run. All
// refuse identically — there is no fallback auth on this listener and no
// oracle to distinguish the cases.
func TestUIGateway_EnterRejectsBadTickets(t *testing.T) {
	h := newUIHarness(t, okBackend())
	used := h.ticket(h.run.ID, h.owner, oidc.RoleMember)
	if rec := h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {used}}); rec.Code != http.StatusFound {
		t.Fatalf("first redemption: %d", rec.Code)
	}
	otherRun := types.AgentRun{ID: uuid.New(), CreatedBy: h.owner, State: types.RunRunning, SandboxRef: "sandbox-2"}
	h.store.putRun(otherRun)

	cases := map[string]url.Values{
		"no ticket":                     {"run": {h.run.ID.String()}, "app": {"code"}},
		"garbage ticket":                {"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {"deadbeef"}},
		"reused ticket":                 {"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {used}},
		"ticket minted for another run": {"run": {otherRun.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleMember)}},
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			if rec := h.enter(q); rec.Code != http.StatusForbidden {
				t.Fatalf("got %d %s, want 403", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestUIGateway_EnterRejectsNonOwnerTicket: minting is owner-or-admin, but a
// member CAN hold a ticket for a run they own — the gateway re-checks the
// ticket's stamped principal/role against the freshly loaded run, exactly as
// the attach WebSocket does, because this lane runs no auth middleware at all.
func TestUIGateway_EnterRejectsNonOwnerTicket(t *testing.T) {
	h := newUIHarness(t, okBackend())
	rec := h.enter(url.Values{
		"run": {h.run.ID.String()}, "app": {"code"},
		"ticket": {h.ticket(h.run.ID, "mallory", oidc.RoleMember)},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner ticket: %d %s", rec.Code, rec.Body.String())
	}
	// An ADMIN's ticket for someone else's run is accepted (owner-OR-admin,
	// the same authorization the ticket endpoint itself applies).
	rec = h.enter(url.Values{
		"run": {h.run.ID.String()}, "app": {"code"},
		"ticket": {h.ticket(h.run.ID, "root", oidc.RoleAdmin)},
	})
	if rec.Code != http.StatusFound {
		t.Fatalf("admin ticket: %d %s", rec.Code, rec.Body.String())
	}
}

// TestUIGateway_EnterRequiresDeclaredApp: the relay serves ONLY a port the
// run's effective policy declared. An undeclared name is refused naming the
// field, and a run whose policy declares nothing has no apps at all.
func TestUIGateway_EnterRequiresDeclaredApp(t *testing.T) {
	h := newUIHarness(t, okBackend())
	rec := h.enter(url.Values{
		"run": {h.run.ID.String()}, "app": {"secretsrv"},
		"ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleMember)},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("undeclared app: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ui_apps") {
		t.Fatalf("refusal does not name the policy field: %s", rec.Body.String())
	}

	// A run with no run.policy.effective envelope at all (never dispatched, or
	// the audit store unavailable) must fail closed the same way.
	bare := types.AgentRun{ID: uuid.New(), CreatedBy: h.owner, State: types.RunRunning, SandboxRef: "sandbox-3"}
	h.store.putRun(bare)
	rec = h.enter(url.Values{
		"run": {bare.ID.String()}, "app": {"code"},
		"ticket": {h.ticket(bare.ID, h.owner, oidc.RoleMember)},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("run with no effective policy: %d", rec.Code)
	}
}

// TestUIGateway_EnterRequiresRunningRun: a stopped run has no sandbox to relay
// into, and a valid ticket must not paper over that.
func TestUIGateway_EnterRequiresRunningRun(t *testing.T) {
	h := newUIHarness(t, okBackend())
	stopped := h.run
	stopped.State = types.RunStopped
	h.store.putRun(stopped)
	rec := h.enter(url.Values{
		"run": {h.run.ID.String()}, "app": {"code"},
		"ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleMember)},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stopped run: %d %s", rec.Code, rec.Body.String())
	}
}

// TestUIGateway_EnterSetsRunScopedCookie pins the cookie's attributes. Path
// scoping is the control that stops one run's page from making the browser
// attach another run's session on a shared origin; HttpOnly stops relayed
// script from reading it at all.
func TestUIGateway_EnterSetsRunScopedCookie(t *testing.T) {
	h := newUIHarness(t, okBackend())
	rec := h.enter(url.Values{
		"run": {h.run.ID.String()}, "app": {"code"},
		"ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleMember)},
	})
	if rec.Code != http.StatusFound {
		t.Fatalf("enter: %d %s", rec.Code, rec.Body.String())
	}
	if got, want := rec.Header().Get("Location"), uiRunPrefix+h.run.ID.String()+"/ide"; got != want {
		t.Fatalf("Location %q, want %q (the app's declared path)", got, want)
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("enter response leaks a referrer (the URL carries a ticket)")
	}
	var c *http.Cookie
	for _, got := range rec.Result().Cookies() {
		if got.Name == uiCookieName {
			c = got
		}
	}
	if c == nil {
		t.Fatal("no relay cookie")
	}
	if c.Path != uiRunPrefix+h.run.ID.String()+"/" {
		t.Fatalf("cookie path %q is not scoped to the run", c.Path)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags: HttpOnly=%v SameSite=%v", c.HttpOnly, c.SameSite)
	}
}

// TestUIGateway_HostModeBindsEnterToTheRunsOrigin: with a per-run origin
// template, an enter served on any other host is refused — otherwise one run's
// cookie would be minted on another run's origin, undoing the isolation the
// template exists to provide.
func TestUIGateway_HostModeBindsEnterToTheRunsOrigin(t *testing.T) {
	h := newUIHarness(t, okBackend())
	h.srv.cfg.UIOriginTemplate = "https://run-{run}.ui.example.com"
	q := url.Values{
		"run": {h.run.ID.String()}, "app": {"code"},
		"ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleMember)},
	}
	req := httptest.NewRequest(http.MethodGet, uiEnterPath+"?"+q.Encode(), nil)
	req.Host = "run-" + uuid.New().String() + ".ui.example.com"
	rec := httptest.NewRecorder()
	h.gateway.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("enter on another run's host: %d %s", rec.Code, rec.Body.String())
	}

	q.Set("ticket", h.ticket(h.run.ID, h.owner, oidc.RoleMember))
	req = httptest.NewRequest(http.MethodGet, uiEnterPath+"?"+q.Encode(), nil)
	req.Host = "run-" + h.run.ID.String() + ".ui.example.com"
	rec = httptest.NewRecorder()
	h.gateway.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("enter on the run's own host: %d %s", rec.Code, rec.Body.String())
	}
}

// ─── relay: the cookie is the only credential ────────────────────────────────

// TestUIGateway_RelayRequiresAValidSessionForThisRun: no cookie, a forged one,
// an expired one, and another run's cookie all fail — and none of them fall
// through to any console credential.
func TestUIGateway_RelayRequiresAValidSessionForThisRun(t *testing.T) {
	h := newUIHarness(t, okBackend())
	good := h.openSession()

	forged := *good
	flipped := []byte(good.Value)
	flipped[0] ^= 0xff
	forged.Value = string(flipped)

	expired := &http.Cookie{Name: uiCookieName, Value: h.srv.encodeUISession(uiSession{
		Run: h.run.ID, App: "code", Port: uiTestPort, Principal: h.owner,
		Expires: time.Now().Add(-time.Minute).Unix(),
	})}
	otherRun := &http.Cookie{Name: uiCookieName, Value: h.srv.encodeUISession(uiSession{
		Run: uuid.New(), App: "code", Port: uiTestPort, Principal: h.owner,
		Expires: time.Now().Add(time.Hour).Unix(),
	})}

	for name, c := range map[string]*http.Cookie{
		"no cookie":            nil,
		"forged signature":     &forged,
		"expired session":      expired,
		"another run's cookie": otherRun,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := h.relay("/ide", c, nil); rec.Code != http.StatusForbidden {
				t.Fatalf("got %d %s, want 403", rec.Code, rec.Body.String())
			}
		})
	}

	// The bearer token that authenticates the CONSOLE must not authenticate
	// here: this listener has exactly one mechanism.
	rec := h.relay("/ide", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer admin-token") })
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bearer token accepted on the relay: %d", rec.Code)
	}

	if rec := h.relay("/ide", good, nil); rec.Code != http.StatusOK {
		t.Fatalf("valid session: %d %s", rec.Code, rec.Body.String())
	}
}

// TestUIGateway_RelayStripsWardynCredentialsInbound: cookies are not
// port-scoped, so a browser hands EVERY wardyn_* cookie for the hostname to a
// request bound for sandbox-authored code. They must come off, along with
// Authorization and any ?ticket — while an app's OWN cookies pass through
// untouched (the app has to be usable).
func TestUIGateway_RelayStripsWardynCredentialsInbound(t *testing.T) {
	seen := make(chan *http.Request, 1)
	h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Clone(context.Background())
		_, _ = io.WriteString(w, "ok")
	}))
	cookie := h.openSession()

	rec := h.relay("/ide?ticket=leaked&folder=/work", cookie, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer console-admin-token")
		r.Header.Set("Proxy-Authorization", "Basic x")
		r.AddCookie(&http.Cookie{Name: "wardyn_session", Value: "console-session"})
		r.AddCookie(&http.Cookie{Name: "app_theme", Value: "dark"})
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("relay: %d %s", rec.Code, rec.Body.String())
	}
	got := <-seen
	if got.Header.Get("Authorization") != "" || got.Header.Get("Proxy-Authorization") != "" {
		t.Fatalf("credential headers reached the sandbox: %v", got.Header)
	}
	for _, c := range got.Cookies() {
		if strings.HasPrefix(strings.ToLower(c.Name), uiCookiePrefix) {
			t.Fatalf("wardyn cookie %q reached the sandbox", c.Name)
		}
	}
	if _, err := got.Cookie("app_theme"); err != nil {
		t.Fatalf("the app's own cookie was stripped too: %v", err)
	}
	if got.URL.Query().Has("ticket") {
		t.Fatalf("?ticket reached the sandbox: %s", got.URL.RawQuery)
	}
	if got.URL.Query().Get("folder") != "/work" {
		t.Fatalf("the app's own query was mangled: %s", got.URL.RawQuery)
	}
	if got.URL.Path != "/ide" {
		t.Fatalf("upstream path %q, want the /r/<run> prefix removed", got.URL.Path)
	}
	if got.Header.Get("X-Forwarded-For") != "" {
		t.Fatal("the operator's address was forwarded into the sandbox")
	}
}

// TestUIGateway_RelayDropsSandboxWardynCookiesOutbound: cookie tossing. An app
// in the sandbox that could set (or clear) wardyn_ui_sess — or the console's
// session cookie on a shared host — would be mounting an authentication
// attack, not a rendering quirk.
func TestUIGateway_RelayDropsSandboxWardynCookiesOutbound(t *testing.T) {
	h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "wardyn_ui_sess=attacker-chosen; Path=/")
		w.Header().Add("Set-Cookie", "WARDYN_session=attacker-chosen; Path=/")
		w.Header().Add("Set-Cookie", "app_theme=dark; Path=/")
		_, _ = io.WriteString(w, "ok")
	}))
	rec := h.relay("/ide", h.openSession(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("relay: %d", rec.Code)
	}
	for _, sc := range rec.Header().Values("Set-Cookie") {
		if strings.HasPrefix(strings.ToLower(sc), uiCookiePrefix) {
			t.Fatalf("sandbox set a wardyn cookie in the browser: %q", sc)
		}
	}
	if len(rec.Header().Values("Set-Cookie")) != 1 {
		t.Fatalf("app cookies: %v — the app's own Set-Cookie must survive", rec.Header().Values("Set-Cookie"))
	}
}

// ─── launcher ────────────────────────────────────────────────────────────────

// TestUIGateway_MissingLauncherIs502WithTheFrozenMessage: the BYOI case an
// operator actually hits. The body is a frozen string the console prints
// verbatim (docs/design/ui-sandboxes-prompt.md §7) — changing it silently
// breaks that contract, so it is asserted byte for byte.
func TestUIGateway_MissingLauncherIs502WithTheFrozenMessage(t *testing.T) {
	h := newUIHarness(t, okBackend())
	cookie := h.openSession()
	h.launcher = 3 // the probe found no executable at the convention path

	rec := h.relay("/ide", cookie, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d %s, want 502", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if want := "no UI launcher in this image: /usr/local/bin/wardyn-ui-code not found"; body.Error != want {
		t.Fatalf("error body:\n got %q\nwant %q", body.Error, want)
	}
}

// TestUIGateway_LaunchIsAudited: exit 5 means the probe actually STARTED the
// app (exit 0 means it was already listening), which is the only case that has
// anything to record — ui.start names an app that was launched, never one that
// was merely reached.
func TestUIGateway_LaunchIsAudited(t *testing.T) {
	h := newUIHarness(t, okBackend())
	cookie := h.openSession()
	h.launcher = 5
	if rec := h.relay("/ide", cookie, nil); rec.Code != http.StatusOK {
		t.Fatalf("relay: %d %s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(h.audit.actions(), " "); !strings.Contains(got, "ui.start/success") {
		t.Fatalf("audit %q missing ui.start/success", got)
	}
}

// TestUIGateway_LauncherTimeoutIs502: the launcher ran but nothing opened the
// port — a clean 502 naming the port and the wait, never a hung request.
func TestUIGateway_LauncherTimeoutIs502(t *testing.T) {
	h := newUIHarness(t, okBackend())
	cookie := h.openSession()
	h.launcher = 4
	rec := h.relay("/ide", cookie, nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "127.0.0.1:8080") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// TestUIGateway_LauncherScriptShape pins the three exit codes the Go side
// interprets, and that the probe runs BEFORE any launch (an app already
// listening must never be started twice).
func TestUIGateway_LauncherScriptShape(t *testing.T) {
	script := uiLauncherScript("/usr/local/bin/wardyn-ui-code", 8080)
	probeAt := strings.Index(script, "socat")
	launchAt := strings.Index(script, `"$b" >/dev/null`)
	if probeAt < 0 || launchAt < 0 || probeAt > launchAt {
		t.Fatalf("probe must precede launch:\n%s", script)
	}
	for _, want := range []string{"exit 3", "exit 4", "exit 5", `[ -x "$b" ]`} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

// ─── bounds ──────────────────────────────────────────────────────────────────

// TestUIGateway_PerRunConnectionCap: every relay connection is a live exec in
// the sandbox, so the count is bounded per run and slots come back on close.
func TestUIGateway_PerRunConnectionCap(t *testing.T) {
	h := newUIHarness(t, okBackend())
	runID := h.run.ID
	releases := make([]func(), 0, maxUIConnsPerRun)
	for i := range maxUIConnsPerRun {
		release, ok := h.srv.acquireUIConn(runID)
		if !ok {
			t.Fatalf("slot %d refused below the cap", i)
		}
		releases = append(releases, release)
	}
	if _, ok := h.srv.acquireUIConn(runID); ok {
		t.Fatalf("slot %d granted above the cap of %d", maxUIConnsPerRun+1, maxUIConnsPerRun)
	}
	// A different run is unaffected — the bound is per run, not global.
	if _, ok := h.srv.acquireUIConn(uuid.New()); !ok {
		t.Fatal("another run's first slot refused")
	}
	releases[0]()
	releases[0]() // idempotent: net/http closes a conn from both loops
	if _, ok := h.srv.acquireUIConn(runID); !ok {
		t.Fatal("released slot was not returned")
	}
}

// TestUIGateway_RelayTouchesTheRun: a human reading code in an editor is not
// idle. The touch is debounced (one per window), like every other keepalive.
func TestUIGateway_RelayTouchesTheRun(t *testing.T) {
	h := newUIHarness(t, okBackend())
	cookie := h.openSession()
	if rec := h.relay("/ide", cookie, nil); rec.Code != http.StatusOK {
		t.Fatalf("relay: %d", rec.Code)
	}
	if !h.store.wasTouched() {
		t.Fatal("a relayed request did not reset the run's idle clock")
	}
}

// ─── audit ───────────────────────────────────────────────────────────────────

// TestUIGateway_AuditsAuthAndSessionWithoutContent: the ui.* actions record
// THAT a human opened an app, never what they did in it — and they are
// deliberately not session.attach, which would put a relay session in the
// recording picker as if a recording existed.
func TestUIGateway_AuditsAuthAndSessionWithoutContent(t *testing.T) {
	h := newUIHarness(t, okBackend())
	cookie := h.openSession()
	if rec := h.relay("/ide", cookie, nil); rec.Code != http.StatusOK {
		t.Fatalf("relay: %d", rec.Code)
	}
	h.enter(url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {"bogus"}})

	got := strings.Join(h.audit.actions(), " ")
	for _, want := range []string{"ui.auth/success", "ui.auth/denied", "ui.open/success"} {
		if !strings.Contains(got, want) {
			t.Fatalf("audit %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "session.attach") {
		t.Fatalf("a relay session was audited as an attach: %q", got)
	}
}

// ─── session cookie ──────────────────────────────────────────────────────────

// TestUIGateway_SessionCookieIsSignedAndBounded: the cookie is the whole
// credential, so a flipped byte, a swapped key, or a passed expiry must all
// fail to decode.
func TestUIGateway_SessionCookieIsSignedAndBounded(t *testing.T) {
	h := newUIHarness(t, okBackend())
	now := time.Now()
	sess := uiSession{Run: h.run.ID, App: "code", Port: uiTestPort, Principal: "alice", Expires: now.Add(time.Hour).Unix()}
	value := h.srv.encodeUISession(sess)

	req := func(v string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: uiCookieName, Value: v})
		return r
	}
	if got, ok := h.srv.decodeUISession(req(value), now); !ok || got.Run != sess.Run || got.Port != sess.Port {
		t.Fatalf("round trip failed: %+v ok=%v", got, ok)
	}
	if _, ok := h.srv.decodeUISession(req(value), now.Add(2*time.Hour)); ok {
		t.Fatal("expired session accepted")
	}
	tampered := []byte(value)
	tampered[0] ^= 0xff
	if _, ok := h.srv.decodeUISession(req(string(tampered)), now); ok {
		t.Fatal("tampered payload accepted")
	}
	other := New(Config{UIListenAddr: ":8081", UISessionKey: make([]byte, 32)})
	if _, ok := other.decodeUISession(req(value), now); ok {
		t.Fatal("a cookie signed under another key was accepted")
	}
}
