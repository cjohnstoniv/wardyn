// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	// markErr fails PauseDesktopVMAgent.
	markErr error
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

func (s *vmSignInTestStore) DesktopVMSignIns(_ context.Context, runID uuid.UUID) (store.DesktopVMSignIns, bool, error) {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	row, ok := s.rows[runID]
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

func (s *vmSignInTestStore) ResumeDesktopVMAgent(_ context.Context, runID uuid.UUID) (string, time.Time, bool, error) {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	row, ok := s.rows[runID]
	if !ok || row.PausedAt == nil {
		return "", time.Time{}, false, nil
	}
	by, at := row.PausedBy, *row.PausedAt
	row.PausedBy, row.PausedAt = "", nil
	s.rows[runID] = row
	return by, at, true, nil
}

// pauseFakeRunner is the guest agent behind runner.AgentPauser: it records each call and whether the
// agent is stopped. A pause's stop lands even when it answers pauseErr, as one whose answer was lost.
type pauseFakeRunner struct {
	*sshFakeRunner
	pmu      sync.Mutex
	calls    []string
	pauseErr error
	stopped  bool
	// hold, when set, keeps each PauseAgent inside the call until it is closed, after telling inPause.
	inPause chan struct{}
	hold    chan struct{}
}

var _ runner.AgentPauser = (*pauseFakeRunner)(nil)

func (f *pauseFakeRunner) PauseAgent(_ context.Context, ref string) error {
	f.pmu.Lock()
	f.calls = append(f.calls, "pause "+ref)
	f.pmu.Unlock()
	if f.hold != nil {
		f.inPause <- struct{}{}
		<-f.hold
	}
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.stopped = true
	return f.pauseErr
}

func (f *pauseFakeRunner) ResumeAgent(_ context.Context, ref string) error {
	f.pmu.Lock()
	defer f.pmu.Unlock()
	f.calls = append(f.calls, "resume "+ref)
	f.stopped = false
	return nil
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
	hold := vmSignInHold(f.run)
	if code, _, body := f.do(t, http.MethodPost, "/pause"); code != http.StatusOK {
		t.Fatalf("pause = %d %s", code, body)
	}
	if !f.srv.desktopRecs.holds(f.run.ID, hold) {
		t.Fatal("the desktop recording is not held paused while the sign-in is open")
	}
	if p, r := desktopRows(f.h.audit); p != 1 || r != 0 {
		t.Fatalf("after the pause: %d desktop.recording.pause and %d .resume rows, want 1 and 0", p, r)
	}
	if code, _, body := f.do(t, http.MethodPost, "/resume"); code != http.StatusOK {
		t.Fatalf("resume = %d %s", code, body)
	}
	if f.srv.desktopRecs.holds(f.run.ID, hold) {
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
	hold := vmSignInHold(f.run)
	release, paused := f.pauseInFlight(t)
	if code, v, body := f.do(t, http.MethodGet, ""); code != http.StatusOK || v.Paused || v.PausedAt != nil {
		t.Fatalf("GET while the runner has not answered the pause = %d %s, want not paused", code, body)
	}
	if f.srv.desktopRecs.holds(f.run.ID, hold) {
		t.Fatal("the desktop recording was held before the runner stopped the agent")
	}
	release()
	if a := recvSlotEvent(t, paused, "the pause's answer"); a.code != http.StatusOK || !a.v.Paused {
		t.Fatalf("pause = %d %s", a.code, a.body)
	}
	if _, v, _ := f.do(t, http.MethodGet, ""); !v.Paused {
		t.Fatal("GET after the runner's answer does not say paused")
	}
	if !f.srv.desktopRecs.holds(f.run.ID, hold) {
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
	if f.srv.desktopRecs.holds(f.run.ID, vmSignInHold(f.run)) {
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
	if f.srv.desktopRecs.holds(f.run.ID, vmSignInHold(f.run)) {
		t.Fatal("an unrecorded pause left the desktop recording held")
	}
	if rows := f.signInRows(); len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("rows = %+v, want one failure", rows)
	}
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
