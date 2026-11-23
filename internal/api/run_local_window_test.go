// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Tests here never call t.Parallel(): the rig swaps the site feature hooks (withSiteFeatureHooks) so the key reads
// available while it is not yet served.

// The stamp modes of localWindowStore.
const (
	stampOK         = iota // the stamp lands (and is debounced after)
	stampStoreFails        // the store write fails: stampPresence logs it and reports nothing, and the next stamp is due
	stampThawFails         // the run reads paused and its thaw cannot read the run: stampPresence returns the error
)

// localWindowStore is macWindowStore with a stamp a test can make fail.
type localWindowStore struct {
	*macWindowStore
	stampMode atomic.Int32
}

func (s *localWindowStore) StampRunActive(context.Context, uuid.UUID) (bool, error) {
	s.stamps.Add(1)
	switch s.stampMode.Load() {
	case stampStoreFails:
		return false, errors.New("conn closed by peer")
	case stampThawFails:
		s.failRunRead(errors.New("conn closed by peer"))
		return true, nil
	}
	return false, nil
}

// fakeLocalWindowSession is a runner's open local window: end it with the runner's end line.
type fakeLocalWindowSession struct {
	ended   chan struct{}
	once    sync.Once
	mu      sync.Mutex
	reason  string
	refusal string
	dropped int
	closed  atomic.Bool
}

func (f *fakeLocalWindowSession) Ended() <-chan struct{} { return f.ended }
func (f *fakeLocalWindowSession) EndReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reason
}
func (f *fakeLocalWindowSession) RefusalKind() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refusal
}
func (f *fakeLocalWindowSession) CutTextDropped() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dropped
}
func (f *fakeLocalWindowSession) Close() error { f.closed.Store(true); return nil }

func (f *fakeLocalWindowSession) end(reason, refusal string, dropped int) {
	f.once.Do(func() {
		f.mu.Lock()
		f.reason, f.refusal, f.dropped = reason, refusal, dropped
		f.mu.Unlock()
		close(f.ended)
	})
}

// fakeLocalWindowSub is the runner's end of the grant.
type fakeLocalWindowSub struct {
	mu        sync.Mutex
	supported bool
	reported  bool
	err       error
	opens     []localWindowRequest
	sessions  []*fakeLocalWindowSession
	onOpen    func()
}

func (f *fakeLocalWindowSub) LocalWindowSupported() (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.supported, f.reported
}

func (f *fakeLocalWindowSub) LocalWindow(_ context.Context, _ string, req localWindowRequest) (localWindowSession, error) {
	f.mu.Lock()
	hook, err := f.onOpen, f.err
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return nil, err
	}
	sess := &fakeLocalWindowSession{ended: make(chan struct{})}
	f.mu.Lock()
	f.opens = append(f.opens, req)
	f.sessions = append(f.sessions, sess)
	f.mu.Unlock()
	return sess, nil
}

func (f *fakeLocalWindowSub) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.opens)
}

func (f *fakeLocalWindowSub) session(i int) *fakeLocalWindowSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[i]
}

// localWindowRig is an interactive Linux VM run on its person's own computer, with features.local_window on and
// available, a runner that serves local windows and a re-check clock the test moves by sending on ticks.
type localWindowRig struct {
	t       *testing.T
	h       *uiHarness
	site    *desktopSiteStore
	st      *localWindowStore
	sub     *fakeLocalWindowSub
	offline atomic.Bool
	ticks   chan time.Time
	rev     *macWindowRevocations
}

// localWindowOnConfig is a site config with features.local_window on.
func localWindowOnConfig() types.SiteConfig {
	on := true
	return types.SiteConfig{FeatureSwitches: &types.SiteFeatureSwitches{LocalWindow: &types.LocalWindowFeature{Enabled: &on}}}
}

func newLocalWindowRig(t *testing.T) *localWindowRig {
	t.Helper()
	withSiteFeatureHooks(t, availableSiteFeatures(featureLocalWindow))
	h, _ := newDesktopHarness(t)
	site := &desktopSiteStore{desktopClosedStore: h.srv.cfg.Store.(*desktopClosedStore), sc: localWindowOnConfig()}
	st := &localWindowStore{macWindowStore: &macWindowStore{desktopSiteStore: site, runners: map[uuid.UUID]types.Runner{}, prefs: map[string]json.RawMessage{}}}
	h.srv.cfg.Store = st
	h.run.Agent, h.run.Experience = "claude-code", types.ExperienceInteractive
	id := uuid.New()
	h.run.RunnerID = &id
	rig := &localWindowRig{t: t, h: h, site: site, st: st, sub: &fakeLocalWindowSub{supported: true, reported: true},
		ticks: make(chan time.Time), rev: &macWindowRevocations{}}
	rig.adoptRunner()
	h.srv.cfg.SessionRevocations = rig.rev
	h.srv.localWindowSubs = func(uuid.UUID) localWindowSub {
		if rig.offline.Load() {
			return nil
		}
		return rig.sub
	}
	h.srv.localWindowTick = func(time.Duration) (<-chan time.Time, func()) { return rig.ticks, func() {} }
	t.Cleanup(func() {
		if w := h.srv.localWindows.open(h.run.ID); w != nil && w.sess != nil {
			h.srv.closeLocalWindow(context.Background(), w, localWindowEnd{reason: macWindowClosedByPerson})
		}
	})
	return rig
}

// adoptRunner makes the run's runner a claimed one registered to the run's person, and the run local.
func (r *localWindowRig) adoptRunner() {
	r.t.Helper()
	r.st.setRunner(types.Runner{ID: *r.h.run.RunnerID, Name: "Alice's laptop", Owner: r.h.owner, State: types.RunnerClaimed, OwnerKind: types.RunnerOwnerPerson})
	r.h.run.Placement = types.PlacementLocal
	r.h.store.putRun(r.h.run)
}

func (r *localWindowRig) set(f func(run *types.AgentRun)) {
	r.t.Helper()
	f(&r.h.run)
	r.h.store.putRun(r.h.run)
}

func (r *localWindowRig) setRunner(f func(rn *types.Runner)) {
	r.t.Helper()
	rn, err := r.st.GetRunner(context.Background(), *r.h.run.RunnerID)
	if err != nil {
		r.t.Fatal(err)
	}
	f(&rn)
	r.st.setRunner(rn)
}

// auth is the run's person as a signed-in console session.
func (r *localWindowRig) auth() macWindowAuthority {
	return macWindowAuthority{ActorType: types.ActorHuman, Principal: r.h.owner, Email: r.h.owner + "@example.com", Issue: oidc.Issue{At: time.Now()}}
}

func (r *localWindowRig) rows(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range r.h.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func (r *localWindowRig) open(a macWindowAuthority) (*localWindow, *runRefusal) {
	r.t.Helper()
	return r.h.srv.openLocalWindow(context.Background(), r.h.run, a, "nonce-1")
}

func (r *localWindowRig) mustOpen() *localWindow {
	r.t.Helper()
	w, rf := r.open(r.auth())
	if rf != nil {
		r.t.Fatalf("open refused: %+v", rf)
	}
	return w
}

func (r *localWindowRig) tick() {
	r.t.Helper()
	select {
	case r.ticks <- time.Now():
	case <-time.After(3 * time.Second):
		r.t.Fatal("the watcher took no tick")
	}
}

// stays ticks once and waits for that pass to touch the run, the last thing a pass that keeps the window does.
func (r *localWindowRig) stays() {
	r.t.Helper()
	want := r.st.touched.Load() + 1
	r.tick()
	macWindowEventually(r.t, "the re-check that keeps the window", func() bool { return r.st.touched.Load() >= want })
}

func (r *localWindowRig) closeRow() map[string]any {
	r.t.Helper()
	macWindowEventually(r.t, "takeover.close", func() bool { return len(r.rows("takeover.close")) > 0 })
	return rowData(r.t, r.rows("takeover.close")[0])
}

// post is POST /runs/{id}/desktop/local-window as sub.
func (r *localWindowRig) post(sub, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	router := chi.NewRouter()
	r.h.srv.mountLocalWindowRoutes(router)
	req := httptest.NewRequest(http.MethodPost, "/runs/"+r.h.run.ID.String()+"/desktop/local-window", strings.NewReader(body))
	req = req.WithContext(withHumanIdentity(req.Context(), sub, sub+"@example.com", oidc.RoleUser, types.UserTypeStandard, nil, false))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// wantRefused fails unless rf is reason with status and nothing reached the runner, and one denied
// takeover.begin names the reason.
func (r *localWindowRig) wantRefused(w *localWindow, rf *runRefusal, status int, reason string) {
	r.t.Helper()
	if w != nil || rf == nil || rf.err != nil || rf.decision != nil || rf.status != status || rf.body.Reason != reason {
		r.t.Fatalf("open = %v %+v, want %d %s", w, rf, status, reason)
	}
	if n := r.sub.openCount(); n != 0 {
		r.t.Errorf("%d runner calls on a refused open", n)
	}
	b := r.rows("takeover.begin")
	if len(b) != 1 || b[0].Outcome != "denied" || rowData(r.t, b[0])["reason"] != reason || rowData(r.t, b[0])["surface"] != localWindowSurface {
		r.t.Errorf("begin rows = %+v, want one denied row naming %s", b, reason)
	}
}

// V-5: only the run creator's own claimed person runner opens a window, with no delegated token; another person,
// a service identity, an agent, a delegate, an operator's run, an organisation's runner and a remote run are all
// refused with the not-your-computer sentence, before any runner call.
func TestLocalWindowOwnRunner(t *testing.T) {
	r := newLocalWindowRig(t)
	if w := r.mustOpen(); w.runnerID != *r.h.run.RunnerID {
		t.Fatalf("window on runner %s, want the run's", w.runnerID)
	}
	for name, tc := range map[string]struct {
		auth   func(a *macWindowAuthority)
		run    func(run *types.AgentRun)
		runner func(rn *types.Runner)
	}{
		"another person":       {auth: func(a *macWindowAuthority) { a.Principal = "bob" }},
		"a service identity":   {auth: func(a *macWindowAuthority) { a.ActorType = types.ActorService }},
		"an agent":             {auth: func(a *macWindowAuthority) { a.ActorType = types.ActorAgent }},
		"a delegated token":    {auth: func(a *macWindowAuthority) { a.Via = true }},
		"an operator's run":    {run: func(run *types.AgentRun) { run.OperatorOwned = true }},
		"a remote run":         {run: func(run *types.AgentRun) { run.Placement = types.PlacementRemote }},
		"an org's runner":      {runner: func(rn *types.Runner) { rn.OwnerKind = types.RunnerOwnerOrg }},
		"another's runner":     {runner: func(rn *types.Runner) { rn.Owner = "bob" }},
		"an unclaimed runner":  {runner: func(rn *types.Runner) { rn.State = types.RunnerUnclaimed }},
		"an unregistered run":  {run: func(run *types.AgentRun) { run.RunnerID = nil }},
		"a runner not on file": {runner: func(rn *types.Runner) { rn.ID = uuid.New() }},
	} {
		t.Run(name, func(t *testing.T) {
			r := newLocalWindowRig(t)
			a := r.auth()
			if tc.auth != nil {
				tc.auth(&a)
			}
			if tc.run != nil {
				r.set(tc.run)
			}
			if tc.runner != nil {
				rn, _ := r.st.GetRunner(context.Background(), *r.h.run.RunnerID)
				tc.runner(&rn)
				if rn.ID != *r.h.run.RunnerID {
					r.st.mu.Lock()
					delete(r.st.runners, *r.h.run.RunnerID)
					r.st.mu.Unlock()
				}
				r.st.setRunner(rn)
			}
			w, rf := r.open(a)
			r.wantRefused(w, rf, http.StatusForbidden, reasonLocalWindowNotYourComputer)
			if rf.body.Error != localWindowNotYourComputerMsg {
				t.Errorf("sentence = %q", rf.body.Error)
			}
		})
	}
}

// V-6: AUTH_OK's local_window follows the switch: false for an absent key, {}, {"enabled": false}, an unavailable
// hook, an unreadable site config and a local-mode server; true only when on.
func TestRunnerLocalWindowFollowsTheSwitch(t *testing.T) {
	r := newLocalWindowRig(t)
	person := types.Runner{OwnerKind: types.RunnerOwnerPerson, State: types.RunnerClaimed}
	off := false
	for name, sc := range map[string]types.SiteConfig{
		"absent key":   {},
		"no block":     {FeatureSwitches: &types.SiteFeatureSwitches{}},
		"{}":           {FeatureSwitches: &types.SiteFeatureSwitches{LocalWindow: &types.LocalWindowFeature{}}},
		"enabled off":  {FeatureSwitches: &types.SiteFeatureSwitches{LocalWindow: &types.LocalWindowFeature{Enabled: &off}}},
		"unreadable":   localWindowOnConfig(),
		"enabled true": localWindowOnConfig(),
	} {
		var err error
		if name == "unreadable" {
			err = errors.New("conn closed by peer")
		}
		r.site.setSite(sc, err)
		if got, want := r.h.srv.runnerLocalWindow(context.Background(), person), name == "enabled true"; got != want {
			t.Errorf("%s: local_window = %v, want %v", name, got, want)
		}
	}
	r.site.setSite(localWindowOnConfig(), nil)
	r.h.srv.cfg.LocalMode = true
	if r.h.srv.runnerLocalWindow(context.Background(), person) {
		t.Error("a local-mode server told a runner local_window")
	}
	r.h.srv.cfg.LocalMode = false
	withSiteFeatureHooks(t, map[siteFeatureKey]siteFeatureHook{featureLocalWindow: {available: func() bool { return false }}})
	if r.h.srv.runnerLocalWindow(context.Background(), person) {
		t.Error("an unavailable key told a runner local_window")
	}
}

// V-12: an organisation's runner is never told local_window, whatever the switch says.
func TestRunnerLocalWindowPersonOnly(t *testing.T) {
	r := newLocalWindowRig(t)
	for _, c := range []struct {
		kind types.RunnerOwnerKind
		want bool
	}{{types.RunnerOwnerPerson, true}, {types.RunnerOwnerOrg, false}} {
		if got := r.h.srv.runnerLocalWindow(context.Background(), types.Runner{OwnerKind: c.kind}); got != c.want {
			t.Errorf("%s runner: local_window = %v, want %v", c.kind, got, c.want)
		}
	}
}

// V-7: with the switch off the owner's open is refused 403 local_window_site_off before any runner call, an
// unreadable site config is a server error, and turning the switch off ends an open window at the next re-check.
func TestLocalWindowSwitch(t *testing.T) {
	t.Run("off refuses the owner", func(t *testing.T) {
		r := newLocalWindowRig(t)
		r.site.setSite(types.SiteConfig{}, nil)
		rec := r.post(r.h.owner, `{"local_nonce":"nonce-1"}`)
		var body errorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusForbidden || body.Reason != reasonLocalWindowSiteOff || body.Error != localWindowSiteOffMsg {
			t.Fatalf("POST = %d %s, want 403 %s", rec.Code, rec.Body.String(), reasonLocalWindowSiteOff)
		}
		if r.sub.openCount() != 0 {
			t.Fatal("the runner was asked with the switch off")
		}
	})
	t.Run("unreadable site config", func(t *testing.T) {
		r := newLocalWindowRig(t)
		r.site.setSite(localWindowOnConfig(), errors.New("conn closed by peer"))
		if _, rf := r.open(r.auth()); rf == nil || rf.err == nil {
			t.Fatalf("open = %+v, want a server error", rf)
		}
	})
	t.Run("on grants through the route", func(t *testing.T) {
		r := newLocalWindowRig(t)
		rec := r.post(r.h.owner, `{"local_nonce":"nonce-1"}`)
		// The answer is exactly {grant_id, runner_id, runner}, and its ids are the takeover rows' ids.
		var g map[string]any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &g) != nil || len(g) != 3 {
			t.Fatalf("POST = %d %s, want a grant of three fields", rec.Code, rec.Body.String())
		}
		b := r.rows("takeover.begin")
		if len(b) != 1 {
			t.Fatalf("begin rows = %+v", b)
		}
		bd := rowData(t, b[0])
		if g["grant_id"] != bd["grant_id"] || g["runner_id"] != bd["runner_id"] || g["runner_id"] != r.h.run.RunnerID.String() || g["runner"] != "Alice's laptop" {
			t.Fatalf("grant = %v, begin row = %v", g, bd)
		}
		if r.sub.openCount() != 1 || r.sub.opens[0].Nonce != "nonce-1" || r.sub.opens[0].GrantID.String() != g["grant_id"] {
			t.Fatalf("runner calls = %+v", r.sub.opens)
		}
		if strings.Contains(rec.Body.String(), "nonce-1") {
			t.Error("the answer quotes the nonce")
		}
	})
	t.Run("no nonce is a bad request after the rows", func(t *testing.T) {
		r := newLocalWindowRig(t)
		if rec := r.post(r.h.owner, `{}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("POST {} = %d %s, want 400", rec.Code, rec.Body.String())
		}
		r.site.setSite(types.SiteConfig{}, nil)
		if rec := r.post(r.h.owner, ``); rec.Code != http.StatusForbidden {
			t.Fatalf("POST with no body and the switch off = %d %s, want the switch's 403", rec.Code, rec.Body.String())
		}
	})
	t.Run("turning it off ends an open window", func(t *testing.T) {
		r := newLocalWindowRig(t)
		r.mustOpen()
		r.site.setSite(types.SiteConfig{}, nil)
		r.tick()
		if c := r.closeRow(); c["reason"] != macWindowRefusedRecheck || c["refused_by"] != reasonLocalWindowSiteOff {
			t.Fatalf("close = %v", c)
		}
		if !r.sub.session(0).closed.Load() {
			t.Error("the runner's session stayed open")
		}
	})
}

// V-7 on the matrix server: entry authorization answers before the switch, so with it off the owner gets 403
// local_window_site_off, a super admin run_owner_only and a non-owning member the run-not-found 404, and with
// it on the owner reaches the handler's own answer.
func TestAuthzMatrixLocalWindowSiteSwitch(t *testing.T) {
	srv, ast, _, _ := newAuthzMatrixServer(t)
	const owner = "sub-member"
	memberSess := ssoSession(t, owner, "member@corp.example", oidc.RoleUser)
	adminSess := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	otherSess := ssoSession(t, "sub-other-member", "other@corp.example", oidc.RoleUser)
	id := uuid.New()
	ast.mu.Lock()
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: owner, State: types.RunRunning, Agent: "claude-code"}
	ast.siteCfg = types.SiteConfig{Runners: ast.siteCfg.Runners, AgentProviders: ast.siteCfg.AgentProviders}
	ast.mu.Unlock()
	path, body := buildPath("/api/v1/runs/{id}/desktop/local-window", id.String()), `{"local_nonce":"n"}`
	if w := doSSO(t, srv, http.MethodPost, path, memberSess, body); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), `"reason":"`+reasonLocalWindowSiteOff+`"`) {
		t.Errorf("switch off: owning member: status = %d, want 403 %s; body=%s", w.Code, reasonLocalWindowSiteOff, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodPost, path, adminSess, body); w.Code != http.StatusForbidden ||
		!strings.Contains(w.Body.String(), `"reason":"run_owner_only"`) {
		t.Errorf("switch off: super admin: status = %d, want 403 run_owner_only; body=%s", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodPost, path, otherSess, body); w.Code != http.StatusNotFound ||
		strings.Contains(w.Body.String(), reasonLocalWindowSiteOff) {
		t.Errorf("switch off: non-owning member: status = %d, want the run-not-found 404; body=%s", w.Code, w.Body.String())
	}
	ast.mu.Lock()
	ast.siteCfg = authzMatrixSiteConfig()
	ast.mu.Unlock()
	if w := doSSO(t, srv, http.MethodPost, path, memberSess, body); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden || w.Code == http.StatusNotFound {
		t.Errorf("switch on: owning member: status = %d, want none of 401/403/404; body=%s", w.Code, w.Body.String())
	}
}

// V-8: Screen and actions recording refuses, since a local window isn't recorded.
func TestLocalWindowRecordingRefused(t *testing.T) {
	r := newLocalWindowRig(t)
	r.set(func(run *types.AgentRun) { run.RecordingLevel = types.RecordingScreenActions })
	w, rf := r.open(r.auth())
	r.wantRefused(w, rf, http.StatusForbidden, reasonLocalWindowRecording)
	if rf.body.Error != localWindowRecordingMsg {
		t.Errorf("sentence = %q", rf.body.Error)
	}
}

// V-13: a background-only run is refused with the door's own reason, as the door contract keeps its desktop
// closed.
func TestLocalWindowBackgroundOnlyRefused(t *testing.T) {
	r := newLocalWindowRig(t)
	r.set(func(run *types.AgentRun) { run.Experience = types.ExperienceBackground })
	w, rf := r.open(r.auth())
	r.wantRefused(w, rf, http.StatusConflict, reasonRunBackgroundOnly)
}

// The rows' other refusals, in order, each before any runner call: the kind, a run that is not running or is
// kept, the runner offline or too old, paused, preparing.
func TestLocalWindowRefusalRows(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		change func(r *localWindowRig)
		status int
		reason string
	}{
		"a macOS VM run": {func(r *localWindowRig) { r.set(func(run *types.AgentRun) { run.MachineKind = types.MachineMacOSVM }) },
			http.StatusConflict, reasonLocalWindowKind},
		"a container run": {func(r *localWindowRig) { r.set(func(run *types.AgentRun) { run.MachineKind = "" }) },
			http.StatusConflict, reasonLocalWindowKind},
		"an ended run": {func(r *localWindowRig) { r.set(func(run *types.AgentRun) { run.State = types.RunKilled }) },
			http.StatusConflict, reasonAttachNotRunning},
		"a kept run": {func(r *localWindowRig) { r.set(func(run *types.AgentRun) { run.LostAt = &now }) },
			http.StatusConflict, reasonAttachRunKept},
		"the runner offline":       {func(r *localWindowRig) { r.offline.Store(true) }, http.StatusConflict, reasonLocalWindowOffline},
		"caps not yet reported":    {func(r *localWindowRig) { r.sub.reported = false }, http.StatusConflict, reasonLocalWindowOffline},
		"a runner without the cap": {func(r *localWindowRig) { r.sub.supported = false }, http.StatusConflict, reasonLocalWindowRunnerTooOld},
		"paused":                   {func(r *localWindowRig) { r.set(func(run *types.AgentRun) { run.PausedAt = &now }) }, http.StatusConflict, reasonLocalWindowPaused},
		"preparing": {func(r *localWindowRig) { r.set(func(run *types.AgentRun) { run.State = types.RunStarting }) },
			http.StatusConflict, reasonLocalWindowPreparing},
	} {
		t.Run(name, func(t *testing.T) {
			r := newLocalWindowRig(t)
			tc.change(r)
			w, rf := r.open(r.auth())
			r.wantRefused(w, rf, tc.status, tc.reason)
		})
	}
}

// The real runner end: with no test seam, row 8 reads the runner's live caps through its remote substrate.
func TestLocalWindowRunnerCaps(t *testing.T) {
	for name, tc := range map[string]struct {
		online, cap bool
		reason      string
	}{
		"offline":       {online: false, cap: true, reason: reasonLocalWindowOffline},
		"no window cap": {online: true, cap: false, reason: reasonLocalWindowRunnerTooOld},
	} {
		t.Run(name, func(t *testing.T) {
			r := newLocalWindowRig(t)
			r.h.srv.localWindowSubs = nil
			caps := laptopCaps(types.CC1)
			caps.LocalWindow = tc.cap
			data, err := json.Marshal(caps)
			if err != nil {
				t.Fatal(err)
			}
			bgWatchPutRunner(r.h, tc.online, nil)
			r.h.srv.runnerEndpoint(*r.h.run.RunnerID).sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: data})
			r.adoptRunner()
			_, rf := r.h.srv.localWindowCheck(context.Background(), r.h.run, r.auth(), localWindowAsk{})
			if rf == nil || rf.body.Reason != tc.reason {
				t.Fatalf("refusal = %+v, want %s", rf, tc.reason)
			}
		})
	}
}

// V-9: a revoked credential, a governance change, a stopped run, a stamp that fails and anything unread each end
// the window with their reason at the next re-check, closing the runner's session once.
func TestLocalWindowRecheckEnds(t *testing.T) {
	cases := []struct {
		name   string
		change func(r *localWindowRig)
		reason string
		by     string
	}{
		{"the credential is revoked", func(r *localWindowRig) { r.rev.set(true, nil) }, macWindowRevoked, ""},
		{"the profile comes to deny interactive desktops", func(r *localWindowRig) {
			walledRun(r.h)(types.GovernanceLimits{DenyInteractive: true})
		}, macWindowRefusedRecheck, string(authz.ReasonGovernanceUnit)},
		{"the run stopped", func(r *localWindowRig) { r.set(func(run *types.AgentRun) { run.State = types.RunKilled }) }, macWindowRunEnded, ""},
		{"the presence stamp fails", func(r *localWindowRig) { r.st.stampMode.Store(stampThawFails) },
			macWindowRefusedRecheck, reasonLocalWindowPaused},
		{"the runner's owner changed", func(r *localWindowRig) { r.setRunner(func(rn *types.Runner) { rn.Owner = "bob" }) },
			macWindowRefusedRecheck, reasonLocalWindowNotYourComputer},
		{"the recording level becomes Screen and actions", func(r *localWindowRig) {
			r.set(func(run *types.AgentRun) { run.RecordingLevel = types.RecordingScreenActions })
		}, macWindowRefusedRecheck, reasonLocalWindowRecording},
		{"the runner goes offline", func(r *localWindowRig) { r.offline.Store(true) }, macWindowRunnerOffline, reasonLocalWindowOffline},
		{"the credential can't be read", func(r *localWindowRig) { r.rev.set(false, errors.New("conn closed by peer")) },
			macWindowRefusedRecheck, uiDeniedReasonRevocationUnavailable},
		{"the run can't be read", func(r *localWindowRig) { r.st.failRunRead(errors.New("conn closed by peer")) },
			macWindowRefusedRecheck, reasonRunUnreadable},
		{"the site config can't be read", func(r *localWindowRig) { r.site.setSite(localWindowOnConfig(), errors.New("conn closed by peer")) },
			macWindowRefusedRecheck, reasonRunUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newLocalWindowRig(t)
			r.st.stampMode.Store(stampStoreFails) // every pass stamps, so a stamp failure set later is reached
			w := r.mustOpen()
			r.stays() // nothing changed: the window stays
			if len(r.rows("takeover.close")) != 0 || r.h.srv.localWindows.open(r.h.run.ID) != w {
				t.Fatal("the window ended with nothing changed")
			}
			tc.change(r)
			r.tick()
			c := r.closeRow()
			if c["reason"] != tc.reason || c["refused_by"] != orNil(tc.by) || c["surface"] != localWindowSurface {
				t.Errorf("close = %v, want reason %q refused_by %q", c, tc.reason, tc.by)
			}
			if !r.sub.session(0).closed.Load() {
				t.Error("the runner's session was not closed")
			}
			macWindowEventually(t, "the registry to empty", func() bool { return r.h.srv.localWindows.open(r.h.run.ID) == nil })
			if got := len(r.rows("takeover.close")); got != 1 {
				t.Errorf("close rows = %d, want one", got)
			}
		})
	}
}

// V-10: one window per run. The slot is reserved before the yield and the yield runs before the runner call;
// a browser session that does not close refuses the open and releases the slot; a second open, or a Mac window
// on the run, is refused before any runner call.
func TestLocalWindowOneKeyboard(t *testing.T) {
	t.Run("reserve, then yield, then the runner", func(t *testing.T) {
		r := newLocalWindowRig(t)
		ctx, cancel := context.WithCancel(context.Background())
		var reservedAtYield atomic.Bool
		untrack := r.h.srv.desktopControl.track(r.h.run.ID, func() {
			reservedAtYield.Store(r.h.srv.localWindows.open(r.h.run.ID) != nil)
			cancel()
		})
		go func() { <-ctx.Done(); untrack() }()
		var reservedAtCall, yieldedAtCall atomic.Bool
		r.sub.onOpen = func() {
			reservedAtCall.Store(r.h.srv.localWindows.open(r.h.run.ID) != nil)
			yieldedAtCall.Store(ctx.Err() != nil)
		}
		r.mustOpen()
		if !reservedAtYield.Load() || !reservedAtCall.Load() || !yieldedAtCall.Load() {
			t.Fatalf("reserved at the yield %v, reserved at the call %v, yielded at the call %v; want all true",
				reservedAtYield.Load(), reservedAtCall.Load(), yieldedAtCall.Load())
		}
	})
	t.Run("a session that does not close refuses and releases the slot", func(t *testing.T) {
		old := macWindowYieldWait
		macWindowYieldWait = 50 * time.Millisecond
		t.Cleanup(func() { macWindowYieldWait = old })
		r := newLocalWindowRig(t)
		r.h.srv.desktopControl.track(r.h.run.ID, func() {}) // never forgets itself
		w, rf := r.open(r.auth())
		r.wantRefused(w, rf, http.StatusConflict, reasonLocalWindowDesktopControl)
		if r.h.srv.localWindows.open(r.h.run.ID) != nil {
			t.Error("the slot was kept")
		}
	})
	t.Run("a second open is refused", func(t *testing.T) {
		r := newLocalWindowRig(t)
		r.mustOpen()
		w, rf := r.open(r.auth())
		if w != nil || rf == nil || rf.body.Reason != reasonLocalWindowOpen || r.sub.openCount() != 1 {
			t.Fatalf("second open = %v %+v, %d runner calls; want 409 %s and one call", w, rf, r.sub.openCount(), reasonLocalWindowOpen)
		}
	})
	t.Run("a Mac window on the run is refused", func(t *testing.T) {
		r := newLocalWindowRig(t)
		r.h.srv.macWindows.reserve(&macWindow{runID: r.h.run.ID})
		w, rf := r.open(r.auth())
		r.wantRefused(w, rf, http.StatusConflict, reasonLocalWindowOpen)
	})
	t.Run("the runner refuses the nonce", func(t *testing.T) {
		r := newLocalWindowRig(t)
		r.sub.err = errLocalWindowNonce
		w, rf := r.open(r.auth())
		if w != nil || rf == nil || rf.status != http.StatusConflict || rf.body.Reason != reasonLocalWindowNonce || r.h.srv.localWindows.open(r.h.run.ID) != nil {
			t.Fatalf("open = %v %+v, want 409 %s and no slot", w, rf, reasonLocalWindowNonce)
		}
		r.sub.err = errors.New("display gone")
		if _, rf := r.open(r.auth()); rf == nil || rf.status != http.StatusBadGateway || rf.body.Reason != reasonLocalWindowFailed {
			t.Fatalf("open = %+v, want 502 %s", rf, reasonLocalWindowFailed)
		}
	})
}

// V-11: an open writes takeover.begin success and every end takeover.close, both with surface local_window, the
// grant and the runner; the close carries refusal_kind and cut_text_dropped when the runner reports them; a
// denial writes the denied row.
func TestLocalWindowAudit(t *testing.T) {
	for in, want := range map[[2]string]string{
		{runnerwire.LocalWindowEndPerson, ""}:                            macWindowClosedByPerson,
		{runnerwire.LocalWindowEndRunEnded, ""}:                          macWindowRunEnded,
		{runnerwire.LocalWindowEndRefused, runnerwire.LocalWindowNotRFB}: localWindowDisplayRefused,
		{runnerwire.LocalWindowEndHelperFailed, ""}:                      macWindowHelperFailed,
		{"anything", ""}: macWindowLinkLost,
	} {
		r := newLocalWindowRig(t)
		w := r.mustOpen()
		b := r.rows("takeover.begin")
		if len(b) != 1 || b[0].Outcome != "success" || b[0].Target != w.grantID.String() {
			t.Fatalf("begin rows = %+v", b)
		}
		bd := rowData(t, b[0])
		if bd["surface"] != localWindowSurface || bd["grant_id"] != w.grantID.String() || bd["runner_id"] != w.runnerID.String() {
			t.Fatalf("begin data = %v", bd)
		}
		r.sub.session(0).end(in[0], in[1], 2)
		c := r.closeRow()
		if c["reason"] != want || c["surface"] != localWindowSurface || c["grant_id"] != w.grantID.String() ||
			c["runner_id"] != w.runnerID.String() || c["cut_text_dropped"] != float64(2) || c["refusal_kind"] != orNil(in[1]) {
			t.Errorf("runner end %v: close = %v, want reason %q", in, c, want)
		}
	}
	r := newLocalWindowRig(t)
	r.site.setSite(types.SiteConfig{}, nil)
	w, rf := r.open(r.auth())
	r.wantRefused(w, rf, http.StatusForbidden, reasonLocalWindowSiteOff)
}

// V-14: each re-check that keeps the window stamps the run's presence and touches it, as the open does once.
func TestLocalWindowIsPresence(t *testing.T) {
	r := newLocalWindowRig(t)
	r.st.stampMode.Store(stampStoreFails) // no stamp lands, so none is debounced: every pass reaches the store
	r.mustOpen()
	if s, tch := r.st.stamps.Load(), r.st.touched.Load(); s != 1 || tch != 1 {
		t.Fatalf("after the open: stamps %d touches %d, want 1 and 1", s, tch)
	}
	const n = 3
	for range n {
		r.stays()
	}
	if s, tch := r.st.stamps.Load(), r.st.touched.Load(); s != n+1 || tch != n+1 {
		t.Errorf("after %d re-checks: stamps %d touches %d, want %d each", n, s, tch, n+1)
	}
}

// V-14: a re-check that finds the run paused ends the window with local_window_paused and stamps and touches
// nothing; an open on a paused run is refused with no stamp.
func TestLocalWindowClosesOnPause(t *testing.T) {
	r := newLocalWindowRig(t)
	r.st.stampMode.Store(stampStoreFails)
	r.mustOpen()
	r.stays()
	stamps, touched := r.st.stamps.Load(), r.st.touched.Load()
	now := time.Now()
	r.set(func(run *types.AgentRun) { run.PausedAt = &now })
	r.tick()
	if c := r.closeRow(); c["reason"] != macWindowRefusedRecheck || c["refused_by"] != reasonLocalWindowPaused {
		t.Fatalf("close = %v, want refused_by %s", c, reasonLocalWindowPaused)
	}
	if r.st.stamps.Load() != stamps || r.st.touched.Load() != touched {
		t.Errorf("stamps %d→%d touches %d→%d: a paused run was stamped", stamps, r.st.stamps.Load(), touched, r.st.touched.Load())
	}

	p := newLocalWindowRig(t)
	p.set(func(run *types.AgentRun) { run.PausedAt = &now })
	w, rf := p.open(p.auth())
	p.wantRefused(w, rf, http.StatusConflict, reasonLocalWindowPaused)
	if p.st.stamps.Load() != 0 || p.st.touched.Load() != 0 {
		t.Error("an open refused for a pause stamped the run")
	}
}

// V-16: until the window is served end to end the key is unavailable: a write that turns it on is refused
// feature_unavailable, and a stored switch reads off. Real registrations, no matrix server.
func TestLocalWindowUnavailableUntilServed(t *testing.T) {
	if siteFeatureAvailable(featureLocalWindow) {
		t.Fatal("local_window is available on this tree: only the commit that serves the window end to end may change this assertion")
	}
	fake := &fakeSiteConfigStore{}
	wantSiteFeatureRefusal(t, putSiteFeatures(t, fake, `{"features":{"local_window":{"enabled":true}}}`), fake, reasonSiteFeatureUnavailable)
	if localWindowOn(localWindowOnConfig()) {
		t.Fatal("a stored local_window switch reads on while the key is unavailable")
	}
}
