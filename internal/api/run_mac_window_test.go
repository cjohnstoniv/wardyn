// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
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
	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// macWindowRevocations is a revocation store a test flips: revoked answers every check, err fails it.
type macWindowRevocations struct {
	mu      sync.Mutex
	revoked bool
	err     error
}

func (r *macWindowRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revoked, r.err
}
func (r *macWindowRevocations) RevokeSub(context.Context, string) error { return nil }
func (r *macWindowRevocations) RevokeAll(context.Context) error         { return nil }

func (r *macWindowRevocations) set(revoked bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.revoked, r.err = revoked, err
}

// macWindowStore is the harness's store with what a window reads on top: runners, per-person records and a
// presence clock, and a run read a test can fail.
type macWindowStore struct {
	*desktopSiteStore
	mu          sync.Mutex
	runners     map[uuid.UUID]types.Runner
	runnerErr   error
	prefs       map[string]json.RawMessage
	getRunErr   error
	tokens      map[uuid.UUID]bool
	cliSessions map[uuid.UUID]store.CLISessionRow
	stamps      atomic.Int32
	touched     atomic.Int32
	prefsWrite  atomic.Int32
}

func (s *macWindowStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runnerErr != nil {
		return types.Runner{}, s.runnerErr
	}
	rn, ok := s.runners[id]
	if !ok {
		return types.Runner{}, store.ErrNotFound
	}
	return rn, nil
}

func (s *macWindowStore) setRunner(rn types.Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runners[rn.ID] = rn
}

func (s *macWindowStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	err := s.getRunErr
	s.mu.Unlock()
	if err != nil {
		return types.AgentRun{}, err
	}
	return s.desktopSiteStore.GetRun(ctx, id)
}

// liveToken registers a personal API token that can still authenticate.
func (s *macWindowStore) liveToken(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = map[uuid.UUID]bool{}
	}
	s.tokens[id] = true
}

func (s *macWindowStore) GetLiveAPIToken(_ context.Context, id uuid.UUID) (types.APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tokens[id] {
		return types.APIToken{}, store.ErrNotFound
	}
	return types.APIToken{ID: id}, nil
}

// RevokeAPIToken is what DELETE /tokens/{id} reaches: the token stops reading as live.
func (s *macWindowStore) RevokeAPIToken(_ context.Context, id uuid.UUID, _ string, _ time.Time) (types.APIToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tokens[id] {
		return types.APIToken{}, store.ErrNotFound
	}
	delete(s.tokens, id)
	return types.APIToken{ID: id}, nil
}

// cliSession registers a CLI session for f to change, and returns its id.
func (s *macWindowStore) cliSession(f func(row *store.CLISessionRow)) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cliSessions == nil {
		s.cliSessions = map[uuid.UUID]store.CLISessionRow{}
	}
	row := store.CLISessionRow{ID: uuid.New(), ExpiresAt: time.Now().Add(24 * time.Hour)}
	if f != nil {
		f(&row)
	}
	s.cliSessions[row.ID] = row
	return row.ID
}

func (s *macWindowStore) GetCLISession(_ context.Context, id uuid.UUID) (store.CLISessionRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.cliSessions[id]
	if !ok {
		return store.CLISessionRow{}, store.ErrNotFound
	}
	return row, nil
}

func (s *macWindowStore) endCLISession(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, now := s.cliSessions[id], time.Now()
	row.EndedAt = &now
	s.cliSessions[id] = row
}

func (s *macWindowStore) failRunRead(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getRunErr = err
}

func (s *macWindowStore) GetPrincipalPref(_ context.Context, principal, key string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.prefs[principal+"\x00"+key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return v, nil
}

func (s *macWindowStore) PutPrincipalPref(_ context.Context, principal, key string, value json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prefs[principal+"\x00"+key] = value
	s.prefsWrite.Add(1)
	return nil
}

func (s *macWindowStore) ListPauseCandidates(context.Context) ([]store.PauseCandidate, time.Time, error) {
	return nil, time.Now(), nil
}
func (s *macWindowStore) StampRunActive(context.Context, uuid.UUID) (bool, error) {
	s.stamps.Add(1)
	return false, nil
}
func (s *macWindowStore) TouchRun(context.Context, uuid.UUID) error {
	s.touched.Add(1)
	return nil
}
func (s *macWindowStore) MarkRunPaused(context.Context, uuid.UUID, types.PauseReason, *time.Time) (bool, error) {
	return false, nil
}
func (s *macWindowStore) ClearRunPaused(context.Context, uuid.UUID) (bool, error) { return false, nil }
func (s *macWindowStore) RunHasOpenRequest(context.Context, uuid.UUID) (bool, error) {
	return false, nil
}

// fakeMacWindowSession is a runner's open window: end it with the runner's reason.
type fakeMacWindowSession struct {
	ended  chan struct{}
	once   sync.Once
	mu     sync.Mutex
	reason string
	closed atomic.Bool
}

func (f *fakeMacWindowSession) Ended() <-chan struct{} { return f.ended }
func (f *fakeMacWindowSession) EndReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reason
}
func (f *fakeMacWindowSession) Close() error { f.closed.Store(true); return nil }

// end is the runner ending the window for reason.
func (f *fakeMacWindowSession) end(reason string) {
	f.once.Do(func() {
		f.mu.Lock()
		f.reason = reason
		f.mu.Unlock()
		close(f.ended)
	})
}

// fakeMacWindowSub is the runner's end of the grant.
type fakeMacWindowSub struct {
	mu        sync.Mutex
	supported bool
	reported  bool
	err       error
	opens     []macWindowRequest
	sessions  []*fakeMacWindowSession
	onOpen    func()
}

func (f *fakeMacWindowSub) MacWindowSupported() (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.supported, f.reported
}

func (f *fakeMacWindowSub) MacWindow(_ context.Context, _ string, req macWindowRequest) (macWindowSession, error) {
	f.mu.Lock()
	hook, err := f.onOpen, f.err
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return nil, err
	}
	sess := &fakeMacWindowSession{ended: make(chan struct{})}
	f.mu.Lock()
	f.opens = append(f.opens, req)
	f.sessions = append(f.sessions, sess)
	f.mu.Unlock()
	return sess, nil
}

func (f *fakeMacWindowSub) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.opens)
}

func (f *fakeMacWindowSub) session(i int) *fakeMacWindowSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions[i]
}

// macWindowRig is a background macOS VM run on its person's own Mac, with the site switch on, a runner that can
// open a window and a re-check clock the test moves by sending on ticks.
type macWindowRig struct {
	t     *testing.T
	h     *uiHarness
	st    *macWindowStore
	sub   *fakeMacWindowSub
	ticks chan time.Time
	rev   *macWindowRevocations
}

const macWindowRunnerName = "Alice's MacBook"

func newMacWindowRig(t *testing.T) *macWindowRig {
	t.Helper()
	h, site := bgWatchHarness(t)
	on := true
	site.sc = types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowAttendedWindow: &on}}
	st := &macWindowStore{desktopSiteStore: site, runners: map[uuid.UUID]types.Runner{}, prefs: map[string]json.RawMessage{}}
	h.srv.cfg.Store = st
	rig := &macWindowRig{t: t, h: h, st: st, sub: &fakeMacWindowSub{supported: true, reported: true},
		ticks: make(chan time.Time), rev: &macWindowRevocations{}}
	rig.adoptRunner()
	h.srv.cfg.SessionRevocations = rig.rev
	h.srv.macWindowSubs = func(uuid.UUID) macWindowSub { return rig.sub }
	h.srv.macWindowTick = func(time.Duration) (<-chan time.Time, func()) { return rig.ticks, func() {} }
	t.Cleanup(func() {
		if w := h.srv.macWindows.open(h.run.ID); w != nil && w.sess != nil {
			h.srv.closeMacWindow(context.Background(), w, macWindowClosedByPerson, "")
		}
	})
	return rig
}

// adoptRunner makes the run's current runner a claimed one registered to the run's person, and the run local.
func (r *macWindowRig) adoptRunner() {
	r.t.Helper()
	id := *r.h.run.RunnerID
	r.st.setRunner(types.Runner{ID: id, Name: macWindowRunnerName, Owner: r.h.owner, State: types.RunnerClaimed, OwnerKind: types.RunnerOwnerPerson})
	r.h.run.Placement = types.PlacementLocal
	r.h.store.putRun(r.h.run)
}

// set changes the run and stores it.
func (r *macWindowRig) set(f func(run *types.AgentRun)) {
	r.t.Helper()
	f(&r.h.run)
	r.h.store.putRun(r.h.run)
}

// setRunner changes the run's runner row.
func (r *macWindowRig) setRunner(f func(rn *types.Runner)) {
	r.t.Helper()
	rn, err := r.st.GetRunner(context.Background(), *r.h.run.RunnerID)
	if err != nil {
		r.t.Fatal(err)
	}
	f(&rn)
	r.st.setRunner(rn)
}

// auth is the run's person as the console's signed-in session.
func (r *macWindowRig) auth() macWindowAuthority {
	return macWindowAuthority{ActorType: types.ActorHuman, Principal: r.h.owner, Email: r.h.owner + "@example.com", Issue: oidc.Issue{At: time.Now()}}
}

// rows are the recorded audit rows of action, oldest first.
func (r *macWindowRig) rows(action string) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range r.h.audit.snapshot() {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

func rowData(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("audit data %s: %v", ev.Data, err)
	}
	return m
}

// eventually waits for cond, which a background watcher makes true.
func macWindowEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// open grants a window to the run's person.
func (r *macWindowRig) open(trigger string) (*macWindow, *runRefusal) {
	r.t.Helper()
	return r.h.srv.openMacWindow(context.Background(), r.h.run, r.auth(), trigger)
}

// mustOpen opens a window and fails the test if it is refused.
func (r *macWindowRig) mustOpen() *macWindow {
	r.t.Helper()
	w, rf := r.open(macWindowTriggerCLI)
	if rf != nil {
		r.t.Fatalf("open refused: %+v", rf)
	}
	return w
}

// tick moves the re-check clock one interval.
func (r *macWindowRig) tick() {
	r.t.Helper()
	select {
	case r.ticks <- time.Now():
	case <-time.After(3 * time.Second):
		r.t.Fatal("the watcher took no tick")
	}
}

// closeRow waits for the window's takeover.close row and returns its data.
func (r *macWindowRig) closeRow() map[string]any {
	r.t.Helper()
	macWindowEventually(r.t, "takeover.close", func() bool { return len(r.rows("takeover.close")) > 0 })
	return rowData(r.t, r.rows("takeover.close")[0])
}

// post is POST /runs/{id}/desktop/window as sub.
func (r *macWindowRig) post(sub, body string) *httptest.ResponseRecorder {
	r.t.Helper()
	router := chi.NewRouter()
	r.h.srv.mountMacWindowRoutes(router)
	req := httptest.NewRequest(http.MethodPost, "/runs/"+r.h.run.ID.String()+"/desktop/window", strings.NewReader(body))
	req = req.WithContext(withHumanIdentity(req.Context(), sub, sub+"@example.com", oidc.RoleUser, types.UserTypeStandard, nil, false))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// One test per refusal row of macWindowRefusal, in its order: the reason, the status and the exact sentence,
// through the open, which also writes the denied takeover.begin row carrying that reason.
func TestMacWindowRefusalRows(t *testing.T) {
	notYours := func(runner string) string { return macWindowNotYourMacMsg(runner) }
	cases := []struct {
		name   string
		mut    func(r *macWindowRig, a *macWindowAuthority)
		status int
		reason string
		msg    string
	}{
		{"row 1: the site switch is absent", func(r *macWindowRig, _ *macWindowAuthority) {
			r.h.srv.cfg.Store.(*macWindowStore).sc = types.SiteConfig{}
		},
			http.StatusForbidden, reasonMacWindowSiteOff, macWindowSiteOffMsg},
		{"row 1: the site switch is false", func(r *macWindowRig, _ *macWindowAuthority) {
			off := false
			r.st.sc = types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowAttendedWindow: &off}}
		}, http.StatusForbidden, reasonMacWindowSiteOff, macWindowSiteOffMsg},
		{"row 2: not a macOS VM", func(r *macWindowRig, _ *macWindowAuthority) {
			r.set(func(run *types.AgentRun) { run.MachineKind = types.MachineLinuxVM })
		}, http.StatusConflict, reasonMacWindowNotMacOSVM, macWindowNotMacOSMsg},
		{"row 2: not running", func(r *macWindowRig, _ *macWindowAuthority) {
			r.set(func(run *types.AgentRun) { run.State = types.RunPending })
		}, http.StatusConflict, reasonAttachNotRunning, "run is not RUNNING; cannot open its window (state=" + string(types.RunPending) + ")"},
		{"row 2: kept", func(r *macWindowRig, _ *macWindowAuthority) {
			now := time.Now()
			r.set(func(run *types.AgentRun) { run.LostAt = &now })
		}, http.StatusConflict, reasonAttachRunKept, "run has ended; cannot open its window"},
		{"row 3: a runner registered to someone else", func(r *macWindowRig, _ *macWindowAuthority) {
			r.setRunner(func(rn *types.Runner) { rn.Owner = "bob" })
		}, http.StatusForbidden, reasonMacWindowNotYourMac, notYours(macWindowRunnerName)},
		{"row 3: an unclaimed runner", func(r *macWindowRig, _ *macWindowAuthority) {
			r.setRunner(func(rn *types.Runner) { rn.State = types.RunnerUnclaimed })
		}, http.StatusForbidden, reasonMacWindowNotYourMac, notYours(macWindowRunnerName)},
		{"row 3: a person who is not the run's creator", func(_ *macWindowRig, a *macWindowAuthority) { a.Principal = "bob" },
			http.StatusForbidden, reasonMacWindowNotYourMac, notYours(macWindowRunnerFallbackMsg)},
		{"row 3: a run placed remote", func(r *macWindowRig, _ *macWindowAuthority) {
			r.set(func(run *types.AgentRun) { run.Placement = types.PlacementRemote })
		}, http.StatusForbidden, reasonMacWindowNotYourMac, notYours(macWindowRunnerFallbackMsg)},
		{"row 3: a service identity", func(_ *macWindowRig, a *macWindowAuthority) { a.ActorType = types.ActorService },
			http.StatusForbidden, reasonMacWindowNotYourMac, notYours(macWindowRunnerFallbackMsg)},
		{"row 3: a system actor", func(_ *macWindowRig, a *macWindowAuthority) { a.ActorType = types.ActorSystem },
			http.StatusForbidden, reasonMacWindowNotYourMac, notYours(macWindowRunnerFallbackMsg)},
		{"row 3: a delegated portal ticket", func(_ *macWindowRig, a *macWindowAuthority) { a.Via = true },
			http.StatusForbidden, reasonMacWindowNotYourMac, notYours(macWindowRunnerFallbackMsg)},
		{"row 3: an operator-owned run", func(r *macWindowRig, _ *macWindowAuthority) {
			r.set(func(run *types.AgentRun) { run.OperatorOwned = true })
		}, http.StatusForbidden, reasonMacWindowNotYourMac, notYours(macWindowRunnerFallbackMsg)},
		{"row 3: local mode", func(r *macWindowRig, _ *macWindowAuthority) { r.h.srv.cfg.LocalMode = true },
			http.StatusForbidden, reasonMacWindowNotYourMac, notYours(macWindowRunnerFallbackMsg)},
		{"row 3: a runner that is not registered", func(r *macWindowRig, _ *macWindowAuthority) {
			r.set(func(run *types.AgentRun) { id := uuid.New(); run.RunnerID = &id })
		}, http.StatusForbidden, reasonMacWindowNotYourMac, notYours(macWindowRunnerFallbackMsg)},
		{"row 4: deny_interactive", func(r *macWindowRig, _ *macWindowAuthority) {
			walledRun(r.h)(types.GovernanceLimits{DenyInteractive: true})
		}, http.StatusForbidden, string(authz.ReasonGovernanceUnit), macWindowProfileMsg},
		{"row 4: deny_ui_apps", func(r *macWindowRig, _ *macWindowAuthority) {
			walledRun(r.h)(types.GovernanceLimits{DenyUIApps: true})
		}, http.StatusForbidden, string(authz.ReasonGovernanceUnit), macWindowProfileMsg},
		{"row 5: the recording level is Screen and actions", func(r *macWindowRig, _ *macWindowAuthority) {
			r.set(func(run *types.AgentRun) { run.RecordingLevel = types.RecordingScreenActions })
		}, http.StatusForbidden, reasonMacWindowUnrecorded, macWindowUnrecordedMsg},
		{"row 6: the agent's desktop sign-ins are off", func(r *macWindowRig, _ *macWindowAuthority) {
			on := true
			r.st.sc = types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowAttendedWindow: &on},
				AgentProviders: &types.AgentProviders{Agents: []types.AgentProvider{{ID: "claude-code", DesktopSignInsOff: true}}}}
		}, http.StatusForbidden, reasonDesktopSignInsOff, macOSDesktopSignInsOffMsg("claude-code")},
		{"row 7: the runner is offline", func(r *macWindowRig, _ *macWindowAuthority) {
			bgWatchPutRunner(r.h, false, bgWatchMac(runnerwire.MacVMDesktopObserve))
			r.adoptRunner()
		}, http.StatusConflict, reasonMacWindowOffline, macWindowOfflineMsg},
		{"row 7: the runner is connected through another server", func(r *macWindowRig, _ *macWindowAuthority) {
			r.set(func(run *types.AgentRun) { id := uuid.New(); run.RunnerID = &id })
			r.adoptRunner()
			r.h.srv.cfg.RunnerOnline = func(uuid.UUID) bool { return true }
		}, http.StatusConflict, reasonMacWindowNotHere, macWindowNotHereMsg},
		{"row 7: the runner has not reported its caps", func(r *macWindowRig, _ *macWindowAuthority) {
			r.sub.mu.Lock()
			r.sub.reported = false
			r.sub.mu.Unlock()
		}, http.StatusConflict, reasonMacWindowOffline, macWindowOfflineMsg},
		{"row 7: the runner's caps say it can't open a window", func(r *macWindowRig, _ *macWindowAuthority) {
			r.sub.mu.Lock()
			r.sub.supported = false
			r.sub.mu.Unlock()
		}, http.StatusConflict, reasonMacWindowRunnerTooOld, macWindowRunnerTooOldMsg},
		{"row 7: the runner has no window end at all", func(r *macWindowRig, _ *macWindowAuthority) {
			r.h.srv.macWindowSubs = func(uuid.UUID) macWindowSub { return nil }
		}, http.StatusConflict, reasonMacWindowRunnerTooOld, macWindowRunnerTooOldMsg},
		{"row 8: the run is paused", func(r *macWindowRig, _ *macWindowAuthority) {
			now := time.Now()
			r.set(func(run *types.AgentRun) { run.PausedAt = &now })
		}, http.StatusConflict, reasonMacWindowPaused, macWindowPausedMsg},
		{"row 9: the run is still being prepared (sandbox starting)", func(r *macWindowRig, _ *macWindowAuthority) {
			r.set(func(run *types.AgentRun) { run.State = types.RunStarting })
		}, http.StatusConflict, reasonMacWindowPreparing, macWindowPreparingMsg},
		{"row 9: an interactive macOS run is RUNNING but prep has not recorded done", func(r *macWindowRig, _ *macWindowAuthority) {
			r.set(func(run *types.AgentRun) { run.Interactive = true; run.StatusDetail = "" })
		}, http.StatusConflict, reasonMacWindowPreparing, macWindowPreparingMsg},
		{"row 10: a window is already open on the run", func(r *macWindowRig, _ *macWindowAuthority) { r.mustOpen() },
			http.StatusConflict, reasonMacWindowOpen, types.MacWindowOpenMsg(macWindowRunnerName)},
		{"runner side: the person is not at the Mac's unlocked screen", func(r *macWindowRig, _ *macWindowAuthority) {
			r.sub.mu.Lock()
			r.sub.err = errMacWindowNotAtMac
			r.sub.mu.Unlock()
		}, http.StatusConflict, reasonMacWindowNotAtMac, types.MacWindowNotAtMacMsg(macWindowRunnerName)},
		{"runner side: the Mac can't tell whether its screen is locked", func(r *macWindowRig, _ *macWindowAuthority) {
			r.sub.mu.Lock()
			r.sub.err = errMacWindowLockUnknown
			r.sub.mu.Unlock()
		}, http.StatusConflict, reasonMacWindowLockUnknown, macWindowLockUnknownMsg},
		{"runner side: the runner already holds a window for the run", func(r *macWindowRig, _ *macWindowAuthority) {
			r.sub.mu.Lock()
			r.sub.err = errMacWindowAlreadyOpen
			r.sub.mu.Unlock()
		}, http.StatusConflict, reasonMacWindowOpen, types.MacWindowOpenMsg(macWindowRunnerName)},
		{"the credential was cut off", func(r *macWindowRig, _ *macWindowAuthority) { r.rev.set(true, nil) },
			http.StatusForbidden, reasonMacWindowRevoked, macWindowRevokedMsg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newMacWindowRig(t)
			a := r.auth()
			tc.mut(r, &a)
			before := len(r.rows("takeover.begin"))
			w, rf := r.h.srv.openMacWindow(context.Background(), r.h.run, a, macWindowTriggerCLI)
			if w != nil || rf == nil {
				t.Fatalf("the open succeeded, want a refusal")
			}
			got, gotMsg, gotStatus := "", "", rf.status
			switch {
			case rf.decision != nil:
				got, gotMsg, gotStatus = string(rf.decision.Reason), rf.decision.Sentence, rf.decision.Status
				if rf.decision.Target != "runs.desktop_window" {
					t.Errorf("decision target = %q, want runs.desktop_window", rf.decision.Target)
				}
			default:
				got, gotMsg = rf.body.Reason, rf.body.Error
			}
			if got != tc.reason || gotMsg != tc.msg || (gotStatus != 0 && gotStatus != tc.status) {
				t.Errorf("refusal = %d %q %q, want %d %q %q", gotStatus, got, gotMsg, tc.status, tc.reason, tc.msg)
			}
			rows := r.rows("takeover.begin")[before:]
			denied := 0
			for _, ev := range rows {
				if ev.Outcome != "denied" {
					continue
				}
				denied++
				d := rowData(t, ev)
				if d["reason"] != tc.reason || d["surface"] != "mac_window" || d["recorded"] != false || d["trigger"] != "cli" {
					t.Errorf("denied row data = %v, want reason %q, surface mac_window, recorded false, trigger cli", d, tc.reason)
				}
			}
			if denied != 1 {
				t.Errorf("denied rows = %d, want exactly one", denied)
			}
		})
	}
}

// A run that fails two rows answers the earlier: the site switch before the runner before governance before
// recording before the rest.
func TestMacWindowRefusalOrder(t *testing.T) {
	r := newMacWindowRig(t)
	r.set(func(run *types.AgentRun) {
		run.RecordingLevel = types.RecordingScreenActions
		run.State = types.RunStarting
	})
	r.setRunner(func(rn *types.Runner) { rn.Owner = "bob" })
	walledRun(r.h)(types.GovernanceLimits{DenyInteractive: true})
	r.st.sc = types.SiteConfig{}
	a := r.auth()
	steps := []struct {
		fix  func()
		want string
	}{
		{func() {}, reasonMacWindowSiteOff},
		{func() {
			on := true
			r.st.sc = types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowAttendedWindow: &on}}
		}, reasonMacWindowNotYourMac},
		{func() { r.setRunner(func(rn *types.Runner) { rn.Owner = r.h.owner }) }, string(authz.ReasonGovernanceUnit)},
		{func() { walledRun(r.h)(types.GovernanceLimits{}) }, reasonMacWindowUnrecorded},
		{func() { r.set(func(run *types.AgentRun) { run.RecordingLevel = "" }) }, reasonMacWindowPreparing},
	}
	for _, st := range steps {
		st.fix()
		rf := r.h.srv.macWindowRefusal(context.Background(), r.h.run, a, macWindowAsk{})
		if rf == nil || macWindowRefusalReason(rf) != st.want {
			t.Fatalf("refusal = %+v, want %s", rf, st.want)
		}
	}
	r.set(func(run *types.AgentRun) { run.State = types.RunRunning })
	if rf := r.h.srv.macWindowRefusal(context.Background(), r.h.run, a, macWindowAsk{}); rf != nil {
		t.Fatalf("refusal = %+v, want none once every row passes", rf)
	}
}

// An unreadable runner, site config or profile refuses (fail closed).
func TestMacWindowRefusalFailsClosed(t *testing.T) {
	t.Run("the runner can't be read", func(t *testing.T) {
		r := newMacWindowRig(t)
		r.st.mu.Lock()
		r.st.runnerErr = errors.New("conn closed by peer")
		r.st.mu.Unlock()
		if rf := r.h.srv.macWindowRefusal(context.Background(), r.h.run, r.auth(), macWindowAsk{}); rf == nil || rf.err == nil {
			t.Fatalf("refusal = %+v, want a read failure that refuses", rf)
		}
	})
	t.Run("the site config can't be read", func(t *testing.T) {
		r := newMacWindowRig(t)
		r.st.err = errors.New("conn closed by peer")
		if rf := r.h.srv.macWindowRefusal(context.Background(), r.h.run, r.auth(), macWindowAsk{}); rf == nil || rf.err == nil {
			t.Fatalf("refusal = %+v, want a read failure that refuses", rf)
		}
	})
}

// Every row passing, the route answers the grant and writes takeover.begin with the fields consumers filter on,
// for a background run and an interactive one; a refusal answers with its sentence.
func TestMacWindowRouteGrantAndAudit(t *testing.T) {
	for _, bg := range []bool{true, false} {
		name := "interactive run"
		if bg {
			name = "background run"
		}
		t.Run(name, func(t *testing.T) {
			r := newMacWindowRig(t)
			if !bg {
				r.set(func(run *types.AgentRun) {
					run.Experience = types.ExperienceInteractive
					run.Interactive = true
					run.StatusDetail = statusDetailMacPrepDone
				})
			}
			rec := r.post(r.h.owner, `{"trigger":"run_page"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("code = %d %s", rec.Code, rec.Body.String())
			}
			var grant macWindowGrant
			if err := json.Unmarshal(rec.Body.Bytes(), &grant); err != nil || grant.GrantID == uuid.Nil || grant.Trigger != "run_page" || grant.Runner != macWindowRunnerName {
				t.Fatalf("grant = %+v, %v", grant, err)
			}
			if got := r.sub.opens[0]; got.GrantID != grant.GrantID || got.Person != r.h.owner || got.Title == "" {
				t.Errorf("the runner was asked %+v, want the grant, the person and a title", got)
			}
			begin := r.rows("takeover.begin")
			if len(begin) != 1 || begin[0].Outcome != "success" || begin[0].Target != grant.GrantID.String() || begin[0].Actor != r.h.owner {
				t.Fatalf("begin rows = %+v", begin)
			}
			d := rowData(t, begin[0])
			if d["surface"] != "mac_window" || d["grant_id"] != grant.GrantID.String() || d["runner_id"] != grant.RunnerID.String() ||
				d["trigger"] != "run_page" || d["background"] != bg || d["recorded"] != false {
				t.Errorf("begin data = %v", d)
			}
			// The person closes it on the Mac.
			r.sub.session(0).end("closed_by_person")
			c := r.closeRow()
			if c["grant_id"] != grant.GrantID.String() || c["reason"] != "closed_by_person" || c["surface"] != "mac_window" ||
				c["recorded"] != false || c["background"] != bg || c["runner_id"] != grant.RunnerID.String() {
				t.Errorf("close data = %v", c)
			}
			if _, ok := c["held_ms"]; !ok {
				t.Error("close row has no held_ms")
			}
			macWindowEventually(t, "the registry to empty", func() bool { return r.h.srv.macWindows.open(r.h.run.ID) == nil })
		})
	}
	t.Run("a refusal answers with the sentence and the status", func(t *testing.T) {
		r := newMacWindowRig(t)
		r.st.sc = types.SiteConfig{}
		rec := r.post(r.h.owner, ``)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), macWindowSiteOffMsg) || !strings.Contains(rec.Body.String(), reasonMacWindowSiteOff) {
			t.Fatalf("answer = %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a trigger the API doesn't know is a 400", func(t *testing.T) {
		r := newMacWindowRig(t)
		if rec := r.post(r.h.owner, `{"trigger":"launch"}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400 (launch is the server's own)", rec.Code)
		}
		if r.sub.openCount() != 0 {
			t.Error("the runner was asked")
		}
	})
}

// The registry allows one window per run on a replica, and the second grant is refused with the sentence the
// runner uses (types.MacWindowOpenMsg), without the runner being asked.
func TestMacWindowOneWindowPerRun(t *testing.T) {
	r := newMacWindowRig(t)
	r.mustOpen()
	_, rf := r.open(macWindowTriggerCLI)
	if rf == nil || rf.status != http.StatusConflict || rf.body.Reason != reasonMacWindowOpen || rf.body.Error != types.MacWindowOpenMsg(macWindowRunnerName) {
		t.Fatalf("second grant = %+v, want 409 %s %q", rf, reasonMacWindowOpen, types.MacWindowOpenMsg(macWindowRunnerName))
	}
	if r.sub.openCount() != 1 {
		t.Errorf("the runner was asked %d times, want once", r.sub.openCount())
	}
	t.Run("two grants at once reach the runner once", func(t *testing.T) {
		r := newMacWindowRig(t)
		var wg sync.WaitGroup
		var granted atomic.Int32
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if w, rf := r.open(macWindowTriggerCLI); w != nil && rf == nil {
					granted.Add(1)
				}
			}()
		}
		wg.Wait()
		if granted.Load() != 1 || r.sub.openCount() != 1 {
			t.Fatalf("granted %d, runner asked %d, want one each", granted.Load(), r.sub.openCount())
		}
	})
	t.Run("a closed window frees the slot, and the reopen is a new grant", func(t *testing.T) {
		r := newMacWindowRig(t)
		first := r.mustOpen()
		r.sub.session(0).end("closed_by_person")
		macWindowEventually(t, "the registry to empty", func() bool { return r.h.srv.macWindows.open(r.h.run.ID) == nil })
		second := r.mustOpen()
		if first.grantID == second.grantID {
			t.Error("the reopen reused the grant id")
		}
	})
	// A window the Mac's lock closed stays closed: nothing reopens it on unlock, and a reopen is a new grant that
	// the runner is asked for again.
	t.Run("a reopen after a lock close is a new grant", func(t *testing.T) {
		r := newMacWindowRig(t)
		first := r.mustOpen()
		r.sub.session(0).end(macWindowHostLocked)
		if c := r.closeRow(); c["reason"] != macWindowHostLocked {
			t.Fatalf("close reason = %v, want %s", c["reason"], macWindowHostLocked)
		}
		macWindowEventually(t, "the registry to empty", func() bool { return r.h.srv.macWindows.open(r.h.run.ID) == nil })
		if r.sub.openCount() != 1 {
			t.Fatalf("the runner was asked %d times before any reopen, want once", r.sub.openCount())
		}
		second := r.mustOpen()
		if first.grantID == second.grantID || r.sub.openCount() != 2 {
			t.Fatalf("reopen: grant reused = %v, runner asked %d times", first.grantID == second.grantID, r.sub.openCount())
		}
		r.sub.mu.Lock()
		g0, g1 := r.sub.opens[0].GrantID, r.sub.opens[1].GrantID
		r.sub.mu.Unlock()
		if g0 != first.grantID || g1 != second.grantID {
			t.Errorf("the runner got grants %v then %v, want %v then %v", g0, g1, first.grantID, second.grantID)
		}
	})
}

// A revoke, a pause, a changed runner owner, the end of the run and the switch turned off each end an open
// window within one re-check; the close row says which. The clock is the test's.
func TestMacWindowRecheckEndsTheWindow(t *testing.T) {
	cases := []struct {
		name   string
		change func(r *macWindowRig)
		reason string
		by     string
	}{
		{"the person's credential is revoked", func(r *macWindowRig) { r.rev.set(true, nil) }, macWindowRevoked, ""},
		{"the run is paused", func(r *macWindowRig) {
			now := time.Now()
			r.set(func(run *types.AgentRun) { run.PausedAt = &now })
		}, macWindowRefusedRecheck, reasonMacWindowPaused},
		{"the runner's owner changed", func(r *macWindowRig) {
			r.setRunner(func(rn *types.Runner) { rn.Owner = "bob" })
		}, macWindowRefusedRecheck, reasonMacWindowNotYourMac},
		{"the run ended", func(r *macWindowRig) {
			r.set(func(run *types.AgentRun) { run.State = types.RunKilled })
		}, macWindowRunEnded, ""},
		{"the run is kept after its end", func(r *macWindowRig) {
			now := time.Now()
			r.set(func(run *types.AgentRun) { run.LostAt = &now })
		}, macWindowRunEnded, ""},
		{"the site switch is turned off", func(r *macWindowRig) { r.st.setSite(types.SiteConfig{}, nil) }, macWindowRefusedRecheck, reasonMacWindowSiteOff},
		{"the profile comes to deny interactive desktops", func(r *macWindowRig) {
			walledRun(r.h)(types.GovernanceLimits{DenyInteractive: true})
		}, macWindowRefusedRecheck, string(authz.ReasonGovernanceUnit)},
		{"the run's recording level becomes Screen and actions", func(r *macWindowRig) {
			r.set(func(run *types.AgentRun) { run.RecordingLevel = types.RecordingScreenActions })
		}, macWindowRefusedRecheck, reasonMacWindowUnrecorded},
		{"the runner goes offline", func(r *macWindowRig) {
			r.h.srv.runnerStreams.subsMu.Lock()
			delete(r.h.srv.runnerStreams.subs, *r.h.run.RunnerID)
			r.h.srv.runnerStreams.subsMu.Unlock()
		}, macWindowRunnerOffline, reasonMacWindowOffline},
		// Fail closed: a re-check that can't read ends the window.
		{"the credential can't be read", func(r *macWindowRig) { r.rev.set(false, errors.New("conn closed by peer")) },
			macWindowRefusedRecheck, uiDeniedReasonRevocationUnavailable},
		{"the run can't be read", func(r *macWindowRig) { r.st.failRunRead(errors.New("conn closed by peer")) },
			macWindowRefusedRecheck, reasonRunUnreadable},
		{"the site config can't be read", func(r *macWindowRig) {
			on := true
			r.st.setSite(types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowAttendedWindow: &on}}, errors.New("conn closed by peer"))
		},
			macWindowRefusedRecheck, reasonRunUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newMacWindowRig(t)
			w := r.mustOpen()
			r.tick() // nothing changed: the window stays
			time.Sleep(20 * time.Millisecond)
			if len(r.rows("takeover.close")) != 0 || r.h.srv.macWindows.open(r.h.run.ID) != w {
				t.Fatal("the window ended with nothing changed")
			}
			tc.change(r)
			r.tick()
			c := r.closeRow()
			if c["reason"] != tc.reason || c["refused_by"] != orNil(tc.by) {
				t.Errorf("close = reason %v refused_by %v, want %q %q", c["reason"], c["refused_by"], tc.reason, tc.by)
			}
			if !r.sub.session(0).closed.Load() {
				t.Error("the runner's session was not closed, so its view stays up")
			}
			macWindowEventually(t, "the registry to empty", func() bool { return r.h.srv.macWindows.open(r.h.run.ID) == nil })
			if got := len(r.rows("takeover.close")); got != 1 {
				t.Errorf("close rows = %d, want one", got)
			}
		})
	}
}

// orNil is a close row's refused_by for a check that names none: the field is absent.
func orNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// What the runner reports when it ends a window is the close reason; a reason it doesn't know reads as link_lost.
func TestMacWindowRunnerEnds(t *testing.T) {
	for in, want := range map[string]string{"closed_by_person": "closed_by_person", "host_locked": "host_locked",
		"helper_failed": "helper_failed", "link_lost": "link_lost", "run_ended": "run_ended", "helper_lost": "link_lost", "": "link_lost", "anything": "link_lost"} {
		r := newMacWindowRig(t)
		r.mustOpen()
		r.sub.session(0).end(in)
		if c := r.closeRow(); c["reason"] != want {
			t.Errorf("runner reason %q: close reason = %v, want %q", in, c["reason"], want)
		}
	}
}

// Prep done on an interactive macOS run lets the window open.
func TestMacWindowOpensOnceInteractivePrepIsDone(t *testing.T) {
	r := newMacWindowRig(t)
	r.set(func(run *types.AgentRun) { run.Interactive = true; run.StatusDetail = statusDetailMacPrepDone })
	if rf := r.h.srv.macWindowRefusal(context.Background(), r.h.run, r.auth(), macWindowAsk{}); rf != nil {
		t.Fatalf("refusal = %+v, want none once prep is done", rf)
	}
}

// TouchRun is called when a window opens and on each re-check that lets it stay, and on no re-check that ends
// it; over N re-checks the calls never pass one per re-check plus the open. This pins the calls only; it does
// not exercise the idle reaper.
func TestMacWindowKeepsTheRunActive(t *testing.T) {
	t.Run("the open and each re-check that lets it stay touch the run", func(t *testing.T) {
		r := newMacWindowRig(t)
		r.mustOpen()
		if got := r.st.touched.Load(); got != 1 {
			t.Fatalf("touches after the open = %d, want 1", got)
		}
		const n = 4
		for i := 1; i <= n; i++ {
			r.tick()
			want := int32(1 + i)
			macWindowEventually(t, "a touch per re-check", func() bool { return r.st.touched.Load() >= want })
		}
		if got := r.st.touched.Load(); got > n+1 {
			t.Errorf("touches over %d re-checks = %d, want at most one per re-check plus the open (%d)", n, got, n+1)
		}
		if r.st.stamps.Load() < 1 {
			t.Error("the presence stamp was dropped")
		}
		if macWindowRecheckEvery < lifecycle.TouchDebounce {
			t.Errorf("the re-check interval %v is below the reaper's TouchDebounce %v", macWindowRecheckEvery, lifecycle.TouchDebounce)
		}
	})
	for _, tc := range []struct {
		name   string
		change func(r *macWindowRig)
	}{
		{"a refused re-check", func(r *macWindowRig) {
			now := time.Now()
			r.set(func(run *types.AgentRun) { run.PausedAt = &now })
		}},
		{"a revoked credential", func(r *macWindowRig) { r.rev.set(true, nil) }},
		{"a runner gone offline", func(r *macWindowRig) {
			r.h.srv.runnerStreams.subsMu.Lock()
			delete(r.h.srv.runnerStreams.subs, *r.h.run.RunnerID)
			r.h.srv.runnerStreams.subsMu.Unlock()
		}},
		{"an ended run", func(r *macWindowRig) { r.set(func(run *types.AgentRun) { run.State = types.RunKilled }) }},
	} {
		t.Run(tc.name+" does not touch", func(t *testing.T) {
			r := newMacWindowRig(t)
			r.mustOpen()
			r.tick()
			macWindowEventually(t, "the staying re-check's touch", func() bool { return r.st.touched.Load() >= 2 })
			before := r.st.touched.Load()
			tc.change(r)
			r.tick()
			r.closeRow()
			if got := r.st.touched.Load(); got != before {
				t.Errorf("touches = %d after the window lost its authority, want %d", got, before)
			}
		})
	}
}

// F1: a create with desktop_window at_start leaves a pending grant, and the launch opens under it. No record,
// no open; a creator revoked between the create and the start is refused with a denied row; a revoke mid-window
// ends the window within one re-check.
func TestMacWindowAtStartPendingGrant(t *testing.T) {
	t.Run("the launch opens under the stored grant", func(t *testing.T) {
		r := newMacWindowRig(t)
		a := r.auth()
		if err := r.h.srv.macWindowStorePending(context.Background(), r.h.run.ID, a); err != nil {
			t.Fatal(err)
		}
		r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
		if r.sub.openCount() != 1 {
			t.Fatalf("the runner was asked %d times, want once", r.sub.openCount())
		}
		b := r.rows("takeover.begin")
		if len(b) != 1 || b[0].Outcome != "success" || rowData(t, b[0])["trigger"] != "launch" {
			t.Fatalf("begin rows = %+v, want one success with trigger launch", b)
		}
	})
	t.Run("no record, no open", func(t *testing.T) {
		r := newMacWindowRig(t)
		r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
		b := r.rows("takeover.begin")
		if r.sub.openCount() != 0 || len(b) != 1 || b[0].Outcome != "denied" || rowData(t, b[0])["reason"] != macWindowNoGrant {
			t.Fatalf("opens %d, begin rows %+v, want none and one denied row (%s)", r.sub.openCount(), b, macWindowNoGrant)
		}
	})
	t.Run("a record kept for someone else is no record", func(t *testing.T) {
		r := newMacWindowRig(t)
		a := r.auth()
		raw, _ := json.Marshal(macWindowPending{ActorType: types.ActorHuman, Principal: "bob", AuthorizedAt: time.Now()})
		r.st.prefs[a.Principal+"\x00"+macWindowPendingKey+r.h.run.ID.String()] = raw
		r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
		if r.sub.openCount() != 0 {
			t.Fatal("the runner was asked under another person's record")
		}
	})
	t.Run("the creator revoked between create and start is refused with a denied row", func(t *testing.T) {
		r := newMacWindowRig(t)
		if err := r.h.srv.macWindowStorePending(context.Background(), r.h.run.ID, r.auth()); err != nil {
			t.Fatal(err)
		}
		r.rev.set(true, nil)
		r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
		b := r.rows("takeover.begin")
		if r.sub.openCount() != 0 || len(b) != 1 || b[0].Outcome != "denied" || rowData(t, b[0])["reason"] != reasonMacWindowRevoked {
			t.Fatalf("opens %d, begin rows %+v, want none and one denied (%s)", r.sub.openCount(), b, reasonMacWindowRevoked)
		}
	})
	t.Run("a revoke mid-window ends the launch's window within one re-check", func(t *testing.T) {
		r := newMacWindowRig(t)
		if err := r.h.srv.macWindowStorePending(context.Background(), r.h.run.ID, r.auth()); err != nil {
			t.Fatal(err)
		}
		r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
		r.rev.set(true, nil)
		r.tick()
		if c := r.closeRow(); c["reason"] != macWindowRevoked || c["trigger"] != "launch" {
			t.Fatalf("close = %v, want revoked on the launch's window", c)
		}
	})
	t.Run("a locked screen at start leaves the run going with a denied row", func(t *testing.T) {
		r := newMacWindowRig(t)
		r.sub.err = errMacWindowNotAtMac
		if err := r.h.srv.macWindowStorePending(context.Background(), r.h.run.ID, r.auth()); err != nil {
			t.Fatal(err)
		}
		r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
		b := r.rows("takeover.begin")
		if len(b) != 1 || b[0].Outcome != "denied" || rowData(t, b[0])["reason"] != reasonMacWindowNotAtMac {
			t.Fatalf("begin rows = %+v", b)
		}
	})
	t.Run("a run still preparing at the open is refused with the preparing sentence", func(t *testing.T) {
		r := newMacWindowRig(t)
		r.set(func(run *types.AgentRun) { run.State = types.RunStarting })
		if err := r.h.srv.macWindowStorePending(context.Background(), r.h.run.ID, r.auth()); err != nil {
			t.Fatal(err)
		}
		r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
		b := r.rows("takeover.begin")
		if r.sub.openCount() != 0 || len(b) != 1 || rowData(t, b[0])["reason"] != reasonMacWindowPreparing {
			t.Fatalf("opens %d, begin rows %+v, want none and a preparing refusal", r.sub.openCount(), b)
		}
	})
	t.Run("the record round-trips", func(t *testing.T) {
		r := newMacWindowRig(t)
		a := r.auth()
		a.Issue.Seq, a.Token, a.TokenID, a.CLISessionID = 41, true, uuid.New(), uuid.New()
		if err := r.h.srv.macWindowStorePending(context.Background(), r.h.run.ID, a); err != nil {
			t.Fatal(err)
		}
		got, ok, err := r.h.srv.macWindowLoadPending(context.Background(), r.h.run)
		if err != nil || !ok || got.Principal != a.Principal || got.Email != a.Email || got.Issue.Seq != 41 || !got.Token || !got.Issue.At.Equal(a.Issue.At) ||
			got.TokenID != a.TokenID || got.CLISessionID != a.CLISessionID {
			t.Fatalf("loaded %+v ok=%v err=%v, want %+v", got, ok, err, a)
		}
	})
}

// F7: a window opened over a browser desktop that has control ends that session first, so the runner is told to
// open only when every browser session is view only or closed; a session that won't close refuses the open.
func TestMacWindowYieldsBrowserControl(t *testing.T) {
	t.Run("a control session is closed before the runner is asked", func(t *testing.T) {
		r := newMacWindowRig(t)
		ctx, cancel := context.WithCancel(context.Background())
		untrack := r.h.srv.desktopControl.track(r.h.run.ID, cancel)
		go func() { <-ctx.Done(); untrack() }()
		var endedBeforeOpen, viewOnlyAtOpen atomic.Bool
		r.sub.onOpen = func() {
			endedBeforeOpen.Store(ctx.Err() != nil)
			viewOnlyAtOpen.Store(r.h.srv.macOSDesktopViewOnly(r.h.run))
		}
		r.mustOpen()
		if !endedBeforeOpen.Load() {
			t.Error("the runner was told to open while a browser session still had control")
		}
		if !viewOnlyAtOpen.Load() {
			t.Error("a new browser session was not forced view only at the open")
		}
	})
	t.Run("a session that does not close refuses the open with its own sentence", func(t *testing.T) {
		old := macWindowYieldWait
		macWindowYieldWait = 50 * time.Millisecond
		t.Cleanup(func() { macWindowYieldWait = old })
		r := newMacWindowRig(t)
		r.h.srv.desktopControl.track(r.h.run.ID, func() {}) // never forgets itself
		w, rf := r.open(macWindowTriggerCLI)
		if w != nil || rf == nil || rf.body.Reason != reasonMacWindowDesktopControl || rf.body.Error != macWindowDesktopControlMsg {
			t.Fatalf("open = %v %+v, want 409 %s %q", w, rf, reasonMacWindowDesktopControl, macWindowDesktopControlMsg)
		}
		if r.sub.openCount() != 0 || r.h.srv.macWindows.open(r.h.run.ID) != nil {
			t.Error("the runner was asked, or the slot was kept")
		}
		if b := r.rows("takeover.begin"); len(b) != 1 || b[0].Outcome != "denied" {
			t.Errorf("begin rows = %+v, want one denied", b)
		}
	})
	t.Run("the browser desktop is view only while a window is open, whatever the Mac offers", func(t *testing.T) {
		r := newMacWindowRig(t)
		bgWatchPutRunner(r.h, true, bgWatchMac(runnerwire.MacVMDesktopControl))
		r.adoptRunner()
		if r.h.srv.macOSDesktopViewOnly(r.h.run) {
			t.Fatal("a Mac that offers control is view only with no window open")
		}
		w := r.mustOpen()
		if !r.h.srv.macOSDesktopViewOnly(r.h.run) {
			t.Error("the browser desktop keeps control beside an open window")
		}
		r.h.srv.closeMacWindow(context.Background(), w, macWindowClosedByPerson, "")
		if r.h.srv.macOSDesktopViewOnly(r.h.run) {
			t.Error("the browser desktop stays view only after the window closed")
		}
	})
	t.Run("a Mac that offers control still gets a window", func(t *testing.T) {
		r := newMacWindowRig(t)
		bgWatchPutRunner(r.h, true, bgWatchMac(runnerwire.MacVMDesktopControl))
		r.adoptRunner()
		if rf := r.h.srv.macWindowRefusal(context.Background(), r.h.run, r.auth(), macWindowAsk{}); rf != nil {
			t.Fatalf("refusal = %+v, want none: one keyboard at a time is enforced by yielding, not refused", rf)
		}
	})
}

// F4: the window's route sits in the desktop door's Routes with no symbol of its own, and the handler asks
// macWindowRefusal's function, never backgroundOnlyRefusal, so a background run's owner can sit at it.
func TestMacWindowIsTheDoorsSecondException(t *testing.T) {
	const route = "POST /runs/{id}/desktop/window"
	var found bool
	for _, door := range backgroundDoors {
		for _, rt := range door.Routes {
			if rt == route {
				found = true
				if door.Door != "desktop" {
					t.Errorf("%s is listed under %q, want the desktop door", route, door.Door)
				}
			}
		}
		for _, sym := range door.Symbols {
			if strings.Contains(sym, "MacWindow") {
				t.Errorf("door %q lists %s as a symbol: the window is an exception, asked in macWindowRefusal", door.Door, sym)
			}
		}
	}
	if !found {
		t.Errorf("%s is in no door's Routes", route)
	}
	if !runRoutes(t)[route] {
		t.Errorf("%s is not registered", route)
	}
	bodies := backgroundDoorBodies(t)
	for _, sym := range []string{"handleMacWindow", "openMacWindow", "macWindowCheck"} {
		body, ok := bodies[sym]
		if !ok {
			t.Fatalf("%s is not declared", sym)
		}
		if callsBackgroundPredicate(body) {
			t.Errorf("%s asks the background predicate: a background run's window must not be refused as background", sym)
		}
	}
}

// F5, the org half: a window the runner reports with no grant from this control plane is audited as unexpected.
func TestMacWindowUnexpectedIsAudited(t *testing.T) {
	r := newMacWindowRig(t)
	id := *r.h.run.RunnerID
	r.h.srv.auditMacWindowUnexpected(context.Background(), id, &r.h.run.ID, strings.Repeat("g", 100))
	b := r.rows("takeover.begin")
	if len(b) != 1 || b[0].Outcome != "denied" {
		t.Fatalf("rows = %+v, want one row", b)
	}
	d := rowData(t, b[0])
	if d["reason"] != "mac_window_unexpected" || d["surface"] != "mac_window" || d["runner_id"] != id.String() || len(d["grant_id"].(string)) != 64 {
		t.Errorf("data = %v", d)
	}
}

// GET /runs/{id}'s mac_window: open is the only state with a button, each refusal names its state and carries its
// sentence, a run that isn't a live macOS VM has none, and someone else's run says only that.
func TestMacWindowViewStates(t *testing.T) {
	r := newMacWindowRig(t)
	ctx := context.Background()
	if v := r.h.srv.macWindowViewFor(ctx, r.h.run, r.auth()); v == nil || v.State != macWindowStateOpen || v.Message != "" {
		t.Fatalf("view = %+v, want open", v)
	}
	bob := r.auth()
	bob.Principal = "bob"
	if v := r.h.srv.macWindowViewFor(ctx, r.h.run, bob); v == nil || v.State != macWindowStateNotRunOwner || v.Message != "" {
		t.Fatalf("view for another person = %+v, want not_run_owner with no sentence", v)
	}
	r.mustOpen()
	if v := r.h.srv.macWindowViewFor(ctx, r.h.run, r.auth()); v == nil || v.State != macWindowStateWindowOpen || v.Message != types.MacWindowOpenMsg(macWindowRunnerName) {
		t.Fatalf("view with a window open = %+v", v)
	}
	r2 := newMacWindowRig(t)
	r2.st.sc = types.SiteConfig{}
	if v := r2.h.srv.macWindowViewFor(ctx, r2.h.run, r2.auth()); v == nil || v.State != macWindowStateSiteOff || v.Message != macWindowSiteOffMsg {
		t.Fatalf("view with the switch off = %+v", v)
	}
	r2.set(func(run *types.AgentRun) { run.State = types.RunStarting })
	r2.st.sc = r.st.sc
	if v := r2.h.srv.macWindowViewFor(ctx, r2.h.run, r2.auth()); v == nil || v.State != macWindowStatePreparing || v.Message != macWindowPreparingMsg {
		t.Fatalf("view while preparing = %+v", v)
	}
	r2.set(func(run *types.AgentRun) { run.State = types.RunRunning; run.MachineKind = types.MachineLinuxVM })
	if v := r2.h.srv.macWindowViewFor(ctx, r2.h.run, r2.auth()); v != nil {
		t.Fatalf("view of a Linux VM = %+v, want none", v)
	}
	r3 := newMacWindowRig(t)
	walledRun(r3.h)(types.GovernanceLimits{DenyInteractive: true})
	if v := r3.h.srv.macWindowViewFor(ctx, r3.h.run, r3.auth()); v == nil || v.State != macWindowStateGovernance || v.Message != macWindowProfileMsg {
		t.Fatalf("view under a profile = %+v", v)
	}
	r4 := newMacWindowRig(t)
	r4.setRunner(func(rn *types.Runner) { rn.Owner = "bob" })
	if v := r4.h.srv.macWindowViewFor(ctx, r4.h.run, r4.auth()); v == nil || v.State != macWindowStateNotYourMac {
		t.Fatalf("view on someone else's Mac = %+v", v)
	}
}

// The run read carries why the person's last window closed, in the console's four words; a close the person,
// the Mac or the run's end didn't cause reads as failed.
func TestMacWindowLastCloseOnTheRunRead(t *testing.T) {
	for in, want := range map[string]string{macWindowClosedByPerson: "person", macWindowHostLocked: "host_locked", macWindowRunEnded: "run_ended",
		macWindowHelperFailed: "failed", macWindowLinkLost: "failed", macWindowRevoked: "failed", macWindowRefusedRecheck: "failed", macWindowRunnerOffline: "failed"} {
		r := newMacWindowRig(t)
		if v := r.h.srv.macWindowViewFor(context.Background(), r.h.run, r.auth()); v == nil || v.LastClose != nil {
			t.Fatalf("view before any window = %+v, want no last_close", v)
		}
		w := r.mustOpen()
		r.h.srv.closeMacWindow(context.Background(), w, in, "")
		v := r.h.srv.macWindowViewFor(context.Background(), r.h.run, r.auth())
		if v == nil || v.LastClose == nil || v.LastClose.Reason != want || v.LastClose.At.IsZero() {
			t.Errorf("close %q: view = %+v, want last_close %q", in, v, want)
		}
		bob := r.auth()
		bob.Principal = "bob"
		if v := r.h.srv.macWindowViewFor(context.Background(), r.h.run, bob); v == nil || v.LastClose != nil {
			t.Errorf("another person's view = %+v, want no last_close", v)
		}
	}
}

// A window a create asked for that is refused as the run starts is the grant principal's last close, not_opened
// with the refusal's sentence, so the run page says why; with no grant, or no run to read, nothing is kept.
func TestMacWindowAtStartRefusalOnTheRunRead(t *testing.T) {
	atStart := func(t *testing.T, mutate func(r *macWindowRig), grant bool) *macWindowRig {
		t.Helper()
		r := newMacWindowRig(t)
		if grant {
			if err := r.h.srv.macWindowStorePending(context.Background(), r.h.run.ID, r.auth()); err != nil {
				t.Fatal(err)
			}
		}
		mutate(r)
		r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
		return r
	}
	t.Run("a refusal under the grant keeps not_opened with its sentence", func(t *testing.T) {
		r := atStart(t, func(r *macWindowRig) { r.st.sc = types.SiteConfig{} }, true)
		v := r.h.srv.macWindowViewFor(context.Background(), r.h.run, r.auth())
		if v == nil || v.LastClose == nil || v.LastClose.Reason != macWindowEndedNotOpened || v.LastClose.Message != macWindowSiteOffMsg || v.LastClose.At.IsZero() {
			t.Fatalf("view = %+v, want last_close not_opened with the site-off sentence", v)
		}
		if b := r.rows("takeover.begin"); len(b) != 1 || b[0].Outcome != "denied" || rowData(t, b[0])["reason"] != reasonMacWindowSiteOff {
			t.Errorf("begin rows = %+v, want the one denied row as before", b)
		}
		bob := r.auth()
		bob.Principal = "bob"
		if v := r.h.srv.macWindowViewFor(context.Background(), r.h.run, bob); v == nil || v.LastClose != nil {
			t.Errorf("another person's view = %+v, want no last_close", v)
		}
	})
	t.Run("the Mac's screen locked at start keeps the Mac's sentence", func(t *testing.T) {
		r := atStart(t, func(r *macWindowRig) { r.sub.err = errMacWindowNotAtMac }, true)
		v := r.h.srv.macWindowViewFor(context.Background(), r.h.run, r.auth())
		if v == nil || v.LastClose == nil || v.LastClose.Reason != macWindowEndedNotOpened || !strings.Contains(v.LastClose.Message, "didn't open") {
			t.Fatalf("view = %+v, want last_close not_opened with the not-at-the-Mac sentence", v)
		}
	})
	t.Run("an open at start keeps nothing", func(t *testing.T) {
		r := atStart(t, func(*macWindowRig) {}, true)
		if v := r.h.srv.macWindowViewFor(context.Background(), r.h.run, r.auth()); v == nil || v.LastClose != nil {
			t.Fatalf("view = %+v, want no last_close", v)
		}
	})
	t.Run("no grant keeps nothing", func(t *testing.T) {
		r := atStart(t, func(r *macWindowRig) { r.st.sc = types.SiteConfig{} }, false)
		if v := r.h.srv.macWindowViewFor(context.Background(), r.h.run, r.auth()); v == nil || v.LastClose != nil {
			t.Fatalf("view = %+v, want no last_close", v)
		}
	})
	t.Run("a run that can't be read keeps nothing", func(t *testing.T) {
		r := atStart(t, func(r *macWindowRig) { r.st.failRunRead(errors.New("down")) }, true)
		if len(r.st.prefs) != 1 {
			t.Fatalf("prefs = %v, want only the pending grant", r.st.prefs)
		}
	})
}

// desktop_window on a create: only "at_start", and only on a macOS VM run.
func TestDesktopWindowRequestRefusal(t *testing.T) {
	for _, tc := range []struct {
		window, kind string
		refused      bool
	}{
		{"", "", false}, {"", "macos_vm", false}, {"at_start", "macos_vm", false},
		{"at_start", "", true}, {"at_start", "linux_vm", true}, {"now", "macos_vm", true},
	} {
		got := desktopWindowRequestRefusal(createRunRequest{DesktopWindow: tc.window, MachineKind: tc.kind})
		if (got != "") != tc.refused {
			t.Errorf("desktop_window %q on %q: refusal %q, want refused=%v", tc.window, tc.kind, got, tc.refused)
		}
	}
}

// A create with desktop_window at_start goes through the same rows 1, 4, 5 and 6 and is refused with the sentence
// when one fails, writing a denied row; it is never dropped. A create that passes them leaves the rest to the
// launch.
func TestMacWindowCreateRefusesAtStart(t *testing.T) {
	step := func(t *testing.T, mutate func(r *macWindowRig, f *runFold), mode foldMode) (*httptest.ResponseRecorder, bool, *macWindowRig) {
		t.Helper()
		r := newMacWindowRig(t)
		req := httptest.NewRequest(http.MethodPost, "/runs", nil)
		req = req.WithContext(withHumanIdentity(req.Context(), r.h.owner, r.h.owner+"@example.com", oidc.RoleUser, types.UserTypeStandard, nil, false))
		rec := httptest.NewRecorder()
		f := &runFold{mode: mode, w: rec, r: req, ceiling: governanceCeiling{},
			req: &createRunRequest{Agent: "claude-code", MachineKind: "macos_vm", DesktopWindow: "at_start"}}
		mutate(r, f)
		return rec, r.h.srv.stepMacWindow(f, nil), r
	}
	t.Run("a request without the field is untouched", func(t *testing.T) {
		rec, ok, r := step(t, func(r *macWindowRig, f *runFold) { f.req.DesktopWindow = ""; r.st.sc = types.SiteConfig{} }, foldCreate)
		if !ok || rec.Body.Len() != 0 || len(r.rows("takeover.begin")) != 0 {
			t.Fatalf("ok=%v body=%q", ok, rec.Body.String())
		}
	})
	t.Run("row 1: the switch off refuses the create with its sentence", func(t *testing.T) {
		rec, ok, r := step(t, func(r *macWindowRig, f *runFold) { r.st.sc = types.SiteConfig{} }, foldCreate)
		var body errorBody
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if ok || rec.Code != http.StatusForbidden || body.Reason != reasonMacWindowSiteOff || body.Error != macWindowSiteOffMsg {
			t.Fatalf("ok=%v %d %s", ok, rec.Code, rec.Body.String())
		}
		b := r.rows("takeover.begin")
		if len(b) != 1 || b[0].Outcome != "denied" || b[0].RunID != nil || rowData(t, b[0])["reason"] != reasonMacWindowSiteOff {
			t.Fatalf("begin rows = %+v, want one denied row naming no run", b)
		}
	})
	t.Run("a failed read is a 500 audited as a failure, not a denial", func(t *testing.T) {
		rec, ok, r := step(t, func(r *macWindowRig, f *runFold) { r.st.err = errors.New("store down") }, foldCreate)
		b := r.rows("takeover.begin")
		if ok || rec.Code != http.StatusInternalServerError || len(b) != 1 || b[0].Outcome != "failure" {
			t.Fatalf("ok=%v %d %s rows=%+v, want a 500 and one failure row", ok, rec.Code, rec.Body.String(), b)
		}
	})
	t.Run("row 4: a ceiling that denies interactive desktops refuses the create", func(t *testing.T) {
		rec, ok, _ := step(t, func(r *macWindowRig, f *runFold) {
			f.ceiling = governanceCeiling{Limits: types.GovernanceLimits{DenyInteractive: true}}
		}, foldCreate)
		if ok || rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), macWindowProfileMsg) || !strings.Contains(rec.Body.String(), string(authz.ReasonGovernanceUnit)) {
			t.Fatalf("ok=%v %d %s", ok, rec.Code, rec.Body.String())
		}
	})
	t.Run("row 5: a policy that records Screen and actions refuses the create", func(t *testing.T) {
		rec, ok, _ := step(t, func(r *macWindowRig, f *runFold) { f.spec.Recording = types.RecordingScreenActions }, foldCreate)
		if ok || rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), macWindowUnrecordedMsg) {
			t.Fatalf("ok=%v %d %s", ok, rec.Code, rec.Body.String())
		}
	})
	t.Run("row 6: an agent whose desktop sign-ins are off refuses the create", func(t *testing.T) {
		rec, ok, _ := step(t, func(r *macWindowRig, f *runFold) {
			on := true
			r.st.sc = types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowAttendedWindow: &on},
				AgentProviders: &types.AgentProviders{Agents: []types.AgentProvider{{ID: "claude-code", DesktopSignInsOff: true}}}}
		}, foldCreate)
		if ok || rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), macOSDesktopSignInsOffMsg("claude-code")) {
			t.Fatalf("ok=%v %d %s", ok, rec.Code, rec.Body.String())
		}
	})
	t.Run("Review refuses with the sentence and writes no row", func(t *testing.T) {
		rec, ok, r := step(t, func(r *macWindowRig, f *runFold) { r.st.sc = types.SiteConfig{} }, foldPreview)
		if ok || rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), macWindowSiteOffMsg) || len(r.rows("takeover.begin")) != 0 {
			t.Fatalf("ok=%v %d %s rows=%d", ok, rec.Code, rec.Body.String(), len(r.rows("takeover.begin")))
		}
	})
	t.Run("every refusal names the desktop_window field, the governance decision too", func(t *testing.T) {
		on := true
		for name, mutate := range map[string]func(r *macWindowRig, f *runFold){
			"site off": func(r *macWindowRig, _ *runFold) { r.st.sc = types.SiteConfig{} },
			"governance": func(_ *macWindowRig, f *runFold) {
				f.ceiling = governanceCeiling{Limits: types.GovernanceLimits{DenyUIApps: true}}
			},
			"operator":   func(_ *macWindowRig, f *runFold) { f.ceiling = governanceCeiling{Operator: true} },
			"unrecorded": func(_ *macWindowRig, f *runFold) { f.spec.Recording = types.RecordingScreenActions },
			"sign-ins off": func(r *macWindowRig, _ *runFold) {
				r.st.sc = types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowAttendedWindow: &on},
					AgentProviders: &types.AgentProviders{Agents: []types.AgentProvider{{ID: "claude-code", DesktopSignInsOff: true}}}}
			},
		} {
			for _, mode := range []foldMode{foldCreate, foldPreview} {
				rec, ok, _ := step(t, mutate, mode)
				var body errorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || ok || body.Field != fieldDesktopWindow || body.Error == "" {
					t.Errorf("%s (mode %v): ok=%v %d %s, want a refusal naming field %s", name, mode, ok, rec.Code, rec.Body.String(), fieldDesktopWindow)
				}
			}
		}
	})
	t.Run("the field rides the wire only: the governance audit row is unchanged", func(t *testing.T) {
		_, _, r := step(t, func(_ *macWindowRig, f *runFold) {
			f.ceiling = governanceCeiling{Limits: types.GovernanceLimits{DenyInteractive: true}}
		}, foldCreate)
		for _, ev := range r.rows("authz.denied") {
			if _, has := rowData(t, ev)["field"]; has {
				t.Errorf("authz.denied row carries field: %v", rowData(t, ev))
			}
		}
	})
	t.Run("rows 1, 4, 5 and 6 passing, the create goes on", func(t *testing.T) {
		rec, ok, _ := step(t, func(*macWindowRig, *runFold) {}, foldCreate)
		if !ok || rec.Body.Len() != 0 {
			t.Fatalf("ok=%v %d %s", ok, rec.Code, rec.Body.String())
		}
	})
}

// macos_vm.allow_attended_window rides a site-config save as the other macos_vm fields do: an older client that
// doesn't name it can neither turn a stored true off nor a stored false on, and only an explicit value changes it.
func TestCarryForwardAttendedWindow(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name     string
		existing *types.SiteMacOSVM
		body     *types.SiteMacOSVM
		present  bool
		want     *bool
	}{
		{"stored true, body leaves the block out", &types.SiteMacOSVM{AllowAttendedWindow: &on}, nil, false, &on},
		{"stored true, body names only the days", &types.SiteMacOSVM{AllowAttendedWindow: &on}, &types.SiteMacOSVM{MaxBaseAgeDays: 7}, true, &on},
		{"stored true, null block", &types.SiteMacOSVM{AllowAttendedWindow: &on}, nil, true, &on},
		{"stored true, explicit false turns it off", &types.SiteMacOSVM{AllowAttendedWindow: &on}, &types.SiteMacOSVM{AllowAttendedWindow: &off}, true, &off},
		{"stored false, body names only the days", &types.SiteMacOSVM{AllowAttendedWindow: &off}, &types.SiteMacOSVM{MaxBaseAgeDays: 7}, true, &off},
		{"stored false, explicit true turns it on", &types.SiteMacOSVM{AllowAttendedWindow: &off}, &types.SiteMacOSVM{AllowAttendedWindow: &on}, true, &on},
		{"fresh install, body names only the days: still off", nil, &types.SiteMacOSVM{MaxBaseAgeDays: 7}, true, nil},
		{"nothing stored, null block: still off", &types.SiteMacOSVM{MaxBaseAgeDays: 3}, nil, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := types.SiteConfig{MacOSVM: tc.body}
			carryForwardSiteMacOSVM(&cfg, types.SiteConfig{MacOSVM: tc.existing}, map[string]bool{"macos_vm": tc.present})
			var got *bool
			if cfg.MacOSVM != nil {
				got = cfg.MacOSVM.AllowAttendedWindow
			}
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("switch = %v, want %v", got, tc.want)
			}
			if cfg.MacOSVM.AttendedWindowAllowed() != (tc.want != nil && *tc.want) {
				t.Fatal("the read does not follow the stored value")
			}
		})
	}
	t.Run("the watch's switch is carried beside it", func(t *testing.T) {
		cfg := types.SiteConfig{MacOSVM: &types.SiteMacOSVM{MaxBaseAgeDays: 7}}
		carryForwardSiteMacOSVM(&cfg, types.SiteConfig{MacOSVM: &types.SiteMacOSVM{AllowBackgroundWatch: &off, AllowAttendedWindow: &on}}, map[string]bool{"macos_vm": true})
		if cfg.MacOSVM.AllowBackgroundWatch == nil || *cfg.MacOSVM.AllowBackgroundWatch || cfg.MacOSVM.AllowAttendedWindow == nil || !*cfg.MacOSVM.AllowAttendedWindow {
			t.Fatalf("carried %+v", cfg.MacOSVM)
		}
	})
}

// The refusal sentences are the design's words, pinned here so a drive-by edit shows in a diff.
func TestMacWindowSentences(t *testing.T) {
	for got, want := range map[string]string{
		macWindowSiteOffMsg:                  "Opening a window onto a macOS VM run is turned off for this install. An admin can turn it on in Settings, under macOS virtual machines, or set macos_vm.allow_attended_window in the site config.",
		macWindowNotYourMacMsg("Studio"):     "Wardyn opens a window only on a Mac whose runner you registered. This run is on Studio, so Wardyn can't tell who is at its keyboard. Watch its desktop from the run page instead.",
		macWindowProfileMsg:                  "Your organisation's governance profile doesn't allow an interactive desktop on this run.",
		macWindowUnrecordedMsg:               "This run's recording level is Screen and actions, and a window on your Mac isn't recorded, so Wardyn won't open one. Watch its desktop from the run page instead.",
		macWindowOfflineMsg:                  "This Mac's runner is offline, so Wardyn can't open a window on it.",
		macWindowRunnerTooOldMsg:             "This Mac's runner can't open a window. Upgrade it to this server's version.",
		macWindowNotHereMsg:                  "Wardyn couldn't reach this Mac's runner from here. Try again in a moment.",
		macWindowPreparingMsg:                "This run is still being prepared; the window opens when preparation finishes.",
		types.MacWindowOpenMsg("Studio"):     "This run's window is already open on Studio.",
		types.MacWindowNotAtMacMsg("Studio"): "The window didn't open: you aren't signed in at Studio's screen, or it is locked. Unlock it and open the window again.",
		macWindowLockUnknownMsg:              "Wardyn can't tell whether this Mac's screen is locked, so the window stays closed.",
	} {
		if got != want {
			t.Errorf("sentence = %q, want %q", got, want)
		}
	}
	for _, msg := range []string{macWindowSiteOffMsg, macWindowNotYourMacMsg("Studio"), macWindowProfileMsg, macWindowUnrecordedMsg,
		macWindowOfflineMsg, macWindowRunnerTooOldMsg, macWindowDesktopControlMsg, macWindowNotHereMsg, macWindowPausedMsg,
		macWindowPreparingMsg, macWindowRevokedMsg, macWindowFailedMsg, types.MacWindowOpenMsg("Studio"), types.MacWindowNotAtMacMsg("Studio"),
		macWindowLockUnknownMsg} {
		if strings.Contains(msg, "`") {
			t.Errorf("sentence %q has a backtick: the console prints it literally", msg)
		}
	}
	if !bytes.Contains([]byte(macWindowDesktopControlMsg), []byte("Close it")) {
		t.Error("the control sentence must say what to do")
	}
}

// tokenAuth is the run's person as an API token admitted them, bound to token id.
func (r *macWindowRig) tokenAuth(id uuid.UUID) macWindowAuthority {
	a := r.auth()
	a.Token, a.TokenID = true, id
	return a
}

// cliAuth is the run's person as a CLI sign-in admitted them, bound to session id (never to its access token).
func (r *macWindowRig) cliAuth(id uuid.UUID) macWindowAuthority {
	a := r.auth()
	a.Token, a.CLISessionID = true, id
	return a
}

// openAs opens a window under a.
func (r *macWindowRig) openAs(a macWindowAuthority) *macWindow {
	r.t.Helper()
	w, rf := r.h.srv.openMacWindow(context.Background(), r.h.run, a, macWindowTriggerCLI)
	if rf != nil {
		r.t.Fatalf("open refused: %+v", rf)
	}
	return w
}

// stays ticks once and fails if the window ended.
func (r *macWindowRig) stays(w *macWindow) {
	r.t.Helper()
	r.tick()
	time.Sleep(20 * time.Millisecond)
	if len(r.rows("takeover.close")) != 0 || r.h.srv.macWindows.open(r.h.run.ID) != w {
		r.t.Fatal("the window ended while its credential was live")
	}
}

// closesRevoked ticks once and expects the window to have closed as revoked.
func (r *macWindowRig) closesRevoked() {
	r.t.Helper()
	r.tick()
	if c := r.closeRow(); c["reason"] != macWindowRevoked {
		r.t.Fatalf("close = %v, want %s", c, macWindowRevoked)
	}
	macWindowEventually(r.t, "the registry to empty", func() bool { return r.h.srv.macWindows.open(r.h.run.ID) == nil })
}

// A window ends with the API token or CLI session that opened it, and an at-start grant opened by one that is
// gone does not open.
func TestMacWindowEndsWithItsCredential(t *testing.T) {
	t.Run("a token-opened window closes after DELETE /tokens/{id}", func(t *testing.T) {
		r := newMacWindowRig(t)
		id := uuid.New()
		r.st.liveToken(id)
		w := r.openAs(r.tokenAuth(id))
		r.stays(w)
		router := chi.NewRouter()
		router.Delete("/tokens/{id}", r.h.srv.handleAdminRevokeAPIToken)
		req := httptest.NewRequest(http.MethodDelete, "/tokens/"+id.String(), nil)
		req = req.WithContext(withHumanIdentity(req.Context(), "root", "root@example.com", oidc.RoleAdmin, types.UserTypeStandard, nil, false))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("the revoke answered %d, want %d", rec.Code, http.StatusNoContent)
		}
		r.closesRevoked()
	})
	t.Run("a CLI-opened window survives a rotation and closes when the session ends", func(t *testing.T) {
		r := newMacWindowRig(t)
		sess := r.st.cliSession(nil)
		oldAccess := uuid.New()
		r.st.liveToken(oldAccess) // the access token the window was asked with
		w := r.openAs(r.cliAuth(sess))
		// A refresh revokes the old access token and mints another; the session lives on.
		if _, err := r.st.RevokeAPIToken(context.Background(), oldAccess, "", time.Now()); err != nil {
			t.Fatal(err)
		}
		r.stays(w)
		r.st.endCLISession(sess)
		r.closesRevoked()
	})
	t.Run("a CLI-opened window closes when its session expires", func(t *testing.T) {
		r := newMacWindowRig(t)
		sess := r.st.cliSession(nil)
		r.openAs(r.cliAuth(sess))
		r.st.mu.Lock()
		row := r.st.cliSessions[sess]
		row.ExpiresAt = time.Now().Add(-time.Minute)
		r.st.cliSessions[sess] = row
		r.st.mu.Unlock()
		r.closesRevoked()
	})
	t.Run("a pending at-start grant from a revoked token does not open", func(t *testing.T) {
		r := newMacWindowRig(t)
		id := uuid.New()
		r.st.liveToken(id)
		if err := r.h.srv.macWindowStorePending(context.Background(), r.h.run.ID, r.tokenAuth(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.st.RevokeAPIToken(context.Background(), id, "", time.Now()); err != nil {
			t.Fatal(err)
		}
		r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
		b := r.rows("takeover.begin")
		if r.sub.openCount() != 0 || len(b) != 1 || b[0].Outcome != "denied" || rowData(t, b[0])["reason"] != reasonMacWindowRevoked {
			t.Fatalf("opens %d, begin rows %+v, want none and one denied (%s)", r.sub.openCount(), b, reasonMacWindowRevoked)
		}
	})
	t.Run("a pending grant with no binding is refused", func(t *testing.T) {
		r := newMacWindowRig(t)
		a := r.auth()
		raw, _ := json.Marshal(map[string]any{"actor_type": a.ActorType, "principal": a.Principal, "email": a.Email,
			"authorized_at": a.Issue.At, "token": true})
		r.st.prefs[a.Principal+"\x00"+macWindowPendingKey+r.h.run.ID.String()] = raw
		if gone, err := r.h.srv.macWindowRevoked(context.Background(), macWindowAuthority{Token: true}); gone || !errors.Is(err, errMacWindowNoBinding) {
			t.Fatalf("revoked = %v, err = %v, want the unbound refusal", gone, err)
		}
		r.h.srv.macWindowAtStart(context.Background(), r.h.run.ID)
		b := r.rows("takeover.begin")
		if r.sub.openCount() != 0 || len(b) != 1 || b[0].Outcome != "denied" {
			t.Fatalf("opens %d, begin rows %+v, want none and one denied", r.sub.openCount(), b)
		}
	})
	t.Run("an open with no binding is refused with the revocation-unavailable reason", func(t *testing.T) {
		r := newMacWindowRig(t)
		a := r.auth()
		a.Token = true
		w, rf := r.h.srv.openMacWindow(context.Background(), r.h.run, a, macWindowTriggerCLI)
		if w != nil || rf == nil {
			t.Fatalf("window = %v, refusal = %+v, want a refusal", w, rf)
		}
		if rf.err != nil || rf.status != http.StatusForbidden || rf.body.Reason != uiDeniedReasonRevocationUnavailable ||
			rf.body.Error != macWindowRevocationUnavailableMsg {
			t.Fatalf("refusal = %+v, want 403 %s %q", rf, uiDeniedReasonRevocationUnavailable, macWindowRevocationUnavailableMsg)
		}
		b := r.rows("takeover.begin")
		if r.sub.openCount() != 0 || len(b) != 1 || b[0].Outcome != "denied" ||
			rowData(t, b[0])["reason"] != uiDeniedReasonRevocationUnavailable {
			t.Fatalf("opens %d, begin rows %+v, want none and one denied (%s)", r.sub.openCount(), b, uiDeniedReasonRevocationUnavailable)
		}
		if r.h.srv.macWindows.open(r.h.run.ID) != nil {
			t.Fatal("a window was registered")
		}
	})
	t.Run("the person-level revoke still closes a token-bound window", func(t *testing.T) {
		r := newMacWindowRig(t)
		id := uuid.New()
		r.st.liveToken(id)
		w := r.openAs(r.tokenAuth(id))
		r.stays(w)
		r.rev.set(true, nil)
		r.closesRevoked()
	})
	t.Run("the request's CLI session wins over its access token", func(t *testing.T) {
		tok, sess := uuid.New(), uuid.New()
		ctx := withTokenKind(withCLISessionID(withAPITokenID(context.Background(), tok), sess), types.APITokenKindCLIAccess)
		if a := macWindowAuthorityFrom(ctx, types.ActorHuman, "alice", time.Now()); !a.Token || a.CLISessionID != sess || a.TokenID != uuid.Nil {
			t.Fatalf("authority = %+v, want bound to session %s alone", a, sess)
		}
		if a := macWindowAuthorityFrom(withAPITokenID(context.Background(), tok), types.ActorHuman, "alice", time.Now()); !a.Token || a.TokenID != tok || a.CLISessionID != uuid.Nil {
			t.Fatalf("authority = %+v, want bound to token %s", a, tok)
		}
	})
}
