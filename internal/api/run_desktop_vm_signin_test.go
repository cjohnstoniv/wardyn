// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// vmSignInTestStore is the UI gateway's memory store plus VM sign-in rows and a site config.
type vmSignInTestStore struct {
	*uiMemStore
	vmu  sync.Mutex
	rows map[uuid.UUID]store.DesktopVMSignIns
	sc   types.SiteConfig
	// markErr fails PauseDesktopVMAgent; readErr fails DesktopVMSignIns.
	markErr error
	readErr error
	// onRead, when set, runs as each DesktopVMSignIns begins, with the read's context, and what it
	// returns runs once that read has taken its answer, before the answer is returned: writes landing
	// just before and just after.
	onRead func(ctx context.Context) (after func())
}

func (s *vmSignInTestStore) setReadErr(err error) {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	s.readErr = err
}

func (s *vmSignInTestStore) setOnRead(f func(context.Context) func()) {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	s.onRead = f
}

var _ store.DesktopVMSignInStore = (*vmSignInTestStore)(nil)

func (s *vmSignInTestStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.sc, nil
}

func (s *vmSignInTestStore) RecordDesktopVMSignIns(_ context.Context, runID uuid.UUID, st string) error {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	s.rows[runID] = store.DesktopVMSignIns{Store: st}
	return nil
}

func (s *vmSignInTestStore) DesktopVMSignIns(ctx context.Context, runID uuid.UUID) (store.DesktopVMSignIns, bool, error) {
	s.vmu.Lock()
	onRead := s.onRead
	s.vmu.Unlock()
	var after func()
	if onRead != nil {
		after = onRead(ctx)
	}
	s.vmu.Lock()
	row, ok := s.rows[runID]
	err := s.readErr
	s.vmu.Unlock()
	if after != nil {
		after()
	}
	if err != nil {
		return store.DesktopVMSignIns{}, false, err
	}
	return row, ok, nil
}

func (s *vmSignInTestStore) PauseDesktopVMAgent(_ context.Context, runID uuid.UUID, by string, at time.Time) (bool, error) {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	if s.markErr != nil {
		return false, s.markErr
	}
	row, ok := s.rows[runID]
	if !ok || row.Store != "memory" || row.PausedAt != nil {
		return false, nil
	}
	row.PausedBy, row.PausedAt = by, &at
	s.rows[runID] = row
	return true, nil
}

func (s *vmSignInTestStore) ResumeDesktopVMAgent(_ context.Context, runID uuid.UUID, pausedAt time.Time) (string, time.Time, bool, error) {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	row, ok := s.rows[runID]
	if !ok || row.PausedAt == nil || !row.PausedAt.Equal(pausedAt) {
		return "", time.Time{}, false, nil
	}
	by, at := row.PausedBy, *row.PausedAt
	row.PausedBy, row.PausedAt = "", nil
	s.rows[runID] = row
	return by, at, true, nil
}

// pauseFakeRunner is the guest agent behind runner.AgentPauser: it records each call and whether the
// agent is stopped. A pause's stop lands even when it answers pauseErr, or the call's context has
// ended, as one whose answer was lost.
type pauseFakeRunner struct {
	*sshFakeRunner
	pmu      sync.Mutex
	calls    []string
	pauseErr error
	// resumeErr is what each ResumeAgent answers once its continue has landed, as a lost answer.
	resumeErr error
	stopped   bool
	// hold, when set, keeps each PauseAgent inside the call until it is closed, after telling inPause.
	inPause chan struct{}
	hold    chan struct{}
	// during, when set, runs inside each call ("pause" or "resume"): before a pause's stop lands, after
	// a resume's continue has.
	during func(ctx context.Context, call string)
}

var _ runner.AgentPauser = (*pauseFakeRunner)(nil)

func (f *pauseFakeRunner) PauseAgent(ctx context.Context, ref string) error {
	f.pmu.Lock()
	f.calls = append(f.calls, "pause "+ref)
	f.pmu.Unlock()
	if f.hold != nil {
		f.inPause <- struct{}{}
		<-f.hold
	}
	if f.during != nil {
		f.during(ctx, "pause")
	}
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.stopped = true
	if f.pauseErr != nil {
		return f.pauseErr
	}
	return ctx.Err()
}

func (f *pauseFakeRunner) ResumeAgent(ctx context.Context, ref string) error {
	f.pmu.Lock()
	f.calls = append(f.calls, "resume "+ref)
	f.stopped = false
	f.pmu.Unlock()
	if f.during != nil {
		f.during(ctx, "resume")
	}
	f.pmu.Lock()
	defer f.pmu.Unlock()
	return f.resumeErr
}

func (f *pauseFakeRunner) seen() []string {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	return slices.Clone(f.calls)
}

func (f *pauseFakeRunner) agentStopped() bool {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	return f.stopped
}

// runLockWaits tells waits as each take of a run's operation lock begins, so a test knows a request
// has reached the lock another holds.
type runLockWaits struct {
	db.Locker
	waits chan struct{}
}

func (l runLockWaits) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	if k.Class == db.RunOpLockClass {
		l.waits <- struct{}{}
	}
	return l.Locker.Lock(ctx, k, wait)
}

// droppingLocker is the in-process locker with a run lock a test can lose, as a Postgres session's lock
// is lost with its connection: drop releases the first hold of a run's lock taken through it, so
// another request takes the lock, ends that hold's context with db.ErrLockLost, and from then on
// lockHeld answers ErrLockLost for it. refuse makes every later take of a run's lock refuse at once.
type droppingLocker struct {
	db.Locker
	mu     sync.Mutex
	drop   func()
	refuse atomic.Bool
}

type lostHoldKey struct{}

func (l *droppingLocker) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	if k.Class == db.RunOpLockClass && l.refuse.Load() {
		return nil, nil, db.ErrLockBusy
	}
	lctx, unlock, err := l.Locker.Lock(ctx, k, wait)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil || k.Class != db.RunOpLockClass || l.drop != nil {
		return lctx, unlock, err
	}
	lost := new(atomic.Bool)
	cctx, cancel := context.WithCancelCause(context.WithValue(lctx, lostHoldKey{}, lost))
	release := sync.OnceFunc(unlock)
	l.drop = func() { lost.Store(true); release(); cancel(db.ErrLockLost) }
	return cctx, release, nil
}

// loseLocks gives f's server a droppingLocker: drop loses the next run lock a request takes, and with
// refuse, no request takes it again.
func (f *vmSignInFixture) loseLocks() (drop func(refuse bool)) {
	l := &droppingLocker{Locker: db.NewLocalLocker()}
	f.srv.locks.override = l
	f.srv.locks.held = func(ctx context.Context) error {
		if lost, _ := ctx.Value(lostHoldKey{}).(*atomic.Bool); lost != nil && lost.Load() {
			return db.ErrLockLost
		}
		return db.LockHeld(ctx)
	}
	return func(refuse bool) {
		l.refuse.Store(refuse)
		l.mu.Lock()
		d := l.drop
		l.mu.Unlock()
		d()
	}
}

type vmSignInFixture struct {
	srv    *Server
	h      *harness
	st     *vmSignInTestStore
	rn     *pauseFakeRunner
	run    types.AgentRun
	cookie *http.Cookie
	// lockWaits is told of each request that reaches the run's lock, once pauseInFlight is called.
	lockWaits chan struct{}
}

func newVMSignInFixture(t *testing.T, kept string) *vmSignInFixture {
	t.Helper()
	h := newHarness(t)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: signinOwner, Agent: "claude-code", State: types.RunRunning, SandboxRef: "vm-1",
		MachineKind: types.MachineLinuxVM}
	st := &vmSignInTestStore{uiMemStore: newUIMemStore(), rows: map[uuid.UUID]store.DesktopVMSignIns{run.ID: {Store: kept}}}
	st.putRun(run)
	rn := &pauseFakeRunner{sshFakeRunner: &sshFakeRunner{}}
	f := &vmSignInFixture{h: h, st: st, rn: rn, run: run, cookie: ssoSession(t, signinOwner, "owner@corp.example", oidc.RoleUser)}
	f.restart()
	return f
}

// restart is a new wardynd over the same database and runner: nothing the old one held in memory, its
// locks included, carries over.
func (f *vmSignInFixture) restart() {
	cfg := baseTestConfig(f.h, f.st)
	cfg.Runner, cfg.OIDC = f.rn, &oidc.Authenticator{}
	f.srv = New(cfg)
}

func (f *vmSignInFixture) do(t *testing.T, method, path string) (int, desktopSignInsView, string) {
	t.Helper()
	w := doSSO(t, f.srv, method, "/api/v1/runs/"+f.run.ID.String()+"/desktop/sign-ins"+path, f.cookie, "")
	var v desktopSignInsView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return w.Code, v, w.Body.String()
}

type vmSignInAnswer struct {
	code int
	v    desktopSignInsView
	body string
}

// goDo is do on a goroutine of its own: its answer arrives on the channel.
func (f *vmSignInFixture) goDo(t *testing.T, method, path string) <-chan vmSignInAnswer {
	ch := make(chan vmSignInAnswer, 1)
	go func() {
		code, v, body := f.do(t, method, path)
		ch <- vmSignInAnswer{code, v, body}
	}()
	return ch
}

// pauseInFlight starts a pause and returns once the runner holds it inside PauseAgent, with the run's
// lock taken: release lets the runner answer, and the pause's answer arrives on the channel.
func (f *vmSignInFixture) pauseInFlight(t *testing.T) (release func(), answer <-chan vmSignInAnswer) {
	t.Helper()
	f.lockWaits = make(chan struct{}, 4)
	f.srv.locks.override = runLockWaits{Locker: f.srv.locker(), waits: f.lockWaits}
	f.rn.inPause, f.rn.hold = make(chan struct{}), make(chan struct{})
	answer = f.goDo(t, http.MethodPost, "/pause")
	recvSlotEvent(t, f.lockWaits, "the pause taking the run's lock")
	recvSlotEvent(t, f.rn.inPause, "the pause reaching the runner")
	release = sync.OnceFunc(func() { close(f.rn.hold) })
	t.Cleanup(release)
	return release, answer
}

func (f *vmSignInFixture) recordDesktop(t *testing.T) {
	t.Helper()
	f.srv.cfg.RecordingStore = fsRecordingStore(t)
	f.run.RecordingLevel = types.RecordingScreenActions
	f.st.putRun(f.run)
}

func (f *vmSignInFixture) signInRows() []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range f.h.audit.snapshot() {
		if ev.Action == auditDesktopSignIn {
			out = append(out, ev)
		}
	}
	return out
}

// The policy knob's default is Allowed: no roster, a row without the field, and the zero value all
// allow sign-ins, and the field is absent from a row that never set it. An unreadable roster keeps
// none.
func TestDesktopSignIns_DefaultIsAllowed(t *testing.T) {
	var zero types.AgentProvider
	if zero.DesktopSignInsOff {
		t.Fatal("the zero agent row is Not allowed")
	}
	b, _ := json.Marshal(types.AgentProvider{ID: "claude-code"})
	if strings.Contains(string(b), "desktop_sign_ins_off") {
		t.Fatalf("an untouched row carries the knob: %s", b)
	}
	roster := types.SiteConfig{AgentProviders: &types.AgentProviders{Agents: []types.AgentProvider{{ID: "claude-code"}}}}
	for name, sc := range map[string]types.SiteConfig{"no roster": {}, "a row without the field": roster} {
		if desktopSignInsOff(sc, nil, "claude-code") {
			t.Errorf("%s: sign-ins are off", name)
		}
	}
	if !desktopSignInsOff(types.SiteConfig{}, errors.New("pool closed"), "claude-code") {
		t.Error("an unreadable roster allowed sign-ins")
	}
	f := newVMSignInFixture(t, "memory")
	code, v, body := f.do(t, http.MethodGet, "")
	if code != http.StatusOK || v.Store != "memory" || !v.CanPause || v.Paused || v.Agent != "Claude Code" {
		t.Fatalf("GET = %d %s, want memory, can pause, agent Claude Code", code, body)
	}
}

// "Pause the agent to sign in" stops the agent through the runner's guest agent (never the run's
// freeze), and the resume continues it and writes the session's one audit row.
func TestDesktopSignIns_PauseStopsTheAgentAndResumeAuditsTheSession(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	if code, v, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusOK || !v.Paused || v.PausedAt == nil {
		t.Fatalf("pause = %d %s", code, body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1"}) {
		t.Fatalf("runner calls after pause = %v", got)
	}
	if _, v, _ := f.do(t, http.MethodGet, ""); !v.Paused {
		t.Fatal("GET after pause does not say paused")
	}
	if len(f.signInRows()) != 0 {
		t.Fatal("the pause wrote the session's row before the resume")
	}
	if code, v, body := f.do(t, http.MethodPost, "/resume"); code != http.StatusOK || v.Paused {
		t.Fatalf("resume = %d %s", code, body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1", "resume vm-1"}) {
		t.Fatalf("runner calls after resume = %v", got)
	}
	rows := f.signInRows()
	if len(rows) != 1 || rows[0].Outcome != "success" || rows[0].Actor != signinOwner || rows[0].Target != f.run.ID.String() {
		t.Fatalf("desktop.sign_in.pause rows = %+v, want one success by the owner", rows)
	}
	var d map[string]any
	if err := json.Unmarshal(rows[0].Data, &d); err != nil || d["paused_by"] != signinOwner || d["paused_at"] == nil || d["resumed_at"] == nil {
		t.Fatalf("row data = %s, %v", rows[0].Data, err)
	}
	if code, _, _ := f.do(t, http.MethodPost, "/resume"); code != http.StatusOK || len(f.signInRows()) != 1 || len(f.rn.seen()) != 2 {
		t.Fatalf("a second resume = %d, rows %d, calls %v: want no call and no second row", code, len(f.signInRows()), f.rn.seen())
	}
}

// On a run whose desktop screen is recorded, the pause holds the recording paused while the person
// signs in, so no frame of what they type is kept, and the resume releases it.
func TestDesktopSignIns_PauseHoldsTheDesktopRecordingUntilTheResume(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	f.recordDesktop(t)
	if code, _, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusOK {
		t.Fatalf("pause = %d %s", code, body)
	}
	if !signInHeld(f.srv, f.run.ID) {
		t.Fatal("the desktop recording is not held paused while the sign-in is open")
	}
	if p, r := desktopRows(f.h.audit); p != 1 || r != 0 {
		t.Fatalf("after the pause: %d desktop.recording.pause and %d .resume rows, want 1 and 0", p, r)
	}
	if code, _, body := f.do(t, http.MethodPost, "/resume"); code != http.StatusOK {
		t.Fatalf("resume = %d %s", code, body)
	}
	if signInHeld(f.srv, f.run.ID) {
		t.Fatal("the resume left the desktop recording paused")
	}
	if p, r := desktopRows(f.h.audit); p != 1 || r != 1 {
		t.Fatalf("after the resume: %d desktop.recording.pause and %d .resume rows, want 1 and 1", p, r)
	}
}

// Not allowed is refused at the server, not only hidden: on a run whose desktop keeps none, and on
// one the admin turned off after it started. Nothing reaches the runner.
func TestDesktopSignIns_OffIsRefusedServerSide(t *testing.T) {
	t.Run("store none", func(t *testing.T) {
		f := newVMSignInFixture(t, "none")
		if _, v, _ := f.do(t, http.MethodGet, ""); v.CanPause || v.Store != "none" {
			t.Fatalf("GET = %+v, want none and no pause", v)
		}
		if code, _, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusForbidden || !strings.Contains(body, reasonDesktopSignInsOff) {
			t.Fatalf("pause = %d %s, want 403 %s", code, body, reasonDesktopSignInsOff)
		}
		if len(f.rn.seen()) != 0 {
			t.Fatalf("the refused pause reached the runner: %v", f.rn.seen())
		}
	})
	t.Run("admin set Not allowed", func(t *testing.T) {
		f := newVMSignInFixture(t, "memory")
		f.st.sc = types.SiteConfig{AgentProviders: &types.AgentProviders{Agents: []types.AgentProvider{{ID: "claude-code", DesktopSignInsOff: true}}}}
		if _, v, _ := f.do(t, http.MethodGet, ""); v.CanPause {
			t.Fatal("GET offers the pause on an agent set Not allowed")
		}
		if code, _, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusForbidden || !strings.Contains(body, reasonDesktopSignInsOff) {
			t.Fatalf("pause = %d %s, want 403 %s", code, body, reasonDesktopSignInsOff)
		}
		if len(f.rn.seen()) != 0 {
			t.Fatalf("the refused pause reached the runner: %v", f.rn.seen())
		}
	})
}

// A pause the guest agent could not make leaves the agent running and unmarked, and is audited.
func TestDesktopSignIns_FailedPauseResumesAndLeavesNoMark(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	f.rn.pauseErr = errors.New("guestagent: 404 Not Found")
	if code, _, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusBadGateway || !strings.Contains(body, reasonDesktopSignInsPauseFailed) {
		t.Fatalf("pause = %d %s", code, body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1", "resume vm-1"}) {
		t.Fatalf("runner calls = %v, want the pause then a resume", got)
	}
	if _, v, _ := f.do(t, http.MethodGet, ""); v.Paused {
		t.Fatal("a failed pause left the agent marked paused")
	}
	if rows := f.signInRows(); len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("rows = %+v, want one failure", rows)
	}
}

// While the runner has not answered a pause, the run is not paused: the GET says so and the desktop
// recording is not held. Both change only once the runner has stopped the agent.
func TestDesktopSignIns_GETSaysPausedOnlyOnceTheRunnerAnswers(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	f.recordDesktop(t)
	release, paused := f.pauseInFlight(t)
	if code, v, body := f.do(t, http.MethodGet, ""); code != http.StatusOK || v.Paused || v.PausedAt != nil {
		t.Fatalf("GET while the runner has not answered the pause = %d %s, want not paused", code, body)
	}
	if signInHeld(f.srv, f.run.ID) {
		t.Fatal("the desktop recording was held before the runner stopped the agent")
	}
	release()
	if a := recvSlotEvent(t, paused, "the pause's answer"); a.code != http.StatusOK || !a.v.Paused {
		t.Fatalf("pause = %d %s", a.code, a.body)
	}
	if _, v, _ := f.do(t, http.MethodGet, ""); !v.Paused {
		t.Fatal("GET after the runner's answer does not say paused")
	}
	if !signInHeld(f.srv, f.run.ID) {
		t.Fatal("the desktop recording is not held once the agent is paused")
	}
}

// A second pause while the first's runner call is open waits for that call, then answers paused
// without a second call: it never says paused before the runner has.
func TestDesktopSignIns_SecondPauseWaitsForTheFirst(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	release, first := f.pauseInFlight(t)
	second := f.goDo(t, http.MethodPost, "/pause")
	recvSlotEvent(t, f.lockWaits, "the second pause reaching the run's lock")
	select {
	case a := <-second:
		t.Fatalf("the second pause answered %d %s while the first's runner call was open", a.code, a.body)
	default:
	}
	release()
	for name, ch := range map[string]<-chan vmSignInAnswer{"first": first, "second": second} {
		if a := recvSlotEvent(t, ch, "the "+name+" pause's answer"); a.code != http.StatusOK || !a.v.Paused {
			t.Fatalf("%s pause = %d %s", name, a.code, a.body)
		}
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1"}) {
		t.Fatalf("runner calls = %v, want one pause", got)
	}
}

// A resume from another tab while a pause's runner call is open waits for it and then continues the
// agent that pause stopped: it never overtakes the pause and leaves the agent stopped with no mark.
func TestDesktopSignIns_ResumeWaitsForAnInFlightPause(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	release, paused := f.pauseInFlight(t)
	resumed := f.goDo(t, http.MethodPost, "/resume")
	recvSlotEvent(t, f.lockWaits, "the resume reaching the run's lock")
	release()
	if a := recvSlotEvent(t, paused, "the pause's answer"); a.code != http.StatusOK || !a.v.Paused {
		t.Fatalf("pause = %d %s", a.code, a.body)
	}
	if a := recvSlotEvent(t, resumed, "the resume's answer"); a.code != http.StatusOK || a.v.Paused {
		t.Fatalf("resume = %d %s", a.code, a.body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1", "resume vm-1"}) {
		t.Fatalf("runner calls = %v, want the pause then the resume", got)
	}
	if f.rn.agentStopped() {
		t.Fatal("the agent is left stopped after the resume")
	}
	if _, v, _ := f.do(t, http.MethodGet, ""); v.Paused {
		t.Fatal("GET after the resume says paused")
	}
	if rows := f.signInRows(); len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("rows = %+v, want the session's one success", rows)
	}
}

// A pause whose runner call fails leaves no mark at any point: the GET and a resume queued behind it
// see none, the agent is continued, and no desktop recording hold is left.
func TestDesktopSignIns_FailedPauseInFlightLeavesTheAgentRunning(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	f.recordDesktop(t)
	f.rn.pauseErr = errors.New("guestagent: context deadline exceeded")
	release, paused := f.pauseInFlight(t)
	if _, v, _ := f.do(t, http.MethodGet, ""); v.Paused {
		t.Fatal("GET says paused while the pause's runner call is open")
	}
	resumed := f.goDo(t, http.MethodPost, "/resume")
	recvSlotEvent(t, f.lockWaits, "the resume reaching the run's lock")
	release()
	if a := recvSlotEvent(t, paused, "the pause's answer"); a.code != http.StatusBadGateway || !strings.Contains(a.body, reasonDesktopSignInsPauseFailed) {
		t.Fatalf("pause = %d %s", a.code, a.body)
	}
	if a := recvSlotEvent(t, resumed, "the resume's answer"); a.code != http.StatusOK || a.v.Paused {
		t.Fatalf("resume = %d %s", a.code, a.body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1", "resume vm-1"}) {
		t.Fatalf("runner calls = %v, want the failed pause's own continue and nothing from the resume", got)
	}
	if f.rn.agentStopped() {
		t.Fatal("the failed pause left the agent stopped")
	}
	if _, v, _ := f.do(t, http.MethodGet, ""); v.Paused {
		t.Fatal("a failed pause left the agent marked paused")
	}
	if signInHeld(f.srv, f.run.ID) {
		t.Fatal("a failed pause left the desktop recording held")
	}
	if rows := f.signInRows(); len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("rows = %+v, want one failure", rows)
	}
}

// A pause the runner made but the row could not record is undone: the agent is continued, so nothing
// is left stopped that no resume would continue.
func TestDesktopSignIns_UnrecordedPauseContinuesTheAgent(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	f.recordDesktop(t)
	f.st.markErr = errors.New("store: pool closed")
	if code, _, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusConflict || !strings.Contains(body, reasonDesktopSignInsPauseFailed) {
		t.Fatalf("pause = %d %s", code, body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1", "resume vm-1"}) {
		t.Fatalf("runner calls = %v, want the pause then a continue", got)
	}
	if f.rn.agentStopped() {
		t.Fatal("an unrecorded pause left the agent stopped")
	}
	if signInHeld(f.srv, f.run.ID) {
		t.Fatal("an unrecorded pause left the desktop recording held")
	}
	if rows := f.signInRows(); len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("rows = %+v, want one failure", rows)
	}
}

// A mark that changes nothing means another pause marked the row first. That pause owns the stopped
// agent, so this one does not continue it, and answers with that pause's row.
func TestDesktopSignIns_APauseMarkedFirstElsewhereIsNotContinued(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	other := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	f.rn.during = func(_ context.Context, call string) {
		if call != "pause" {
			return
		}
		// Another pause commits its mark while this one's runner call is open.
		if ok, err := f.st.PauseDesktopVMAgent(context.Background(), f.run.ID, "someone-else", other); err != nil || !ok {
			t.Errorf("the other pause's mark = %v %v", ok, err)
		}
	}
	code, v, body := f.do(t, http.MethodPost, "/pause")
	if code != http.StatusOK || !v.Paused || v.PausedAt == nil || !v.PausedAt.Equal(other) {
		t.Fatalf("pause = %d %s, want the other pause's mark", code, body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1"}) || !f.rn.agentStopped() {
		t.Fatalf("runner calls = %v, want the pause and no continue", got)
	}
	if row, _, _ := f.st.DesktopVMSignIns(context.Background(), f.run.ID); row.PausedAt == nil || !row.PausedAt.Equal(other) || row.PausedBy != "someone-else" {
		t.Fatalf("row = %+v, want the other pause's mark", row)
	}
	rows := f.signInRows()
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want only this pause's failure", rows)
	}
	wantSuperseded(t, rows, other)
}

// The pause is the row, not this process's memory: after a restart the GET still says paused, and
// the resume continues the agent and writes the session's row.
func TestDesktopSignIns_PauseSurvivesARestart(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	if code, _, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusOK {
		t.Fatalf("pause = %d %s", code, body)
	}
	f.restart()
	if _, v, _ := f.do(t, http.MethodGet, ""); !v.Paused || v.PausedAt == nil {
		t.Fatal("after a restart, GET does not say paused")
	}
	if code, v, body := f.do(t, http.MethodPost, "/resume"); code != http.StatusOK || v.Paused {
		t.Fatalf("resume after a restart = %d %s", code, body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1", "resume vm-1"}) {
		t.Fatalf("runner calls = %v, want the pause then the resume", got)
	}
	if f.rn.agentStopped() {
		t.Fatal("the agent is left stopped after the resume")
	}
	if rows := f.signInRows(); len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("rows = %+v, want the session's one success", rows)
	}
}

// A pause that loses the run's lock during its runner call, as one whose database session died does,
// acts on the agent no more: another pause took the lock, stopped the agent and marked the row, and
// that agent is its to continue. The first takes the lock again, finds the newer mark and sends
// nothing, so the agent stays stopped under the mark its resume will clear. Its failure row names the
// pause that superseded it.
func TestDesktopSignIns_PauseThatLostItsLockLeavesANewerPauseAlone(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	drop := f.loseLocks()
	var first atomic.Bool
	var newer vmSignInAnswer
	f.rn.during = func(_ context.Context, call string) {
		if call == "pause" && first.CompareAndSwap(false, true) {
			drop(false)
			newer.code, newer.v, newer.body = f.do(t, http.MethodPost, "/pause")
		}
	}
	code, v, body := f.do(t, http.MethodPost, "/pause")
	if newer.code != http.StatusOK || !newer.v.Paused || newer.v.PausedAt == nil {
		t.Fatalf("the newer pause = %d %s", newer.code, newer.body)
	}
	if code != http.StatusOK || !v.Paused || v.PausedAt == nil || !v.PausedAt.Equal(*newer.v.PausedAt) {
		t.Fatalf("the pause that lost its lock = %d %s, want the newer pause's mark", code, body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1", "pause vm-1"}) {
		t.Fatalf("runner calls = %v, want the two pauses and no continue", got)
	}
	if !f.rn.agentStopped() {
		t.Fatal("the pause that lost its lock continued the newer pause's agent")
	}
	if row, _, _ := f.st.DesktopVMSignIns(context.Background(), f.run.ID); row.PausedAt == nil || !row.PausedAt.Equal(*newer.v.PausedAt) {
		t.Fatalf("row = %+v, want the newer pause's mark", row)
	}
	rows := f.signInRows()
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want only the superseded pause's failure until the resume", rows)
	}
	wantSuperseded(t, rows, *newer.v.PausedAt)
}

// wantSuperseded fails t unless rows hold one failure, stage lock_lost, superseded by the pause at at.
func wantSuperseded(t *testing.T, rows []types.AuditEvent, at time.Time) {
	t.Helper()
	n := 0
	for _, ev := range rows {
		var d struct {
			Stage        string     `json:"stage"`
			SupersededBy *time.Time `json:"superseded_by"`
		}
		if ev.Outcome == "failure" && json.Unmarshal(ev.Data, &d) == nil && d.Stage == "lock_lost" && d.SupersededBy != nil && d.SupersededBy.Equal(at) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("desktop.sign_in.pause rows = %+v, want one lock_lost failure superseded by the pause at %v", rows, at)
	}
}

// A pause that loses the run's lock during its runner call and cannot take it again sends nothing:
// the agent may be left stopped with no mark (docs/operations/linux-vm-sandboxes.md says how to
// recover it), the failure is audited as lock_lost, and the answer is the lock's 503.
func TestDesktopSignIns_PauseThatCannotRetakeItsLockSendsNothing(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	drop := f.loseLocks()
	f.rn.during = func(_ context.Context, call string) {
		if call == "pause" {
			drop(true)
		}
	}
	code, _, body := f.do(t, http.MethodPost, "/pause")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, reasonLockUnavailable) {
		t.Fatalf("pause = %d %s, want 503 %s", code, body, reasonLockUnavailable)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1"}) || !f.rn.agentStopped() {
		t.Fatalf("runner calls = %v, want the pause and no continue", got)
	}
	if row, _, _ := f.st.DesktopVMSignIns(context.Background(), f.run.ID); row.PausedAt != nil {
		t.Fatalf("row = %+v, want no mark", row)
	}
	rows := f.signInRows()
	var d map[string]any
	if len(rows) != 1 || rows[0].Outcome != "failure" || json.Unmarshal(rows[0].Data, &d) != nil || d["stage"] != "lock_lost" {
		t.Fatalf("rows = %+v, want one lock_lost failure", rows)
	}
}

// A failed pause's continue is cancelled when the run's lock is lost during it: another pause may take
// the lock and stop the agent, and a continue landing after that would run the agent under its mark.
// The first pause takes the lock again and finds that newer mark. As its continue was sent, it stops
// the agent once more, under the lock it holds again, and sends nothing else, so the newer pause's
// agent stays stopped under its mark.
func TestDesktopSignIns_AContinueThatLosesItsLockIsCancelled(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	drop := f.loseLocks()
	f.rn.pauseErr = errors.New("guestagent: 502 Bad Gateway")
	var first atomic.Bool
	var cause, restopLock error
	var newer vmSignInAnswer
	newerDone, restops := false, 0
	f.rn.during = func(ctx context.Context, call string) {
		if call == "pause" && newerDone { // the first pause's stop, once the newer pause has answered
			restops++
			restopLock = f.srv.lockHeld(ctx)
			return
		}
		if call != "resume" || !first.CompareAndSwap(false, true) {
			return
		}
		drop(false)
		select {
		case <-ctx.Done():
			cause = context.Cause(ctx)
		case <-time.After(5 * time.Second):
		}
		f.rn.pmu.Lock()
		f.rn.pauseErr = nil
		f.rn.pmu.Unlock()
		newer.code, newer.v, newer.body = f.do(t, http.MethodPost, "/pause")
		newerDone = true
	}
	code, v, body := f.do(t, http.MethodPost, "/pause")
	if !errors.Is(cause, db.ErrLockLost) {
		t.Fatalf("the continue's context ended with %v, want it cancelled with the lost lock", cause)
	}
	if newer.code != http.StatusOK || !newer.v.Paused || newer.v.PausedAt == nil {
		t.Fatalf("the newer pause = %d %s", newer.code, newer.body)
	}
	if code != http.StatusOK || v.PausedAt == nil || !v.PausedAt.Equal(*newer.v.PausedAt) {
		t.Fatalf("the failed pause = %d %s, want the newer pause's mark", code, body)
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1", "resume vm-1", "pause vm-1", "pause vm-1"}) {
		t.Fatalf("runner calls = %v, want the failed pause, its cancelled continue, the newer pause and one stop again", got)
	}
	if restops != 1 || restopLock != nil {
		t.Fatalf("stops after the newer pause = %d, run lock %v: want one, sent under the lock taken again", restops, restopLock)
	}
	if !f.rn.agentStopped() {
		t.Fatal("the newer pause's agent is not stopped")
	}
	if row, _, _ := f.st.DesktopVMSignIns(context.Background(), f.run.ID); row.PausedAt == nil || !row.PausedAt.Equal(*newer.v.PausedAt) {
		t.Fatalf("row = %+v, want the newer pause's mark intact", row)
	}
	rows := f.signInRows()
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want only the failed pause's row", rows)
	}
	wantSuperseded(t, rows, *newer.v.PausedAt)
}

// A resume that loses the run's lock after its runner call clears no mark but its own: the session was
// resumed from another tab and a newer pause has marked the row since, so that mark stays for its own
// resume, and the newer pause's agent stays stopped. Its failure row names the pause that superseded it.
func TestDesktopSignIns_ResumeThatLostItsLockLeavesANewerPauseAlone(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	if code, _, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusOK {
		t.Fatalf("pause = %d %s", code, body)
	}
	drop := f.loseLocks()
	var first atomic.Bool
	var newer vmSignInAnswer
	f.rn.during = func(_ context.Context, call string) {
		if call == "resume" && first.CompareAndSwap(false, true) {
			drop(false)
			if code, _, body := f.do(t, http.MethodPost, "/resume"); code != http.StatusOK {
				t.Errorf("the other tab's resume = %d %s", code, body)
			}
			newer.code, newer.v, newer.body = f.do(t, http.MethodPost, "/pause")
		}
	}
	code, v, body := f.do(t, http.MethodPost, "/resume")
	if newer.code != http.StatusOK || !newer.v.Paused || newer.v.PausedAt == nil {
		t.Fatalf("the newer pause = %d %s", newer.code, newer.body)
	}
	if code != http.StatusOK || !v.Paused || v.PausedAt == nil || !v.PausedAt.Equal(*newer.v.PausedAt) {
		t.Fatalf("the resume that lost its lock = %d %s, want the newer pause's mark", code, body)
	}
	if row, _, _ := f.st.DesktopVMSignIns(context.Background(), f.run.ID); row.PausedAt == nil || !row.PausedAt.Equal(*newer.v.PausedAt) {
		t.Fatalf("row = %+v, want the newer pause's mark", row)
	}
	if !f.rn.agentStopped() {
		t.Fatal("the newer pause's agent is not stopped")
	}
	rows := f.signInRows()
	if len(rows) != 2 || rows[0].Outcome != "success" {
		t.Fatalf("rows = %+v, want the other tab's success and this resume's failure", rows)
	}
	wantSuperseded(t, rows, *newer.v.PausedAt)
}

// A failed pause's continue is cancelled when the run's lock is lost even after the person's tab has
// closed: the lock's context then keeps the tab's cause, so the lock itself is asked. Holding the lock
// again with the row unmarked, the pause sends the continue once more.
func TestDesktopSignIns_AContinueIsCancelledOnALockLostAfterTheTabClosed(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	drop := f.loseLocks()
	f.rn.pauseErr = errors.New("guestagent: 502 Bad Gateway")
	tab, closeTab := context.WithCancel(context.Background())
	defer closeTab()
	var first atomic.Bool
	var cause error
	f.rn.during = func(ctx context.Context, call string) {
		if call != "resume" || !first.CompareAndSwap(false, true) {
			return
		}
		closeTab()
		drop(false)
		select {
		case <-ctx.Done():
			cause = context.Cause(ctx)
		case <-time.After(5 * time.Second):
		}
	}
	w := doSSOCtx(t, f.srv, tab, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/desktop/sign-ins/pause", f.cookie, "")
	if !errors.Is(cause, db.ErrLockLost) {
		t.Fatalf("the continue's context ended with %v, want it cancelled with the lost lock", cause)
	}
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), reasonDesktopSignInsPauseFailed) {
		t.Fatalf("pause = %d %s, want the failed pause's 502", w.Code, w.Body.String())
	}
	if got := f.rn.seen(); !slices.Equal(got, []string{"pause vm-1", "resume vm-1", "resume vm-1"}) || f.rn.agentStopped() {
		t.Fatalf("runner calls = %v, want the failed pause, its cancelled continue and one under the lock taken again", got)
	}
}

// A resume whose continue answers an error, with the run's lock lost during it, is reconciled as one
// that answered: a newer pause has marked the row since, and the continue may have landed after its
// stop. The resume takes the lock again, stops the agent once more under it, and leaves that pause's
// mark.
func TestDesktopSignIns_AFailedResumeThatLostItsLockStopsTheAgentAgain(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	if code, _, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusOK {
		t.Fatalf("pause = %d %s", code, body)
	}
	drop := f.loseLocks()
	var first atomic.Bool
	var restopLock error
	var newer vmSignInAnswer
	newerDone, restops := false, 0
	f.rn.during = func(ctx context.Context, call string) {
		if call == "pause" && newerDone { // the resume's stop, once the newer pause has answered
			restops++
			restopLock = f.srv.lockHeld(ctx)
			return
		}
		if call != "resume" || !first.CompareAndSwap(false, true) {
			return
		}
		drop(false)
		if code, _, body := f.do(t, http.MethodPost, "/resume"); code != http.StatusOK {
			t.Errorf("the other tab's resume = %d %s", code, body)
		}
		newer.code, newer.v, newer.body = f.do(t, http.MethodPost, "/pause")
		newerDone = true
		f.rn.pmu.Lock()
		f.rn.resumeErr = errors.New("guestagent: connection reset by peer") // this resume's answer is lost
		f.rn.pmu.Unlock()
	}
	code, v, body := f.do(t, http.MethodPost, "/resume")
	if newer.code != http.StatusOK || !newer.v.Paused || newer.v.PausedAt == nil {
		t.Fatalf("the newer pause = %d %s", newer.code, newer.body)
	}
	if code != http.StatusOK || v.PausedAt == nil || !v.PausedAt.Equal(*newer.v.PausedAt) {
		t.Fatalf("the failed resume = %d %s, want the newer pause's mark", code, body)
	}
	if restops != 1 || restopLock != nil {
		t.Fatalf("stops after the newer pause = %d, run lock %v: want one, sent under the lock taken again", restops, restopLock)
	}
	if !f.rn.agentStopped() {
		t.Fatal("the newer pause's agent is not stopped")
	}
	if row, _, _ := f.st.DesktopVMSignIns(context.Background(), f.run.ID); row.PausedAt == nil || !row.PausedAt.Equal(*newer.v.PausedAt) {
		t.Fatalf("row = %+v, want the newer pause's mark intact", row)
	}
	rows := f.signInRows()
	if len(rows) != 2 || rows[0].Outcome != "success" {
		t.Fatalf("rows = %+v, want the other tab's success and this resume's failure", rows)
	}
	wantSuperseded(t, rows, *newer.v.PausedAt)
}

// A resume that loses the run's lock after its runner call and cannot take it again clears nothing:
// the mark stays for a resume that can record it, the failure is audited as lock_lost, and the answer
// is 503.
func TestDesktopSignIns_ResumeThatCannotRetakeItsLockLeavesTheMark(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	code, paused, body := f.do(t, http.MethodPost, "/pause")
	if code != http.StatusOK || paused.PausedAt == nil {
		t.Fatalf("pause = %d %s", code, body)
	}
	drop := f.loseLocks()
	f.rn.during = func(_ context.Context, call string) {
		if call == "resume" {
			drop(true)
		}
	}
	code, _, body = f.do(t, http.MethodPost, "/resume")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, reasonDesktopSignInsResumeFailed) {
		t.Fatalf("resume = %d %s, want 503 %s", code, body, reasonDesktopSignInsResumeFailed)
	}
	if row, _, _ := f.st.DesktopVMSignIns(context.Background(), f.run.ID); row.PausedAt == nil || !row.PausedAt.Equal(*paused.PausedAt) {
		t.Fatalf("row = %+v, want the pause's mark intact", row)
	}
	rows := f.signInRows()
	var d map[string]any
	if len(rows) != 1 || rows[0].Outcome != "failure" || json.Unmarshal(rows[0].Data, &d) != nil || d["stage"] != "lock_lost" {
		t.Fatalf("rows = %+v, want one lock_lost failure", rows)
	}
}

// vmSignInDesktopStore is the desktop harness's store with VM sign-in rows; vmSignInDesktopRunner is its
// runner with a guest agent that stops and continues the agent.
type vmSignInDesktopStore struct {
	*desktopClosedStore
	store.DesktopVMSignInStore
}

type vmSignInDesktopRunner struct {
	*desktopFakeRunner
	runner.AgentPauser
}

// vmSignInDesktop is vmDesktopRun with the run's sign-in row and a guest agent that stops it. The open
// stream re-reads the row only when the test sends on recheck.
type vmSignInDesktop struct {
	h       *uiHarness
	dr      *desktopFakeRunner
	send    chan []byte
	st      *vmSignInTestStore
	guest   *pauseFakeRunner
	recheck chan chan struct{}
}

func newVMSignInDesktop(t *testing.T, row store.DesktopVMSignIns) *vmSignInDesktop {
	t.Helper()
	h, dr, send, _ := vmDesktopRun(t)
	d := &vmSignInDesktop{h: h, dr: dr, send: send, recheck: make(chan chan struct{}),
		st:    &vmSignInTestStore{uiMemStore: h.store, rows: map[uuid.UUID]store.DesktopVMSignIns{h.run.ID: row}},
		guest: &pauseFakeRunner{sshFakeRunner: h.runner, stopped: row.PausedAt != nil}}
	h.srv.cfg.Store = vmSignInDesktopStore{desktopClosedStore: h.srv.cfg.Store.(*desktopClosedStore), DesktopVMSignInStore: d.st}
	h.srv.cfg.Runner = vmSignInDesktopRunner{desktopFakeRunner: dr, AgentPauser: d.guest}
	h.srv.vmSignInTick = d.recheck
	return d
}

// signIn answers POST .../desktop/sign-ins/<action> ("pause" or "resume") on srv as the run's owner,
// called past the router: the UI harness serves no SSO routes.
func (d *vmSignInDesktop) signIn(srv *Server, action string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+d.h.run.ID.String()+"/desktop/sign-ins/"+action, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", d.h.run.ID.String())
	w := httptest.NewRecorder()
	answer := srv.handleResumeDesktopAgent
	if action == "pause" {
		answer = srv.handlePauseDesktopAgent
	}
	answer(w, r.WithContext(context.WithValue(withOIDCHuman(r.Context(), d.h.owner), chi.RouteCtxKey, rctx)))
	return w
}

// startReread has the open stream recheck the row once, as its vmSignInRecheck ticker does: done is
// closed once that read has answered, or at once when one was still in flight.
func (d *vmSignInDesktop) startReread(t *testing.T) (done chan struct{}) {
	t.Helper()
	done = make(chan struct{})
	select {
	case d.recheck <- done:
	case <-time.After(5 * time.Second):
		t.Fatal("no desktop stream took the re-read")
	}
	return done
}

// reread is startReread, waiting for its answer.
func (d *vmSignInDesktop) reread(t *testing.T) {
	t.Helper()
	recvSlotEvent(t, d.startReread(t), "the stream's re-read")
}

// signInHeld reports whether srv holds run's desktop recording paused for a sign-in or an unread mark.
func signInHeld(srv *Server, run uuid.UUID) bool {
	return len(srv.desktopRecs.heldWith(run, "vm-sign-in")) > 0
}

// A pause's hold on the desktop recording is one process's memory; the row's mark is not. A server that
// never saw the pause, as after a restart, holds a desktop that connects while the row says paused: what
// the VM sends is not recorded until the resume, and is recorded again after it.
func TestDesktopSignIns_RestartedServerHoldsANewDesktopUntilTheResume(t *testing.T) {
	at := time.Now().UTC()
	d := newVMSignInDesktop(t, store.DesktopVMSignIns{Store: "memory", PausedBy: "someone", PausedAt: &at})
	h := d.h
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := openVMDesktop(t, ctx, h, d.dr)
	if !signInHeld(h.srv, h.run.ID) {
		t.Fatal("a desktop that connected while the row says paused is not held")
	}
	d.send <- vmFBU // sent while the sign-in pause is open: the viewer sees it, the recording does not
	v.read(len(vmFBU))
	if w := d.signIn(h.srv, "resume"); w.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", w.Code, w.Body.String())
	}
	if d.guest.agentStopped() || signInHeld(h.srv, h.run.ID) {
		t.Fatal("the resume left the agent stopped or the desktop recording held")
	}
	d.send <- []byte("\x02\x02") // two Bells, after the resume
	v.read(2)
	endVMDesktop(t, h, v, d.send)

	_, pb, kept := storedVMRecording(t, h)
	if want := vmHandshake() + "\x02\x02"; kept != want {
		t.Errorf("recorded frames = %q, want the handshake and the Bells after the resume %q", kept, want)
	}
	if len(pb.Gaps) != 1 || pb.Gaps[0].Reason != desktopPauseSignin || pb.Gaps[0].EndMS == 0 {
		t.Errorf("playback gaps = %+v, want one closed %q gap", pb.Gaps, desktopPauseSignin)
	}
}

// A pause answered on another replica reaches a desktop already open on this one at the stream's next
// re-read of the row (every vmSignInRecheck), and so does the resume: nothing the VM sends in between
// is recorded, and what it sends before and after is.
func TestDesktopSignIns_APauseOnAnotherReplicaHoldsAnOpenDesktop(t *testing.T) {
	d := newVMSignInDesktop(t, store.DesktopVMSignIns{Store: "memory"})
	h := d.h
	other := New(h.srv.cfg) // another replica over the same database and runner
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := openVMDesktop(t, ctx, h, d.dr)
	d.send <- []byte("\x02") // a Bell before the pause
	v.read(1)
	if w := d.signIn(other, "pause"); w.Code != http.StatusOK {
		t.Fatalf("pause on the other replica = %d %s", w.Code, w.Body.String())
	}
	d.reread(t)
	if !signInHeld(h.srv, h.run.ID) {
		t.Fatal("a pause answered on another replica did not hold this replica's open desktop at its re-read")
	}
	d.send <- vmFBU // sent while the pause is open
	v.read(len(vmFBU))
	if w := d.signIn(other, "resume"); w.Code != http.StatusOK {
		t.Fatalf("resume on the other replica = %d %s", w.Code, w.Body.String())
	}
	d.reread(t)
	if signInHeld(h.srv, h.run.ID) {
		t.Fatal("the resume on another replica left this replica's desktop recording held after its re-read")
	}
	d.send <- []byte("\x02\x02") // two Bells after the resume
	v.read(2)
	endVMDesktop(t, h, v, d.send)

	_, pb, kept := storedVMRecording(t, h)
	if want := vmHandshake() + "\x02" + "\x02\x02"; kept != want {
		t.Errorf("recorded frames = %q, want the handshake and the Bells either side of the pause %q", kept, want)
	}
	if len(pb.Gaps) != 1 || pb.Gaps[0].Reason != desktopPauseSignin || pb.Gaps[0].EndMS == 0 {
		t.Errorf("playback gaps = %+v, want one closed %q gap", pb.Gaps, desktopPauseSignin)
	}
}

// A recorded VM desktop whose sign-in mark cannot be read is refused with 503 before the runner is
// dialled: it is never recorded on a state nobody read.
func TestDesktopSignIns_AnUnreadableMarkRefusesTheDesktop(t *testing.T) {
	d := newVMSignInDesktop(t, store.DesktopVMSignIns{Store: "memory"})
	h := d.h
	srv := httptest.NewServer(h.gateway)
	t.Cleanup(srv.Close)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	d.st.setReadErr(errors.New("store: pool closed"))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+desktopPath(h, uiDesktopRFB),
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {c.Name + "=" + c.Value}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial = %v %v, want a 503 refusal", resp, err)
	}
	if body, _ := io.ReadAll(resp.Body); !strings.Contains(string(body), reasonDesktopSignInsUnavailable) || !strings.Contains(string(body), desktopSignInsUnreadableMsg) {
		t.Fatalf("refusal = %s, want %s", body, reasonDesktopSignInsUnavailable)
	}
	select {
	case ref := <-d.dr.dialled:
		t.Fatalf("dialled %q with the sign-in mark unreadable", ref)
	default:
	}
}

// A Linux VM run's declared noVNC app shows the same screen as its display and is admitted as the
// display is: a server that never saw the pause, as after a restart, holds the app relay's recording
// from the first frame while the row marks one, so nothing the desktop sends then is recorded.
func TestDesktopSignIns_AVMsNoVNCAppHoldsAPersistedPause(t *testing.T) {
	offered, pasted := make(chan string, 1), make(chan []byte, 1)
	h := desktopHarness(t, rfbDesktopBackend(t, offered, pasted, false), nil)
	h.run.MachineKind, h.run.RecordingLevel = types.MachineLinuxVM, types.RecordingScreenActions
	h.store.putRun(h.run)
	h.srv.cfg.RecordingStore = fsRecordingStore(t)
	at := time.Now().UTC()
	h.srv.cfg.Store = &vmSignInTestStore{uiMemStore: h.store,
		rows: map[uuid.UUID]store.DesktopVMSignIns{h.run.ID: {Store: "memory", PausedBy: "someone", PausedAt: &at}}}
	ds := openDesktop(t, h, desktopApp)
	if ds.resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(ds.resp.Body)
		t.Fatalf("upgrade = %d %s, want 101", ds.resp.StatusCode, body)
	}
	<-offered
	if next, err := readWSFrame(ds.br); err != nil || string(next) != "\x02" {
		t.Fatalf("after ServerInit the browser got %q (err %v), want the Bell", next, err)
	}
	_ = ds.conn.Close()
	_, pb, kept := storedVMRecording(t, h)
	if kept != vmHandshake() {
		t.Errorf("recorded frames = %q, want only the handshake: the Bell came while the pause was open", kept)
	}
	if len(pb.Gaps) != 1 || pb.Gaps[0].Reason != desktopPauseSignin {
		t.Errorf("playback gaps = %+v, want one %q gap", pb.Gaps, desktopPauseSignin)
	}
}

// An open stream whose re-read of the row fails keeps no frames until a re-read answers: it never
// records on a state nobody read.
func TestDesktopSignIns_AnUnreadableMarkMidStreamHoldsTheRecording(t *testing.T) {
	d := newVMSignInDesktop(t, store.DesktopVMSignIns{Store: "memory"})
	h := d.h
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := openVMDesktop(t, ctx, h, d.dr)
	d.st.setReadErr(errors.New("store: pool closed"))
	d.reread(t)
	if !signInHeld(h.srv, h.run.ID) {
		t.Fatal("a stream whose re-read failed kept recording")
	}
	d.send <- vmFBU // sent while the mark cannot be read
	v.read(len(vmFBU))
	d.st.setReadErr(nil)
	d.reread(t)
	if signInHeld(h.srv, h.run.ID) {
		t.Fatal("a re-read that answered left the recording held")
	}
	d.send <- []byte("\x02\x02")
	v.read(2)
	endVMDesktop(t, h, v, d.send)
	if _, _, kept := storedVMRecording(t, h); kept != vmHandshake()+"\x02\x02" {
		t.Errorf("recorded frames = %q, want only the handshake and the Bells once the mark was read", kept)
	}
}

// A re-read that stalls never keeps the stream recording: at the next recheck, with it still
// unanswered, the mark is unknown, so nothing the VM sends is kept until a re-read answers, and once
// one answers unpaused the frames are kept again.
func TestDesktopSignIns_AStalledReReadHoldsTheRecording(t *testing.T) {
	d := newVMSignInDesktop(t, store.DesktopVMSignIns{Store: "memory"})
	h := d.h
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := openVMDesktop(t, ctx, h, d.dr)
	entered, release := make(chan struct{}), make(chan struct{})
	d.st.setOnRead(func(context.Context) func() {
		d.st.setOnRead(nil)
		close(entered)
		<-release // a read that ignores its deadline, as a wedged pool might
		return nil
	})
	unstall := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unstall)
	stalled := d.startReread(t)
	recvSlotEvent(t, entered, "the re-read reaching the store")
	d.reread(t) // the next recheck finds it unanswered
	if !signInHeld(h.srv, h.run.ID) {
		t.Fatal("a stream whose re-read stalled kept recording")
	}
	d.send <- vmFBU // sent while the mark is unknown
	v.read(len(vmFBU))
	unstall()
	recvSlotEvent(t, stalled, "the stalled re-read's answer")
	if signInHeld(h.srv, h.run.ID) {
		t.Fatal("a re-read that answered unpaused left the recording held")
	}
	d.send <- []byte("\x02\x02")
	v.read(2)
	endVMDesktop(t, h, v, d.send)
	if _, _, kept := storedVMRecording(t, h); kept != vmHandshake()+"\x02\x02" {
		t.Errorf("recorded frames = %q, want only the handshake and the Bells once a re-read answered", kept)
	}
}

// A re-read the store answers only after its deadline counts as unread, whatever it says: a pause may
// have marked the row since. The recording stays held until a re-read answers in time.
func TestDesktopSignIns_AReReadAnsweredPastItsDeadlineHoldsTheRecording(t *testing.T) {
	d := newVMSignInDesktop(t, store.DesktopVMSignIns{Store: "memory"})
	h := d.h
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := openVMDesktop(t, ctx, h, d.dr)
	d.st.setOnRead(func(rctx context.Context) func() {
		d.st.setOnRead(nil)
		<-rctx.Done() // released only once the read's own deadline has passed; it then answers unpaused
		return nil
	})
	d.reread(t)
	if !signInHeld(h.srv, h.run.ID) {
		t.Fatal("a re-read answered past its deadline let the stream keep recording")
	}
	d.send <- vmFBU // sent while the mark is unknown
	v.read(len(vmFBU))
	d.reread(t) // answers in time, unpaused
	if signInHeld(h.srv, h.run.ID) {
		t.Fatal("a re-read that answered in time left the recording held")
	}
	d.send <- []byte("\x02\x02")
	v.read(2)
	endVMDesktop(t, h, v, d.send)
	if _, _, kept := storedVMRecording(t, h); kept != vmHandshake()+"\x02\x02" {
		t.Errorf("recorded frames = %q, want only the handshake and the Bells once a re-read answered in time", kept)
	}
}

// A resume that clears the mark between the stream's first read of it and the hold that read takes
// leaves the stream unheld: the second read ends the hold of the pause that is over.
func TestDesktopSignIns_AResumeRacingTheOpeningHoldEndsUnheld(t *testing.T) {
	at := time.Now().UTC()
	d := newVMSignInDesktop(t, store.DesktopVMSignIns{Store: "memory", PausedBy: "someone", PausedAt: &at})
	h := d.h
	reads := 0
	d.st.setOnRead(func(context.Context) func() {
		if reads++; reads != 1 {
			return nil
		}
		return func() { // the first read has answered paused: the resume lands before its hold is taken
			if w := d.signIn(h.srv, "resume"); w.Code != http.StatusOK {
				t.Errorf("resume = %d %s", w.Code, w.Body.String())
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := openVMDesktop(t, ctx, h, d.dr)
	d.st.setOnRead(nil)
	if signInHeld(h.srv, h.run.ID) {
		t.Fatal("a resume that raced the opening hold left the desktop recording held")
	}
	d.send <- []byte("\x02\x02")
	v.read(2)
	endVMDesktop(t, h, v, d.send)
	if _, _, kept := storedVMRecording(t, h); kept != vmHandshake()+"\x02\x02" {
		t.Errorf("recorded frames = %q, want the handshake and the Bells", kept)
	}
}

// A read of the row that is stale by the time it acts ends only the hold of the pause it saw over,
// never a newer pause's. The stream's first read takes the open pause's hold; a resume ends it before
// the second read, and a new pause marks the row and takes its own hold just after that read has
// answered unpaused. The second read's release must leave the new pause's hold, so nothing the VM
// sends during it is recorded.
func TestDesktopSignIns_AStaleReadNeverEndsANewerPausesHold(t *testing.T) {
	at := time.Now().UTC().Add(-time.Minute)
	d := newVMSignInDesktop(t, store.DesktopVMSignIns{Store: "memory", PausedBy: "someone", PausedAt: &at})
	h := d.h
	reads := 0
	d.st.setOnRead(func(context.Context) func() {
		if reads++; reads != 2 {
			return nil
		}
		d.st.setOnRead(nil)
		if w := d.signIn(h.srv, "resume"); w.Code != http.StatusOK { // before the second read
			t.Errorf("resume = %d %s", w.Code, w.Body.String())
		}
		return func() { // the second read has answered unpaused: a new pause lands before it acts
			if w := d.signIn(h.srv, "pause"); w.Code != http.StatusOK {
				t.Errorf("new pause = %d %s", w.Code, w.Body.String())
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := openVMDesktop(t, ctx, h, d.dr)
	if row, _, _ := d.st.DesktopVMSignIns(ctx, h.run.ID); row.PausedAt == nil || row.PausedAt.Equal(at) {
		t.Fatalf("row = %+v, want the new pause's mark", row)
	}
	if !signInHeld(h.srv, h.run.ID) {
		t.Fatal("the stream's stale read ended the new pause's hold on the desktop recording")
	}
	d.send <- vmFBU // sent during the new pause
	v.read(len(vmFBU))
	if w := d.signIn(h.srv, "resume"); w.Code != http.StatusOK {
		t.Fatalf("resume = %d %s", w.Code, w.Body.String())
	}
	d.send <- []byte("\x02\x02")
	v.read(2)
	endVMDesktop(t, h, v, d.send)
	if _, _, kept := storedVMRecording(t, h); kept != vmHandshake()+"\x02\x02" {
		t.Errorf("recorded frames = %q, want the handshake and the Bells after the new pause's resume", kept)
	}
}

// Another person may not pause someone else's run's agent.
func TestDesktopSignIns_OnlyThePersonWhoMayEnter(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	f.cookie = ssoSession(t, "sub-someone-else", "else@corp.example", oidc.RoleUser)
	if code, _, _ := f.do(t, http.MethodPost, "/pause"); code != http.StatusNotFound {
		t.Fatalf("a stranger's pause = %d, want 404", code)
	}
	if len(f.rn.seen()) != 0 {
		t.Fatalf("a stranger's pause reached the runner: %v", f.rn.seen())
	}
}

// A VM's guest report becomes the run's row; a container, and a guest that reported nothing, get none,
// so the run page claims nothing for them.
func TestDesktopSignIns_RecordedOnlyFromTheGuestsReport(t *testing.T) {
	f := newVMSignInFixture(t, "memory")
	ctx := context.Background()
	vm := types.AgentRun{ID: uuid.New(), MachineKind: types.MachineLinuxVM}
	f.srv.recordDesktopVMSignIns(ctx, vm, runner.Sandbox{SignIns: "none"})
	if row, ok, _ := f.st.DesktopVMSignIns(ctx, vm.ID); !ok || row.Store != "none" {
		t.Fatalf("VM row = %+v %v, want none", row, ok)
	}
	for _, c := range []struct {
		run types.AgentRun
		sb  runner.Sandbox
	}{
		{types.AgentRun{ID: uuid.New(), MachineKind: types.MachineLinuxVM}, runner.Sandbox{}},
		{types.AgentRun{ID: uuid.New()}, runner.Sandbox{SignIns: "memory"}},
	} {
		f.srv.recordDesktopVMSignIns(ctx, c.run, c.sb)
		if _, ok, _ := f.st.DesktopVMSignIns(ctx, c.run.ID); ok {
			t.Fatalf("run %+v with sandbox %+v got a row", c.run.MachineKind, c.sb)
		}
	}
}
