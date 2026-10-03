// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// THE ATTACH ORDER (term-t7): registration decides the role BEFORE the exec is
// opened, so an observer's tmux client is created with ignore-size and the
// writer's window is never resized by anyone but the writer. These pin the
// order, the size every client attaches at, the attach states (a slow attach
// must neither look dead nor lose authority checks), and the promotion
// re-attach, on both lanes. The instrument is a runner whose Attach the test
// controls call by call.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// scriptedRunner decides each Attach through `attach` (n is the 0-based call
// index) and records the options and the session of every call. A nil `attach`
// hands out a countingShellSession.
type scriptedRunner struct {
	fakeRunner
	mu     sync.Mutex
	calls  []runner.AttachOptions
	sess   map[int]runner.Session
	attach func(ctx context.Context, n int, opts runner.AttachOptions) (runner.Session, error)
}

func (r *scriptedRunner) Attach(ctx context.Context, _ string, opts runner.AttachOptions) (runner.Session, error) {
	r.mu.Lock()
	n := len(r.calls)
	r.calls = append(r.calls, opts)
	r.mu.Unlock()
	var sess runner.Session = newCountingShellSession()
	if r.attach != nil {
		var err error
		if sess, err = r.attach(ctx, n, opts); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	if r.sess == nil {
		r.sess = map[int]runner.Session{}
	}
	r.sess[n] = sess
	r.mu.Unlock()
	return sess, nil
}

func (r *scriptedRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *scriptedRunner) opts(i int) runner.AttachOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.calls) {
		return runner.AttachOptions{}
	}
	return r.calls[i]
}

// session waits for the i-th Attach to have RETURNED a session.
func (r *scriptedRunner) session(t *testing.T, i int) *countingShellSession {
	t.Helper()
	var s runner.Session
	waitFor(t, "the attach exec to open", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		s = r.sess[i]
		return s != nil
	})
	cs, ok := s.(*countingShellSession)
	if !ok {
		t.Fatalf("attach %d did not return a countingShellSession: %T", i, s)
	}
	return cs
}

// gateAttach holds Attach number n until the test closes release. With
// honourCtx the wait also ends on the attach context (a runner that notices the
// cancel); without it the session comes back LATE, after the connection ended,
// which is the case the identity close exists for.
func gateAttach(n int, release <-chan struct{}, honourCtx bool, started chan<- struct{}) func(context.Context, int, runner.AttachOptions) (runner.Session, error) {
	return func(ctx context.Context, i int, _ runner.AttachOptions) (runner.Session, error) {
		if i != n {
			return newCountingShellSession(), nil
		}
		if started != nil {
			close(started)
		}
		if honourCtx {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		} else {
			<-release
		}
		return newCountingShellSession(), nil
	}
}

// scriptedServer is holderTestServer with the runner swapped in.
func scriptedServer(t *testing.T, rn runner.Runner) (*Server, *sshTestRecorder, types.AgentRun) {
	t.Helper()
	ast := newAuthzStore()
	st := &touchCountingStore{authzStore: ast}
	rec := &sshTestRecorder{}
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.Audit = rec
	cfg.Runner = rn
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: holderOwner, OperatorOwned: true, State: types.RunRunning, SandboxRef: "sbx-t7"}
	ast.mu.Lock()
	ast.runs[run.ID] = run
	ast.mu.Unlock()
	return srv, rec, run
}

func resizeFrame(cols, rows int) []byte {
	return mustJSON(map[string]any{"type": "resize", "cols": cols, "rows": rows})
}

func auditIndex(events []types.AuditEvent, runID uuid.UUID, action, outcome string, nth int) int {
	seen := 0
	for i := range events {
		ev := &events[i]
		if ev.RunID != nil && *ev.RunID == runID && ev.Action == action && ev.Outcome == outcome {
			if seen == nth {
				return i
			}
			seen++
		}
	}
	return -1
}

// The writer's exec starts at the size the browser sent and nothing resizes it
// afterwards; an observer is attached as an observer at the WRITER's size (its
// own 80x24 is not what it opens at), and every accepted writer resize reaches
// the observer's PTY and, as an attach-mode frame, its browser.
func TestAttachWS_WriterSeededObserverIgnoresSizeAndFollowsWriter(t *testing.T) {
	rn := &scriptedRunner{}
	srv, _, run := scriptedServer(t, rn)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=120&rows=40")
	if m := readAttachMode(t, c1); m.ReadOnly {
		t.Fatal("the first client was told it is read-only")
	}
	writerSess := rn.session(t, 0)
	if o := rn.opts(0); o != (runner.AttachOptions{Cols: 120, Rows: 40}) {
		t.Fatalf("writer attach options = %+v, want 120x40 and no observer flag", o)
	}

	c2 := dialAttach(t, ts, srv, run.ID, holderSecond, "&cols=80&rows=24")
	if m := readAttachMode(t, c2); !m.ReadOnly {
		t.Fatal("the second client was admitted writable")
	}
	observerSess := rn.session(t, 1)
	if o := rn.opts(1); o != (runner.AttachOptions{Cols: 120, Rows: 40, Observer: true}) {
		t.Fatalf("observer attach options = %+v, want the writer's 120x40 with the observer flag (ignore-size)", o)
	}
	if n := writerSess.resizeCount(); n != 0 {
		t.Errorf("the writer's exec was resized %d time(s) after registration; it attaches at its real size", n)
	}

	// The writer resizes: its PTY, the observer's PTY and the observer's browser
	// all follow, and the observer's own size is never applied.
	wsWrite(t, c1, websocket.MessageText, resizeFrame(140, 50))
	m := readNextAttachMode(t, c2)
	if !m.ReadOnly || m.Holder == nil || m.Holder.Cols != 140 || m.Holder.Rows != 50 || m.Holder.Principal != holderOwner {
		t.Fatalf("observer attach-mode after the writer's resize = %+v, want read_only with the writer at 140x50", m)
	}
	waitFor(t, "the writer's PTY to take 140x50", func() bool { c, r := writerSess.lastSize(); return c == 140 && r == 50 })
	waitFor(t, "the observer's PTY to follow the writer to 140x50", func() bool { c, r := observerSess.lastSize(); return c == 140 && r == 50 })

	// An observer's own resize is kept for its promotion and applied to nothing.
	wsWrite(t, c2, websocket.MessageText, resizeFrame(20, 5))
	go drainClient(c2)
	wsPing(t, c2)
	if c, r := observerSess.lastSize(); c != 140 || r != 50 {
		t.Errorf("the observer's PTY is %dx%d after its own 20x5 resize frame, want it pinned to the writer's 140x50", c, r)
	}
	if v := srv.attachHolderFor(run.ID).view(); v.Cols != 140 || v.Rows != 50 {
		t.Errorf("the writer's registry geometry = %+v, want 140x50", v)
	}
}

// An exec that cannot be opened ends the connection with 1011, frees the
// holder, and is audited as a second session.attach row behind the first.
func TestAttachWS_AttachFailureReleasesHolder(t *testing.T) {
	rn := &scriptedRunner{attach: func(_ context.Context, n int, _ runner.AttachOptions) (runner.Session, error) {
		if n == 0 {
			return nil, io.ErrClosedPipe
		}
		return newCountingShellSession(), nil
	}}
	srv, rec, run := scriptedServer(t, rn)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=80&rows=24")
	readAttachMode(t, c1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := c1.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusInternalError {
		t.Fatalf("close status after a failed attach = %v (%v), want 1011", got, err)
	}
	waitFor(t, "the failed attach to release the holder", func() bool { return srv.attachHolderFor(run.ID) == nil })

	first := waitForAudit(t, rec, run.ID, "session.attach", "success")
	fail := waitForAudit(t, rec, run.ID, "session.attach", "failure")
	if first == nil || fail == nil {
		t.Fatalf("want a registration row and a failure row; events = %s", auditDump(rec.snapshot(), run.ID))
	}
	if !strings.Contains(string(first.Data), `"state":"attaching"`) || !strings.Contains(string(fail.Data), `"state":"attaching"`) {
		t.Errorf("session.attach rows = %s / %s, want state attaching on both", first.Data, fail.Data)
	}
	ev := rec.snapshot()
	if auditIndex(ev, run.ID, "session.attach", "success", 0) > auditIndex(ev, run.ID, "session.attach", "failure", 0) {
		t.Errorf("the failure row precedes the registration row: %s", auditDump(ev, run.ID))
	}

	// The slot is free: the next client is the writer, not queued behind a ghost.
	c2 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=80&rows=24")
	if m := readAttachMode(t, c2); m.ReadOnly {
		t.Fatal("the next client was admitted read-only behind a holder whose attach failed")
	}
}

// A slow Attach must not look like a dead peer: the reader runs before the exec
// exists, so a ping is answered and a close is honoured while Attach is still
// in flight, and the cancel reaches the attach.
func TestAttachWS_SlowAttachAnswersPingsAndHonoursClose(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	ctxEnded := make(chan struct{})
	rn := &scriptedRunner{attach: func(ctx context.Context, n int, opts runner.AttachOptions) (runner.Session, error) {
		s, err := gateAttach(0, release, true, started)(ctx, n, opts)
		if err != nil {
			close(ctxEnded)
		}
		return s, err
	}}
	srv, _, run := scriptedServer(t, rn)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=80&rows=24")
	readAttachMode(t, c1)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("Attach was never called")
	}
	go drainClient(c1)
	wsPing(t, c1) // answered while Attach is parked

	if err := c1.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("close during a slow attach: %v", err)
	}
	select {
	case <-ctxEnded:
	case <-time.After(3 * time.Second):
		t.Fatal("the close did not cancel the attach in flight")
	}
	waitFor(t, "the closed connection to release its holder", func() bool { return srv.attachHolderFor(run.ID) == nil })
}

// A session that comes back after its connection ended is closed by its own
// identity, and nothing was ever written to it.
func TestAttachWS_LateSessionFromACancelledAttachIsClosed(t *testing.T) {
	release := make(chan struct{})
	rn := &scriptedRunner{attach: gateAttach(0, release, false, nil)}
	srv, _, run := scriptedServer(t, rn)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=80&rows=24")
	readAttachMode(t, c1)
	waitFor(t, "Attach to be called", func() bool { return rn.callCount() == 1 })
	wsWrite(t, c1, websocket.MessageBinary, []byte("held\r"))
	if err := c1.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitFor(t, "the connection to release its holder", func() bool { return srv.attachHolderFor(run.ID) == nil })

	close(release) // the runner finally answers
	late := rn.session(t, 0)
	waitFor(t, "the late session to be closed", late.isClosed)
	if n := late.written(); n != 0 {
		t.Errorf("%d write(s) reached a session whose connection had ended", n)
	}
}

// Input typed while the exec is still opening is held, and a take-over decided
// meanwhile discards it: not one held byte reaches any session.
func TestAttachWS_EvictionDuringSlowAttachDropsHeldInput(t *testing.T) {
	release := make(chan struct{})
	rn := &scriptedRunner{attach: gateAttach(0, release, false, nil)}
	srv, rec, run := scriptedServer(t, rn)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=80&rows=24")
	readAttachMode(t, c1)
	waitFor(t, "Attach to be called", func() bool { return rn.callCount() == 1 })
	go drainClient(c1)
	wsWrite(t, c1, websocket.MessageBinary, []byte("held-1\r"))
	wsWrite(t, c1, websocket.MessageBinary, []byte("held-2\r"))
	wsPing(t, c1) // the server's reader consumed both frames ahead of the ping

	if srv.evictAttachHolder(run.ID) == nil {
		t.Fatal("nothing to evict: the attaching writer was not registered")
	}
	close(release)
	late := rn.session(t, 0)
	waitFor(t, "the evicted holder's late session to be closed", late.isClosed)
	if n := late.written(); n != 0 {
		t.Errorf("%d held write(s) reached the session of a holder evicted during its attach", n)
	}
	waitForAudit(t, rec, run.ID, "session.detach", "success")
	if n := late.written(); n != 0 {
		t.Errorf("%d held write(s) reached the session after the detach", n)
	}
}

// blockedWriteSession parks every Write until it is closed.
type blockedWriteSession struct {
	*countingShellSession
	entered chan struct{}
	once    sync.Once
}

func (s *blockedWriteSession) Write(p []byte) (int, error) {
	s.once.Do(func() { close(s.entered) })
	for !s.isClosed() {
		time.Sleep(5 * time.Millisecond)
	}
	return 0, io.ErrClosedPipe
}

// With Session.Write blocked the socket reader is not: pong and close are still
// processed, because the reader never writes.
func TestAttachWS_BlockedSessionWriteStillProcessesPongAndClose(t *testing.T) {
	bs := &blockedWriteSession{countingShellSession: newCountingShellSession(), entered: make(chan struct{})}
	rn := &scriptedRunner{attach: func(context.Context, int, runner.AttachOptions) (runner.Session, error) { return bs, nil }}
	srv, _, run := scriptedServer(t, rn)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=80&rows=24")
	readAttachMode(t, c1)
	wsWrite(t, c1, websocket.MessageBinary, []byte("paste\r"))
	select {
	case <-bs.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the keystrokes never reached Session.Write")
	}
	go drainClient(c1)
	wsPing(t, c1) // processed by a reader that is not stuck behind the write
	if err := c1.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("close with a blocked write: %v", err)
	}
	waitFor(t, "the close to release the holder despite the blocked write", func() bool { return srv.attachHolderFor(run.ID) == nil })
}

// The input queue is bounded: past attachReadLimit held bytes the socket is
// closed with 1009, rather than the reader blocking or a chunk vanishing.
func TestAttachWS_HeldInputOverflowClosesWith1009(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	rn := &scriptedRunner{attach: gateAttach(0, release, true, nil)}
	srv, _, run := scriptedServer(t, rn)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=80&rows=24")
	readAttachMode(t, c1)
	waitFor(t, "Attach to be called", func() bool { return rn.callCount() == 1 })
	chunk := make([]byte, 700<<10)
	wsWrite(t, c1, websocket.MessageBinary, chunk)
	wsWrite(t, c1, websocket.MessageBinary, chunk)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, _, err := c1.Read(ctx)
		if err == nil {
			continue
		}
		if got := websocket.CloseStatus(err); got != websocket.StatusMessageTooBig {
			t.Fatalf("close status on input overflow = %v (%v), want 1009", got, err)
		}
		return
	}
}

// blockedResizeSession parks Resize until released and records the sizes.
type blockedResizeSession struct {
	*countingShellSession
	gate chan struct{}
}

func (s *blockedResizeSession) Resize(ctx context.Context, cols, rows uint16) error {
	<-s.gate
	return s.countingShellSession.Resize(ctx, cols, rows)
}

// Resizes coalesce latest-wins off the reader: a Resize parked in the driver
// delays only the resize worker, never pong, and the newest size is the last
// one applied.
func TestAttachWS_ResizesCoalesceOffTheReader(t *testing.T) {
	bs := &blockedResizeSession{countingShellSession: newCountingShellSession(), gate: make(chan struct{})}
	rn := &scriptedRunner{attach: func(context.Context, int, runner.AttachOptions) (runner.Session, error) { return bs, nil }}
	srv, _, run := scriptedServer(t, rn)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=80&rows=24")
	readAttachMode(t, c1)
	waitFor(t, "the exec to open", func() bool { return rn.callCount() == 1 })
	go drainClient(c1)
	for i := 0; i < 6; i++ {
		wsWrite(t, c1, websocket.MessageText, resizeFrame(100+i, 30+i))
	}
	wsPing(t, c1) // the reader got through all six although the first is parked in Resize
	close(bs.gate)
	waitFor(t, "the newest size to be the last applied", func() bool { c, r := bs.lastSize(); return c == 105 && r == 35 })
	if n := bs.resizeCount(); n > 3 {
		t.Errorf("%d Resize calls for six frames sent while one was parked; the rest should coalesce", n)
	}
}

// Promotion re-attaches on the same socket: the observer's exec is closed and a
// writer's is opened (no ignore-size) at the PROMOTED client's own size. With
// two observers, the one promoted is the oldest, whatever the other did last.
func TestAttachWS_PromotionReattachesAtThePromotedClientsOwnSize(t *testing.T) {
	rn := &scriptedRunner{}
	srv, _, run := scriptedServer(t, rn)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=100&rows=30")
	readAttachMode(t, c1)
	rn.session(t, 0)
	c2 := dialAttach(t, ts, srv, run.ID, holderSecond, "&cols=90&rows=25")
	readAttachMode(t, c2)
	firstObs := rn.session(t, 1)
	c3 := dialAttach(t, ts, srv, run.ID, "carol@example.com", "&cols=80&rows=20")
	readAttachMode(t, c3)
	rn.session(t, 2)

	// The later observer is the more recently active one.
	wsWrite(t, c3, websocket.MessageText, resizeFrame(70, 18))
	go drainClient(c3)
	wsPing(t, c3)

	if err := c1.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close the writer: %v", err)
	}
	m := readNextAttachMode(t, c2)
	if m.ReadOnly || m.Holder == nil || m.Holder.Principal != holderSecond {
		t.Fatalf("promotion frame = %+v, want the oldest observer promoted", m)
	}
	// The frame is sent after the writer exec is open.
	if rn.callCount() != 4 {
		t.Fatalf("attach calls = %d, want 4 (writer, two observers, the promoted writer exec)", rn.callCount())
	}
	if o := rn.opts(3); o != (runner.AttachOptions{Cols: 90, Rows: 25}) {
		t.Fatalf("promoted writer exec options = %+v, want the promoted client's own 90x25, not the other observer's 70x18, and no observer flag", o)
	}
	waitFor(t, "the observer exec to be closed", firstObs.isClosed)
	if v := srv.attachHolderFor(run.ID).view(); v.Principal != holderSecond || v.Cols != 90 || v.Rows != 25 {
		t.Errorf("registry writer after promotion = %+v, want %s at 90x25", v, holderSecond)
	}
}

// A promotion's writer exec re-checks authority: a holder evicted while its
// writer exec is still being opened never gets it, the late session is closed,
// and the input it had typed meanwhile reaches neither exec.
func TestAttachWS_PromotedHolderEvictedMidReattachGetsNoExec(t *testing.T) {
	release := make(chan struct{})
	rn := &scriptedRunner{attach: gateAttach(2, release, false, nil)}
	srv, _, run := scriptedServer(t, rn)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=100&rows=30")
	readAttachMode(t, c1)
	rn.session(t, 0)
	c2 := dialAttach(t, ts, srv, run.ID, holderSecond, "&cols=90&rows=25")
	readAttachMode(t, c2)
	observerSess := rn.session(t, 1)
	go drainClient(c2)

	if err := c1.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("close the writer: %v", err)
	}
	waitFor(t, "the promoted holder's writer exec to be requested", func() bool { return rn.callCount() == 3 })
	if o := rn.opts(2); o.Observer {
		t.Fatalf("the promotion re-attach was opened as an observer: %+v", o)
	}
	// The promoted client types while its exec is still opening...
	wsWrite(t, c2, websocket.MessageBinary, []byte("held\r"))
	wsPing(t, c2)
	// ...and is taken over before it lands.
	if srv.evictAttachHolder(run.ID) == nil {
		t.Fatal("nothing to evict: the promoted holder is not the registered writer")
	}
	close(release)
	late := rn.session(t, 2)
	waitFor(t, "the evicted holder's late writer exec to be closed", late.isClosed)
	if late.written() != 0 || observerSess.written() != 0 {
		t.Errorf("held input reached an exec of an evicted holder (writer exec %d, observer exec %d)", late.written(), observerSess.written())
	}
}

// A take-over is audited after the attach row it displaces, even when it lands
// while the attach is still in flight.
func TestAttachWS_TakeoverRowNeverPrecedesTheAttachRow(t *testing.T) {
	release := make(chan struct{})
	rn := &scriptedRunner{attach: gateAttach(0, release, false, nil)}
	srv, rec, run := scriptedServer(t, rn)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=80&rows=24")
	readAttachMode(t, c1)
	waitFor(t, "Attach to be called", func() bool { return rn.callCount() == 1 })

	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach/takeover", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("takeover: code = %d, body = %s", w.Code, w.Body.String())
	}
	close(release)
	waitForAudit(t, rec, run.ID, "session.takeover", "success")
	waitForAudit(t, rec, run.ID, "session.detach", "success")

	ev := rec.snapshot()
	attach, takeover := auditIndex(ev, run.ID, "session.attach", "success", 0), auditIndex(ev, run.ID, "session.takeover", "success", 0)
	if attach < 0 || takeover < 0 || attach > takeover {
		t.Fatalf("session.takeover precedes the session.attach row it displaced: %s", auditDump(ev, run.ID))
	}
	if !strings.Contains(string(ev[attach].Data), `"state":"attaching"`) {
		t.Errorf("the first session.attach row = %s, want state attaching", ev[attach].Data)
	}
}

// The SSH lane follows the same rules: the writer opens at the pty-req size, an
// observer opens as an observer at the writer's size, a writer's window-change
// reaches the observer's PTY, and a failed attach frees the holder.
func TestSSHAttach_SameOrderAndSizes(t *testing.T) {
	rn := &scriptedRunner{}
	srv, _, run := scriptedServer(t, rn)

	ch1 := newFakeSSHChannel()
	rc1 := make(chan sshWindowChangeMsg, 1)
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		srv.bridgeSSHShell(context.Background(), run.ID, holderOwner, ch1, 120, 40, rc1)
	}()
	writerSess := rn.session(t, 0)
	if o := rn.opts(0); o != (runner.AttachOptions{Cols: 120, Rows: 40}) {
		t.Fatalf("ssh writer attach options = %+v, want 120x40 and no observer flag", o)
	}
	if n := writerSess.resizeCount(); n != 0 {
		t.Errorf("the ssh writer's exec was resized %d time(s) after registration", n)
	}

	ch2 := newFakeSSHChannel()
	rc2 := make(chan sshWindowChangeMsg, 1)
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		srv.bridgeSSHShell(context.Background(), run.ID, holderSecond, ch2, 80, 24, rc2)
	}()
	observerSess := rn.session(t, 1)
	if o := rn.opts(1); o != (runner.AttachOptions{Cols: 120, Rows: 40, Observer: true}) {
		t.Fatalf("ssh observer attach options = %+v, want the writer's 120x40 with the observer flag", o)
	}

	rc1 <- sshWindowChangeMsg{Columns: 140, Rows: 50}
	waitFor(t, "the observer's PTY to follow the ssh writer", func() bool { c, r := observerSess.lastSize(); return c == 140 && r == 50 })
	rc2 <- sshWindowChangeMsg{Columns: 20, Rows: 5}
	rc2 <- sshWindowChangeMsg{Columns: 20, Rows: 5}
	rc2 <- sshWindowChangeMsg{Columns: 20, Rows: 5}
	if c, r := observerSess.lastSize(); c != 140 || r != 50 {
		t.Errorf("the ssh observer's PTY is %dx%d after its own window-change, want it pinned to the writer's 140x50", c, r)
	}

	_ = ch1.Close()
	_ = ch2.Close()
	for _, d := range []chan struct{}{done1, done2} {
		select {
		case <-d:
		case <-time.After(5 * time.Second):
			t.Fatal("an ssh bridge never returned")
		}
	}
}

func TestSSHAttach_FailureReleasesHolder(t *testing.T) {
	rn := &scriptedRunner{attach: func(context.Context, int, runner.AttachOptions) (runner.Session, error) {
		return nil, io.ErrClosedPipe
	}}
	srv, rec, run := scriptedServer(t, rn)
	ch := newFakeSSHChannel()
	rc := make(chan sshWindowChangeMsg, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.bridgeSSHShell(context.Background(), run.ID, holderOwner, ch, 80, 24, rc)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the ssh bridge never returned after a failed attach")
	}
	if srv.attachHolderFor(run.ID) != nil {
		t.Error("a failed ssh attach left its holder registered")
	}
	if !strings.Contains(ch.stderrString(), "attach failed") {
		t.Errorf("ssh stderr = %q, want the attach failure reported", ch.stderrString())
	}
	if waitForAudit(t, rec, run.ID, "session.attach", "failure") == nil {
		t.Errorf("no failed session.attach row; events = %s", auditDump(rec.snapshot(), run.ID))
	}
}
