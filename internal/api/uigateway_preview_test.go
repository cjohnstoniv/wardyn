// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The gateway's preview tests turn the key on with withSiteFeatureHooks, so none of them runs in parallel.

// newPreviewRigOn is newPreviewRig with backend as the sandbox's app.
func newPreviewRigOn(t *testing.T, backend http.Handler) *previewRig {
	t.Helper()
	withPreviewsAvailable(t)
	h := newUIHarness(t, backend)
	h.srv.cfg.UIOriginTemplate = "https://{run}.ui.example.com"
	st := &previewTestStore{uiMemStore: h.store, previewRows: &previewRows{}, site: previewSite(true, "org")}
	h.srv.cfg.Store = st
	return &previewRig{t: t, h: h, st: st}
}

// runHost is the run's own host, which host mode serves its apps on.
func (g *previewRig) runHost() string { return g.h.run.ID.String() + ".ui.example.com" }

// previewCookie is the relay cookie the gateway sets for principal on the preview of port, entered as caller
// ("" for the run's entry rule, which an owner app's session carries; an inbound app's caller otherwise).
func (g *previewRig) previewCookie(port int, principal, caller string) *http.Cookie {
	now := g.h.clock.now()
	return &http.Cookie{Name: uiCookieName, Value: g.h.srv.encodeUISession(uiSession{Run: g.h.run.ID, App: previewApp(port), Port: port,
		Principal: principal, Expires: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(), AuthorizedAt: now.Add(-time.Minute).Unix(),
		Inbound: caller != "", Caller: caller})}
}

// openPreview mints who's ticket for the preview of port and enters it on the run's host, failing unless a relay
// cookie is set.
func (g *previewRig) openPreview(who previewWho, port int) *http.Cookie {
	g.t.Helper()
	rec := g.h.enterOnRunHost(previewApp(port), ticketOf(g.t, g.ticket(who, strconv.Itoa(port))))
	if rec.Code != http.StatusFound {
		g.t.Fatalf("enter the preview of %d: %d %s, want 302", port, rec.Code, rec.Body.String())
	}
	for _, c := range setUICookies(rec) {
		if c.Name == uiCookieName {
			return c
		}
	}
	g.t.Fatal("the preview's enter set no relay cookie")
	return nil
}

// relayPreview is one request to the preview of port on the run's host, with mutate applied.
func (g *previewRig) relayPreview(port int, path string, c *http.Cookie, mutate func(*http.Request)) *httptest.ResponseRecorder {
	g.t.Helper()
	req := httptest.NewRequest(http.MethodGet, uiRelayPrefix(g.h.run.ID, previewApp(port))+path, nil)
	req.Host = g.runHost()
	if c != nil {
		req.AddCookie(c)
	}
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	g.h.gateway.ServeHTTP(rec, req)
	return rec
}

// dropIdleConns closes the relay's pooled connections, so the next request dials.
func (g *previewRig) dropIdleConns() {
	if tr, ok := g.h.srv.uiReverseProxy().Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func wantRelay(t *testing.T, what string, rec *httptest.ResponseRecorder, status int, reason string) {
	t.Helper()
	if rec.Code != status || (reason != "" && respondedReason(t, rec) != reason) {
		t.Fatalf("%s: %d %s, want %d %s", what, rec.Code, rec.Body.String(), status, reason)
	}
}

// TestPreviewSwitchOffServesNothing: withPreviewApps adds a private row as an owner app and an org row as an
// inbound app whose callers list is the one reserved entry, never writing into the caller's array. With the
// switch off or the site config unreadable it adds nothing, and an open session is refused at its re-check.
func TestPreviewSwitchOffServesNothing(t *testing.T) {
	g := newPreviewRig(t)
	g.st.putPreview(g.h.run.ID, 3000, types.PreviewPrivate)
	g.st.putPreview(g.h.run.ID, 3001, types.PreviewOrg)
	declared := make([]types.UIApp, 1, 4)
	declared[0] = types.UIApp{Name: "code", Port: uiTestPort}
	apps := g.h.srv.withPreviewApps(context.Background(), g.h.run.ID, declared)
	if len(apps) != 3 || apps[1].Name != "__preview-3000" || apps[1].Port != 3000 || apps[1].Path != "/" || apps[1].Inbound ||
		apps[2].Name != "__preview-3001" || !apps[2].Inbound || len(apps[2].Callers) != 1 || apps[2].Callers[0] != previewCallerOrg {
		t.Fatalf("withPreviewApps = %+v, want code, an owner app on 3000 and an org caller's inbound app on 3001", apps)
	}
	if spare := declared[:2]; spare[1].Name != "" {
		t.Fatalf("withPreviewApps wrote into the caller's array: %+v", spare[1])
	}
	c := g.previewCookie(3000, "alice", "")
	wantRelay(t, "switch on", g.relayPreview(3000, "/", c, nil), http.StatusOK, "")
	for name, set := range map[string]func(){
		"switch off":               func() { g.st.setSite(previewSite(false, "org"), nil) },
		"site config unreadable":   func() { g.st.setSite(types.SiteConfig{}, context.DeadlineExceeded) },
		"no features block at all": func() { g.st.setSite(types.SiteConfig{}, nil) },
	} {
		set()
		if got := g.h.srv.withPreviewApps(context.Background(), g.h.run.ID, declared[:1]); len(got) != 1 {
			t.Errorf("%s: withPreviewApps = %+v, want the declared app alone", name, got)
		}
		g.h.clock.advance(time.Minute) // past the session's re-check debounce
		if rec := g.relayPreview(3000, "/", c, nil); rec.Code != http.StatusForbidden && rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: an open session's request = %d %s, want it refused", name, rec.Code, rec.Body.String())
		}
	}
}

// TestAppTicketNeverMintsPreviewCaller: the generic app ticket route knows no preview caller, so a person who
// is not the run's owner gets its foreign-run 404 for an org preview; the owner's ticket there is an owner ticket.
func TestAppTicketNeverMintsPreviewCaller(t *testing.T) {
	g := newPreviewRig(t)
	g.st.putPreview(g.h.run.ID, 3000, types.PreviewOrg)
	for _, groups := range [][]string{nil, {"preview:org"}, {"org"}} {
		if rec := g.h.appTicketAs(previewApp(3000), "bob", groups); rec.Code != http.StatusNotFound {
			t.Fatalf("bob in groups %v: %d %s, want the foreign-run 404", groups, rec.Code, rec.Body.String())
		}
	}
	if tk := g.stored(g.h.appTicketAs(previewApp(3000), "alice", nil)); tk.Caller != "" || tk.App != previewApp(3000) {
		t.Fatalf("the owner's app ticket = %+v, want an owner ticket for the preview", tk)
	}
	g.h.store.mu.Lock()
	defer g.h.store.mu.Unlock()
	for _, tk := range g.h.store.tickets {
		if strings.HasPrefix(tk.Caller, "preview:") {
			t.Fatalf("the app ticket route minted a preview caller's ticket: %+v", tk)
		}
	}
}

// previewTokenStore is the inbound token store with the run_previews table and a site config with previews on.
type previewTokenStore struct {
	*uiTokenStore
	*previewRows
}

func (s *previewTokenStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return previewSite(true, "org"), nil
}

// TestInboundTokenNeverReachesPreview: an inbound token reaches an app only through a callers entry it matches
// or the owner rule, and no token matches a preview's reserved entry, so another person's token gets the one 404
// for an org preview and for a private one, and the sandbox is never reached.
func TestInboundTokenNeverReachesPreview(t *testing.T) {
	var hits atomic.Int32
	g := newPreviewRigOn(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "sandbox app")
	}))
	g.st.putPreview(g.h.run.ID, 3000, types.PreviewOrg)
	g.st.putPreview(g.h.run.ID, 3001, types.PreviewPrivate)
	token := types.APIToken{ID: uuid.New(), Principal: "bob", Email: "bob@example.com", Kind: types.APITokenKindInbound, CreatedAt: time.Now().Add(-time.Hour)}
	g.h.srv.cfg.Store = &previewTokenStore{uiTokenStore: &uiTokenStore{uiMemStore: g.h.store, tokens: map[string]types.APIToken{"wdn_bob": token}},
		previewRows: g.st.previewRows}
	for _, port := range []int{3000, 3001} {
		rec := g.relayPreview(port, "/", nil, func(r *http.Request) { r.Header.Set("Authorization", "Bearer wdn_bob") })
		if rec.Code != http.StatusNotFound {
			t.Errorf("bob's inbound token on the preview of %d: %d %s, want the one 404", port, rec.Code, rec.Body.String())
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the sandbox was reached %d times", n)
	}
}

// TestPreviewNeverTheInboundException: an org preview is an inbound app, which a background-only run keeps as
// its one door; a preview is never that door, for an org caller's session or the owner's.
func TestPreviewNeverTheInboundException(t *testing.T) {
	g := newPreviewRig(t)
	g.st.putPreview(g.h.run.ID, 3000, types.PreviewOrg)
	g.set(func(run *types.AgentRun) { run.Experience = types.ExperienceBackground })
	wantRelay(t, "an org caller", g.relayPreview(3000, "/", g.previewCookie(3000, "bob", previewCallerOrg), nil), http.StatusConflict, reasonRunBackgroundOnly)
	wantRelay(t, "the owner", g.relayPreview(3000, "/", g.previewCookie(3000, "alice", ""), nil), http.StatusConflict, reasonRunBackgroundOnly)
}

// TestPreviewCrossSiteRefused: in host mode a cookie request to an org preview from another site is refused,
// as the inbound rule refuses one to any app; the same session's own request is relayed.
func TestPreviewCrossSiteRefused(t *testing.T) {
	g := newPreviewRig(t)
	g.st.putPreview(g.h.run.ID, 3000, types.PreviewOrg)
	c := g.previewCookie(3000, "bob", previewCallerOrg)
	other := "https://" + uuid.NewString() + ".ui.example.com"
	rec := g.relayPreview(3000, "/", c, func(r *http.Request) {
		r.Header.Set("Sec-Fetch-Site", "same-site")
		r.Header.Set("Sec-Fetch-Mode", "websocket")
		r.Header.Set("Origin", other)
	})
	wantRelay(t, "a sibling run's request", rec, http.StatusForbidden, reasonUIGatewayCrossSite)
	wantRelay(t, "the session's own request", g.relayPreview(3000, "/", c, nil), http.StatusOK, "")
}

// TestPreviewRefusesSupportSession: a helper whose live grant ticks web apps opens the run's declared app, and
// never a preview of the same run, a private one included.
func TestPreviewRefusesSupportSession(t *testing.T) {
	rig := newSupportUIRig(t, string(supportDoorWebApps))
	withPreviewsAvailable(t)
	rows := &previewRows{}
	rows.putPreview(rig.run.ID, 3000, types.PreviewPrivate)
	rig.srv.cfg.Store = &previewSupportStore{supportGrantedStore: rig.srv.cfg.Store.(*supportGrantedStore), previewRows: rows}
	if rec := rig.get("code", "/", rig.helperSession("code")); rec.Code != http.StatusOK {
		t.Fatalf("the helper on the declared app: %d %s, want 200", rec.Code, rec.Body.String())
	}
	rec := rig.enterApp(rig.helperTicket(), previewApp(3000))
	if rec.Code != http.StatusFound {
		t.Fatalf("the helper's enter of the preview: %d %s, want 302 (the enter runs no arm; the first request is refused)", rec.Code, rec.Body.String())
	}
	cookies := setUICookies(rec)
	if len(cookies) == 0 {
		t.Fatal("an admitted enter set no cookie")
	}
	if got := rig.get(previewApp(3000), "/", cookies[0]); got.Code != http.StatusForbidden || respondedReason(t, got) != uiDeniedReasonNotAuthorized {
		t.Fatalf("the helper's request to the preview: %d %s, want 403 %s", got.Code, got.Body.String(), uiDeniedReasonNotAuthorized)
	}
}

// previewSupportStore is the support rig's store with the run_previews table and previews on.
type previewSupportStore struct {
	*supportGrantedStore
	*previewRows
}

func (s *previewSupportStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return previewSite(true, "org"), nil
}

// previewPresenceStore is the preview rig's store that counts the presence stamps and thaws a person's traffic
// makes (stampPresence, resumeRun). Its other pause methods are never reached here.
type previewPresenceStore struct {
	*previewTestStore
	store.RunPauser
	mu             sync.Mutex
	stamps, clears int
}

func (s *previewPresenceStore) StampRunActive(context.Context, uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stamps++
	return false, nil
}

func (s *previewPresenceStore) ClearRunPaused(context.Context, uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clears++
	return false, nil
}

func (s *previewPresenceStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stamps, s.clears
}

// TestPreviewViewerIsNotPresence: a preview opened through its ticket and relayed any number of times never
// touches the run's idle clock or stamps its presence, and a paused run answers 409 preview_run_paused and stays
// paused. The run's declared app, on the same run and store, does both, which proves the counters see them.
func TestPreviewViewerIsNotPresence(t *testing.T) {
	g := newPreviewRig(t)
	ps := &previewPresenceStore{previewTestStore: g.st}
	g.h.srv.cfg.Store = ps
	g.st.putPreview(g.h.run.ID, 3000, types.PreviewPrivate)
	c := g.openPreview(previewOwner, 3000)
	for i := range 5 {
		g.dropIdleConns() // every other request dials anew
		if rec := g.relayPreview(3000, "/", c, nil); rec.Code != http.StatusOK || rec.Body.String() != "sandbox app" {
			t.Fatalf("request %d: %d %s, want the app", i, rec.Code, rec.Body.String())
		}
	}
	if stamps, _ := ps.counts(); g.h.store.touchCount() != 0 || stamps != 0 {
		t.Fatalf("preview requests touched the run %d times and stamped its presence %d times, want neither", g.h.store.touchCount(), stamps)
	}

	paused := time.Now().UTC()
	g.set(func(run *types.AgentRun) { run.PausedAt = &paused })
	g.dropIdleConns()
	wantRelay(t, "a paused run's preview", g.relayPreview(3000, "/", c, nil), http.StatusConflict, reasonPreviewRunPaused)
	if run, err := g.h.store.GetRun(context.Background(), g.h.run.ID); err != nil || run.PausedAt == nil {
		t.Fatalf("the run after a preview request: paused_at %v (%v), want it still paused", run.PausedAt, err)
	}
	if stamps, clears := ps.counts(); g.h.store.touchCount() != 0 || stamps != 0 || clears != 0 {
		t.Fatalf("a paused run's preview: %d touches, %d stamps, %d resumes; want none", g.h.store.touchCount(), stamps, clears)
	}

	g.set(func(run *types.AgentRun) { run.PausedAt = nil })
	rec := g.h.enterOnRunHost("code", g.h.ticket(g.h.run.ID, g.h.owner, oidc.RoleUser))
	var code *http.Cookie
	for _, ck := range setUICookies(rec) {
		if ck.Name == uiCookieName {
			code = ck
		}
	}
	if code == nil {
		t.Fatalf("enter the declared app: %d %s, want a relay cookie", rec.Code, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, uiRelayPrefix(g.h.run.ID, "code")+"/", nil)
	req.Host = g.runHost()
	req.AddCookie(code)
	got := httptest.NewRecorder()
	g.h.gateway.ServeHTTP(got, req)
	if stamps, _ := ps.counts(); got.Code != http.StatusOK || g.h.store.touchCount() == 0 || stamps == 0 {
		t.Fatalf("the declared app: %d, %d touches, %d stamps; want 200, both counted", got.Code, g.h.store.touchCount(), stamps)
	}
}

// previewIdleStore is the preview rig's store whose TouchRun is the idle fixture's (idleStore's): it moves
// updated_at to the fixture's virtual now.
type previewIdleStore struct {
	*previewTestStore
	idle *idleStore
}

func (s *previewIdleStore) TouchRun(ctx context.Context, id uuid.UUID) error {
	return s.idle.TouchRun(ctx, id)
}

// TestPreviewConnectionHeldAcrossIdleWindowLeavesLastActivityUnchanged: a preview's connection, dialled by uiDial
// and held open across many keepalive ticks, never moves the run's last activity, and the real reaper stops the
// run on schedule while it is still open. The same dial and hold for the run's declared app is touched on every
// tick, which proves the hold spans them.
func TestPreviewConnectionHeldAcrossIdleWindowLeavesLastActivityUnchanged(t *testing.T) {
	dial := func(t *testing.T, app string, port int) (*idleFixture, net.Conn) {
		t.Helper()
		g := newPreviewRig(t)
		f := newIdleFixture(g.h.run.ID)
		g.h.srv.cfg.Store = &previewIdleStore{previewTestStore: g.st, idle: &idleStore{f: f}}
		g.h.srv.keepaliveEvery = idleKeepalive
		g.st.putPreview(g.h.run.ID, 3000, types.PreviewPrivate)
		now := g.h.clock.now()
		ctx := context.WithValue(context.Background(), uiSessionCtxKey{}, uiSession{Run: g.h.run.ID, App: app, Port: port, Principal: g.h.owner,
			Expires: now.Add(time.Hour).Unix(), IssuedAt: now.Unix(), AuthorizedAt: now.Add(-time.Minute).Unix()})
		conn, err := g.h.srv.uiDial(ctx, "tcp", net.JoinHostPort(g.h.run.ID.String(), strconv.Itoa(port)))
		if err != nil {
			t.Fatalf("dial %s: %v", app, err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return f, conn
	}

	f, _ := dial(t, previewApp(3000), 3000)
	time.Sleep(20 * idleKeepalive)
	f.mu.Lock()
	touches, unchanged := f.touches, f.updated.Equal(f.now)
	f.mu.Unlock()
	if touches != 0 || !unchanged {
		t.Fatalf("a preview's connection held for %v: %d touches, last activity unchanged %v; want none, unchanged", 20*idleKeepalive, touches, unchanged)
	}
	f.advance(2*time.Hour + lifecycle.TouchDebounce)
	if evs := f.reap(t); len(evs) != 1 || !f.isStopped() {
		t.Fatalf("the reaper with only a preview's connection open: stopped=%v, run.autostop events=%d; want it stopped once", f.isStopped(), len(evs))
	}

	control, _ := dial(t, "code", uiTestPort)
	time.Sleep(20 * idleKeepalive)
	control.waitTicking()
	if n := control.touchCount(); n < 3 {
		t.Fatalf("the declared app's connection held for %v was touched %d times, want at least 3", 20*idleKeepalive, n)
	}
}

// TestPreviewNeverStartsALauncher: a preview's port is probed and never launched, even when the image ships a
// launcher at the preview app's convention path, so a port nothing listens on is 502 preview_not_listening with
// no ui.start row. Once the port listens, the probe answers, and a second dial within the probe's cache runs no
// exec.
func TestPreviewNeverStartsALauncher(t *testing.T) {
	g := newPreviewRig(t)
	g.st.putPreview(g.h.run.ID, 3000, types.PreviewPrivate)
	var (
		mu        sync.Mutex
		scripts   []string
		listening bool
	)
	relay := g.h.runner.execFn
	g.h.runner.execFn = func(spec runner.ExecSpec) (*runner.ExecSession, error) {
		if spec.Argv[0] != "sh" {
			return relay(spec)
		}
		script := strings.Join(spec.Argv, " ")
		mu.Lock()
		scripts = append(scripts, script)
		code := 1
		switch {
		case strings.Contains(script, uiLauncherPrefix): // the planted launcher runs and its port comes up
			code = 5
		case listening:
			code = 0
		}
		mu.Unlock()
		return &runner.ExecSession{Wait: func() (int, error) { return code, nil }}, nil
	}
	shells := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), scripts...) }

	c := g.previewCookie(3000, "alice", "")
	wantRelay(t, "nothing listening", g.relayPreview(3000, "/", c, nil), http.StatusBadGateway, reasonPreviewNotListening)
	for _, s := range shells() {
		if strings.Contains(s, uiLauncherPrefix) || !strings.Contains(s, "p=3000") {
			t.Fatalf("a preview's exec: %q, want the probe of 3000 alone", s)
		}
	}
	if rows := append(g.h.audit.auditRows("ui.start", "success"), g.h.audit.auditRows("ui.start", "failure")...); len(rows) != 0 {
		t.Fatalf("ui.start rows = %+v, want none", rows)
	}

	mu.Lock()
	listening = true
	mu.Unlock()
	g.h.clock.advance(time.Minute) // past the session's re-check debounce
	wantRelay(t, "a listening port", g.relayPreview(3000, "/", c, nil), http.StatusOK, "")
	probes := len(shells())
	g.dropIdleConns()
	wantRelay(t, "a second dial", g.relayPreview(3000, "/", c, nil), http.StatusOK, "")
	if n := len(shells()); n != probes {
		t.Fatalf("a second dial within the probe's cache ran %d more shell execs, want none", n-probes)
	}
}

// TestPreviewProbeMatchesLauncherProbe: a preview's probe is the listening check the launcher script runs, for
// every machine kind.
func TestPreviewProbeMatchesLauncherProbe(t *testing.T) {
	for _, kind := range []types.MachineKind{"", types.MachineContainer, types.MachineLinuxVM, types.MachineMacOSVM} {
		if script := uiLauncherScriptFor(kind, uiLauncherPrefix+"code", 3000, 0); !strings.Contains(script, uiPreviewProbeCmd(kind)) {
			t.Errorf("%q: the launcher script %q does not run the preview's probe %q", kind, script, uiPreviewProbeCmd(kind))
		}
	}
}

// previewRaw opens a connection to srv and sends one GET for path on the run's host with cookie and extra
// headers, unread.
func (g *previewRig) previewRaw(t *testing.T, srv *httptest.Server, path string, c *http.Cookie, extra string) {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req := "GET " + path + " HTTP/1.1\r\nHost: " + g.runHost() + "\r\nCookie: " + c.Name + "=" + c.Value + "\r\n" + extra + "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
}

// TestPreviewRecheckEnds: an open preview stream, a 101 and a streaming answer, is cut within one re-check of
// each event that ends its authority, for the owner (on a private row) and an org caller (on an org row). A
// lowered ceiling and an org row lowered to private leave a private row the owner opens, so they are the org
// caller's alone. The row's expiry ends the stream at that time.
func TestPreviewRecheckEnds(t *testing.T) {
	const every = 300 * time.Millisecond
	setRelayWatch(t, every)
	bound := every + relayBound + relaySlack
	ctx := context.Background()
	causes := []struct {
		name    string
		orgOnly bool
		do      func(g *previewRig)
	}{
		{"revoke", false, func(g *previewRig) { _, _ = g.st.RevokeRunPreview(ctx, g.h.run.ID, 3000, "alice") }},
		{"lowered ceiling", true, func(g *previewRig) { g.st.setSite(previewSite(true, "private"), nil) }},
		{"switch off", false, func(g *previewRig) { g.st.setSite(previewSite(false, "org"), nil) }},
		{"org lowered to private", true, func(g *previewRig) {
			_, _, _, _ = g.st.UpsertRunPreview(ctx, g.h.run.ID, 3000, types.PreviewPrivate, time.Hour, "alice", nil)
		}},
		{"run stopped", false, func(g *previewRig) { g.set(func(run *types.AgentRun) { run.State = types.RunStopped }) }},
		{"data_out tightened", false, func(g *previewRig) {
			g.h.store.putEffectivePolicy(g.h.run.ID, types.RunPolicySpec{MinConfinementClass: types.CC2, DataOut: types.TransferBlocked,
				UIApps: []types.UIApp{{Name: "code", Port: uiTestPort, Path: "/ide"}}})
		}},
	}
	streams := []struct{ name, path, extra string }{
		{"101", "/ws", "Connection: Upgrade\r\nUpgrade: test\r\n"},
		{"streaming body", "/stream", ""},
	}
	callers := []struct {
		name, principal, caller string
		vis                     types.PreviewVisibility
	}{
		{"owner", "alice", "", types.PreviewPrivate},
		{"org caller", "bob", previewCallerOrg, types.PreviewOrg},
	}
	open := func(t *testing.T, vis types.PreviewVisibility, expires time.Duration, principal, caller, path, extra string) (*previewRig, chan time.Time) {
		t.Helper()
		g := newPreviewRigOn(t, streamBackend(&inputLog{}))
		p := g.st.putPreview(g.h.run.ID, 3000, vis)
		if expires > 0 {
			g.st.previewMu.Lock()
			for i := range g.st.rows {
				if g.st.rows[i].ID == p.ID {
					g.st.rows[i].ExpiresAt = time.Now().Add(expires)
				}
			}
			g.st.previewMu.Unlock()
		}
		h, ended := gatewayEnds(g.h.gateway)
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		g.previewRaw(t, srv, uiRelayPrefix(g.h.run.ID, previewApp(3000))+path, g.previewCookie(3000, principal, caller), extra)
		return g, ended
	}
	for _, who := range callers {
		for _, st := range streams {
			for _, cause := range causes {
				if cause.orgOnly && who.caller == "" {
					continue
				}
				name := who.name + ", " + st.name + ", " + cause.name
				t.Run(name, func(t *testing.T) {
					g, ended := open(t, who.vis, 0, who.principal, who.caller, st.path, st.extra)
					requireOpen(t, ended, 300*time.Millisecond+every) // one re-check passes while nothing has changed
					at := time.Now()
					cause.do(g)
					cutBy(t, name, at, waitEnded(t, ended, 10*time.Second, name), bound)
				})
			}
			name := who.name + ", " + st.name + ", expiry"
			t.Run(name, func(t *testing.T) {
				const expires = 1500 * time.Millisecond
				at := time.Now().Add(expires)
				_, ended := open(t, who.vis, expires, who.principal, who.caller, st.path, st.extra)
				requireOpen(t, ended, 300*time.Millisecond)
				cutBy(t, name, at, waitEnded(t, ended, 10*time.Second, name), relaySlack)
			})
		}
	}
}

// TestPreviewProfileEditEndsAnOpenPreview: the re-check asks the run's profile as it stands now, so an edit that
// denies interactive sessions or UI apps refuses an open private preview at its next request with
// preview_profile, as its ticket is refused, and a profile that denies neither leaves a new one open.
func TestPreviewProfileEditEndsAnOpenPreview(t *testing.T) {
	g := newPreviewRig(t)
	g.st.putPreview(g.h.run.ID, 3000, types.PreviewPrivate)
	set := walledRun(g.h)
	for _, c := range []struct {
		what  string
		limit types.GovernanceLimits
	}{
		{"denies interactive sessions", types.GovernanceLimits{DenyInteractive: true}},
		{"denies UI apps", types.GovernanceLimits{DenyUIApps: true}},
	} {
		set(types.GovernanceLimits{})
		cookie := g.openPreview(previewOwner, 3000)
		wantRelay(t, c.what+": before the edit", g.relayPreview(3000, "/", cookie, nil), http.StatusOK, "")
		set(c.limit)
		g.h.clock.advance(time.Minute) // past the session's re-check debounce
		wantRelay(t, c.what+": after the edit", g.relayPreview(3000, "/", cookie, nil), http.StatusForbidden, reasonPreviewProfile)
	}
}

// TestPreviewRefusalSentences: a preview's refusals speak to whoever opened the link. A revoked row, and a row
// that no longer admits the session's caller, say the preview ended; a request with no session says to open the
// link again, while a declared app keeps its own sentence; the ceiling sentence names visibilities by label.
func TestPreviewRefusalSentences(t *testing.T) {
	g := newPreviewRig(t)
	ctx := context.Background()
	sentence := func(what string, rec *httptest.ResponseRecorder, status int, reason, want string) {
		t.Helper()
		var body errorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != status || body.Reason != reason || body.Error != want {
			t.Fatalf("%s: %d %s, want %d %s %q", what, rec.Code, rec.Body.String(), status, reason, want)
		}
	}
	recheck := func() { g.h.clock.advance(time.Minute) } // past the session's re-check debounce

	g.st.putPreview(g.h.run.ID, 3000, types.PreviewPrivate)
	owner := g.previewCookie(3000, "alice", "")
	wantRelay(t, "the owner's private preview", g.relayPreview(3000, "/", owner, nil), http.StatusOK, "")
	_, _ = g.st.RevokeRunPreview(ctx, g.h.run.ID, 3000, "alice")
	recheck()
	sentence("revoked", g.relayPreview(3000, "/", owner, nil), http.StatusForbidden, uiDeniedReasonRevoked,
		"This preview has ended or was revoked. Ask the person who shared it for a new link.")

	g.st.putPreview(g.h.run.ID, 3001, types.PreviewOrg)
	org := g.previewCookie(3001, "bob", previewCallerOrg)
	wantRelay(t, "an org caller's org preview", g.relayPreview(3001, "/", org, nil), http.StatusOK, "")
	g.st.setSite(previewSite(true, "private"), nil)
	recheck()
	sentence("above the ceiling", g.relayPreview(3001, "/", org, nil), http.StatusForbidden, reasonPreviewAboveCeiling,
		"This deployment allows previews no wider than Private, so a preview set to Organisation can't be created or opened.")
	g.st.setSite(previewSite(true, "org"), nil)
	_, _, _, _ = g.st.UpsertRunPreview(ctx, g.h.run.ID, 3001, types.PreviewPrivate, time.Hour, "alice", nil)
	recheck()
	sentence("org lowered to private", g.relayPreview(3001, "/", org, nil), http.StatusForbidden, uiDeniedReasonNotAuthorized,
		"This preview has ended or was revoked. Ask the person who shared it for a new link.")

	sentence("no session", g.relayPreview(3001, "/", nil, nil), http.StatusForbidden, reasonUIGatewayNoSession,
		"This preview needs you to sign in again. Open it from the link you were given.")
	req := httptest.NewRequest(http.MethodGet, uiRelayPrefix(g.h.run.ID, "code")+"/ide", nil)
	req.Host = g.runHost()
	rec := httptest.NewRecorder()
	g.h.gateway.ServeHTTP(rec, req)
	sentence("a declared app with no session", rec, http.StatusForbidden, reasonUIGatewayNoSession, uiNoSessionMsg)
}
