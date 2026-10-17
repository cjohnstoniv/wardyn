// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The pause's call sites (RL-7, #572): a decision closing a paused run's last
// request, and every path that execs into a sandbox or carries a person's
// keystrokes. run_pause_test.go pins the pause's own rules; these pin that
// the rest of the product actually calls them.

// TestRunPause_DecideResumesAPausedRun: approving a paused run's last open
// request thaws it at once, through the decide handler's own call. The pause
// is idle, which the sweep's backstop never resumes, so only that call can.
func TestRunPause_DecideResumesAPausedRun(t *testing.T) {
	approvals := newFakeApprovals()
	f := newPauseFixture(t, time.Hour, func(c *Config) { c.Approvals = approvals })
	paused := time.Now().UTC()
	f.st.run.PausedAt, f.st.run.PausedReason = &paused, types.PauseIdle
	id := uuid.New()
	approvals.byID[id] = types.ApprovalRequest{
		ID: id, RunID: f.run.ID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":"example.com"}`),
		State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}

	if w := do(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", adminToken, `{}`); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s, want 200", w.Code, w.Body.String())
	}
	if _, thaws := f.rn.counts(); thaws != 1 {
		t.Fatalf("thaws = %d after the run's last request was decided, want 1", thaws)
	}
	if pausedAt, _ := f.st.paused(); pausedAt != nil {
		t.Errorf("still paused at %v", pausedAt)
	}
	if rows := f.rows("run.resume", "success"); len(rows) != 1 || leaseAuditData(t, rows[0])["reason"] != "request_closed" {
		t.Errorf("run.resume rows = %+v, want one with reason request_closed", rows)
	}
}

// pauseMarks is a minimal store.RunPauser for the wiring fixtures: one paused
// flag, and a count of presence stamps.
type pauseMarks struct {
	pmu    sync.Mutex
	paused bool
	stamps int
}

func (s *pauseMarks) ListPauseCandidates(context.Context) ([]store.PauseCandidate, time.Time, error) {
	return nil, time.Time{}, nil
}

func (s *pauseMarks) StampRunActive(context.Context, uuid.UUID) (bool, error) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.stamps++
	return s.paused, nil
}

func (s *pauseMarks) MarkRunPaused(context.Context, uuid.UUID, types.PauseReason, *time.Time) (bool, error) {
	return false, nil
}

func (s *pauseMarks) ClearRunPaused(context.Context, uuid.UUID) (bool, error) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	was := s.paused
	s.paused = false
	return was, nil
}

func (s *pauseMarks) RunHasOpenRequest(context.Context, uuid.UUID) (bool, error) {
	return false, nil
}

func (s *pauseMarks) state() (paused bool, stamps int) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return s.paused, s.stamps
}

// wiredPauseStore is the attach fixture's store (touchCountingStore) with the
// pause marks.
type wiredPauseStore struct {
	*touchCountingStore
	pauseMarks
}

var _ store.RunPauser = (*wiredPauseStore)(nil)

// thawLog is a runner.Freezer that counts thaws, and notes how many had
// happened by each exec.
type thawLog struct {
	fmu         sync.Mutex
	thaws       int
	thawsAtExec []int
}

func (r *thawLog) FreezeSandbox(context.Context, string) error { return nil }

func (r *thawLog) ThawSandbox(context.Context, string) error {
	r.fmu.Lock()
	defer r.fmu.Unlock()
	r.thaws++
	return nil
}

func (r *thawLog) noteExec() {
	r.fmu.Lock()
	defer r.fmu.Unlock()
	r.thawsAtExec = append(r.thawsAtExec, r.thaws)
}

func (r *thawLog) thawCount() int {
	r.fmu.Lock()
	defer r.fmu.Unlock()
	return r.thaws
}

func (r *thawLog) thawsBeforeExec(i int) int {
	r.fmu.Lock()
	defer r.fmu.Unlock()
	if i >= len(r.thawsAtExec) {
		return -1
	}
	return r.thawsAtExec[i]
}

// wiredPauseRunner is holderTestRunner with the thaw log; Attach is its exec.
type wiredPauseRunner struct {
	*holderTestRunner
	thawLog
}

func (r *wiredPauseRunner) Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error) {
	r.noteExec()
	return r.holderTestRunner.Attach(ctx, ref, opts)
}

// newWiredPauseServer is holderTestServer's wiring on the pause-aware store
// and runner: a RUNNING run owned by holderOwner.
func newWiredPauseServer(t *testing.T) (*Server, *wiredPauseStore, *wiredPauseRunner, types.AgentRun) {
	t.Helper()
	ast := newAuthzStore()
	st := &wiredPauseStore{touchCountingStore: &touchCountingStore{authzStore: ast}}
	fr := &wiredPauseRunner{holderTestRunner: &holderTestRunner{}}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.Audit = &sshTestRecorder{}
	cfg.Runner = fr
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: holderOwner, State: types.RunRunning, SandboxRef: "sbx-1"}
	ast.mu.Lock()
	ast.runs[run.ID] = run
	ast.mu.Unlock()
	return srv, st, fr, run
}

// forgetPresence drops the run's presence debounce, so the next keystroke
// stamps again inside the minute.
func forgetPresence(srv *Server, runID uuid.UUID) {
	srv.pause.mu.Lock()
	defer srv.pause.mu.Unlock()
	delete(srv.pause.presence, runID)
}

// TestRunPause_WebAttachThawsFirstAndKeystrokesArePresence: a web attach to a
// paused run thaws it before the exec (the daemon refuses an exec into a
// paused container), and the writer's keystrokes move the presence clock.
func TestRunPause_WebAttachThawsFirstAndKeystrokesArePresence(t *testing.T) {
	srv, st, fr, run := newWiredPauseServer(t)
	st.paused = true
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	readAttachMode(t, c)
	waitFor(t, "the holder's session to open", func() bool { return fr.session(0) != nil })
	if atAttach := fr.thawsBeforeExec(0); atAttach != 1 {
		t.Fatalf("thaws before the attach exec = %d, want 1", fr.thawsBeforeExec(0))
	}
	if paused, _ := st.state(); paused {
		t.Fatal("the run is still marked paused after the attach thawed it")
	}

	forgetPresence(srv, run.ID)
	_, before := st.state()
	wsWrite(t, c, websocket.MessageBinary, []byte("ls\r"))
	waitFor(t, "the keystrokes to reach the sandbox", func() bool { return fr.session(0).written() >= 1 })
	if _, after := st.state(); after != before+1 {
		t.Errorf("presence stamps after a writer's keystrokes = %d, want %d", after, before+1)
	}
}

// TestRunPause_SSHChannelThawsFirst: every ssh channel goes through
// sshFreshRun before it execs, and that thaws a paused run.
func TestRunPause_SSHChannelThawsFirst(t *testing.T) {
	srv, st, fr, run := newWiredPauseServer(t)
	st.paused = true
	if _, msg := srv.sshFreshRun(context.Background(), run.ID, holderOwner); msg != "" {
		t.Fatalf("sshFreshRun refused: %s", msg)
	}
	if n := fr.thawCount(); n != 1 {
		t.Fatalf("thaws = %d, want 1 before any ssh exec", n)
	}
	if paused, _ := st.state(); paused {
		t.Error("the run is still marked paused after an ssh channel thawed it")
	}
}

// TestRunPause_SSHKeystrokesArePresence: bytes a person sends on an ssh shell
// and on an ssh exec's stdin move the presence clock.
func TestRunPause_SSHKeystrokesArePresence(t *testing.T) {
	t.Run("shell", func(t *testing.T) {
		srv, st, fr, run := newWiredPauseServer(t)
		ch := newFakeSSHChannel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.bridgeSSHShell(context.Background(), run.ID, holderOwner, ch, 80, 24, make(chan sshWindowChangeMsg, 1))
		}()
		waitFor(t, "the shell session to open", func() bool { return fr.session(0) != nil })
		forgetPresence(srv, run.ID)
		_, before := st.state()
		ch.clientSend("ls\r")
		waitFor(t, "the keystrokes to reach the sandbox", func() bool { return fr.session(0).written() >= 1 })
		if _, after := st.state(); after != before+1 {
			t.Errorf("presence stamps after shell keystrokes = %d, want %d", after, before+1)
		}
		_ = ch.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the ssh shell bridge never returned")
		}
	})
	t.Run("exec stdin", func(t *testing.T) {
		srv, st, _, run := newWiredPauseServer(t)
		ch := newFakeSSHChannel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			srv.sshBridgeExecSession(context.Background(), run.ID, holderOwner, ch, fakeEchoExecSession(), false)
		}()
		ch.clientSend("hello")
		waitFor(t, "the stdin echo", func() bool {
			ch.mu.Lock()
			defer ch.mu.Unlock()
			return strings.Contains(ch.out.String(), "hello")
		})
		if _, stamps := st.state(); stamps != 1 {
			t.Errorf("presence stamps after exec stdin = %d, want 1", stamps)
		}
		_ = ch.inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the ssh exec bridge never returned")
		}
	})
}

// TestAttachHolder_OnlyAWritersInputIsPresence: writeGated tells onInput only
// for the holder that may write; an observer's dropped bytes are not a person
// at the run.
func TestAttachHolder_OnlyAWritersInputIsPresence(t *testing.T) {
	srv := New(Config{Audit: &recRecorder{}, AdminToken: adminToken})
	runID := uuid.New()
	var writerInputs, observerInputs int
	writer := &attachHolder{principal: holderOwner, source: attachSourceWeb, onInput: func() { writerInputs++ }}
	_, release := srv.registerAttachHolder(runID, writer)
	defer release()
	observer := &attachHolder{principal: holderSecond, source: attachSourceWeb, onInput: func() { observerInputs++ }}
	readOnly, releaseObserver := srv.registerAttachHolder(runID, observer)
	defer releaseObserver()
	if !readOnly {
		t.Fatal("the second attach was admitted as a writer")
	}
	sess := newCountingShellSession()
	if err := writer.writeGated(sess, []byte("ls")); err != nil {
		t.Fatalf("writer writeGated: %v", err)
	}
	if err := observer.writeGated(sess, []byte("x")); err != nil {
		t.Fatalf("observer writeGated: %v", err)
	}
	if writerInputs != 1 || observerInputs != 0 {
		t.Errorf("onInput calls: writer %d, observer %d; want 1 and 0", writerInputs, observerInputs)
	}
}

// uiPauseStore / uiPauseRunner put the pause marks and the thaw log on the UI
// gateway harness; the relay's exec is socat.
type uiPauseStore struct {
	*uiMemStore
	pauseMarks
}

type uiPauseRunner struct {
	*sshFakeRunner
	thawLog
}

func (r *uiPauseRunner) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if len(spec.Argv) > 0 && spec.Argv[0] == "socat" {
		r.noteExec()
	}
	return r.sshFakeRunner.ExecStream(ctx, ref, spec)
}

// TestRunPause_UIRelayThawsBeforeItsExec: a request relayed to a paused
// run's in-sandbox UI thaws it before the relay's socat exec. Both the relay's
// presence stamp and uiDial's exec thaw can do it; this pins that one does.
func TestRunPause_UIRelayThawsBeforeItsExec(t *testing.T) {
	h := newUIHarness(t, okBackend())
	ps := &uiPauseStore{uiMemStore: h.store}
	pr := &uiPauseRunner{sshFakeRunner: h.runner}
	h.srv.cfg.Store, h.srv.cfg.Runner = ps, pr
	cookie := h.openSession()
	ps.pmu.Lock()
	ps.paused = true
	ps.pmu.Unlock()

	if rec := h.relay("/", cookie, nil); rec.Code != http.StatusOK {
		t.Fatalf("relay = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if n := pr.thawsBeforeExec(0); n != 1 {
		t.Fatalf("thaws before the relay's exec = %d, want 1", n)
	}
	if paused, _ := ps.state(); paused {
		t.Error("the run is still marked paused after the relay thawed it")
	}
}

// reauthPauseStore is the pause fixture's store with the re-auth resolver's
// one-transaction seam, which always lands.
type reauthPauseStore struct{ *pauseStore }

func (s *reauthPauseStore) ResolveReauthApproval(context.Context, uuid.UUID, types.ApprovalDecision, types.AuditEvent) (types.ApprovalRequest, error) {
	return types.ApprovalRequest{}, nil
}

// TestRunPause_AWSReauthResolveResumesAPausedRun: the AWS reconcile-on-read
// resolving a paused run's re-auth request thaws it, through resolveReauth's own
// call. The capture door's resume is TestRunPause_AWSCaptureResumesAPausedRun.
func TestRunPause_AWSReauthResolveResumesAPausedRun(t *testing.T) {
	f := newPauseFixture(t, time.Hour)
	paused := time.Now().UTC()
	f.st.run.PausedAt, f.st.run.PausedReason = &paused, types.PauseIdle
	f.srv.cfg.Store = &reauthPauseStore{pauseStore: f.st}
	ap := types.ApprovalRequest{
		ID: uuid.New(), RunID: f.run.ID, Kind: types.ApprovalCredentialReauth,
		RequestedScope: json.RawMessage(`{"owner":"` + pauseOwner + `","provider":"` + awsSSOProvider + `"}`),
		State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	if err := f.srv.resolveReauth(context.Background(), ap, pauseOwner, uuid.Nil); err != nil {
		t.Fatalf("resolveReauth: %v", err)
	}
	if _, thaws := f.rn.counts(); thaws != 1 {
		t.Fatalf("thaws = %d after the re-auth resolved, want 1", thaws)
	}
	if pausedAt, _ := f.st.paused(); pausedAt != nil {
		t.Errorf("still paused at %v", pausedAt)
	}
}

// adoPauseStore is the ADO capability fixture's store with the pause marks.
// That fixture has no run row, so GetRun answers one RUNNING run with a
// sandbox, paused while the marks say so.
type adoPauseStore struct {
	*adoCapStore
	pauseMarks
}

func (s *adoPauseStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	r := types.AgentRun{ID: id, State: types.RunRunning, SandboxRef: "sbx-ado"}
	if paused, _ := s.state(); paused {
		now := time.Now().UTC()
		r.PausedAt, r.PausedReason = &now, types.PauseIdle
	}
	return r, nil
}

// freezingRunner is any runner with the thaw log, whose sandbox reads running.
type freezingRunner struct {
	runner.Runner
	thawLog
}

func (r *freezingRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil
}

// TestRunPause_ADOReauthResolveResumesAPausedRun: the Azure DevOps sign-in
// resolving a paused run's consent request thaws it, through
// reconcileADOReauthOnRead's own call (which the sign-in runs).
func TestRunPause_ADOReauthResolveResumesAPausedRun(t *testing.T) {
	f := newADOCapFixture(t)
	a := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, uuid.Nil, prPath), adoCapabilityPendingState)
	f.decide(t, a, types.ApprovalApproved, types.ScopeOnce)
	f.fake.SetConsentRequired(true)
	b := pendingID(t, f.ask(t, adoscope.CapPR, types.FirstUseWaitForReview, a, prPath), reauthPendingState)
	ps := &adoPauseStore{adoCapStore: f.srv.cfg.Store.(*adoCapStore)}
	ps.paused = true
	fr := &freezingRunner{Runner: f.srv.cfg.Runner}
	f.srv.cfg.Store, f.srv.cfg.Runner = ps, fr

	// The sign-in itself resolves the pending consent request.
	f.fake.SetConsentRequired(false)
	later := time.Now().Add(time.Minute)
	f.srv.cfg.Now = func() time.Time { return later }
	if w := f.capture(t, f.subject); w.Code != http.StatusFound {
		t.Fatalf("re-sign-in: %d %s", w.Code, w.Body.String())
	}
	if got := f.row(b); got.State != types.ApprovalApproved {
		t.Fatalf("after the sign-in the consent request is %s, want APPROVED", got.State)
	}
	if n := fr.thawCount(); n != 1 {
		t.Fatalf("thaws = %d after the consent resolved, want 1", n)
	}
	if paused, _ := ps.state(); paused {
		t.Error("the run is still marked paused after its request resolved")
	}
}
