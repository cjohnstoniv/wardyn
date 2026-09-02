// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F5 PROBE — SSH attach -> session takeover -> PTY writer race.
//
// INTENDED DESTINATION: internal/api/attach_takeover_f5_probe_test.go
// (package api; same fakes/helpers as attach_holder_test.go: holderOwner,
// dialAttach, readAttachMode, waitFor, doSSO, ssoSession, newFakeSSHChannel,
// fakeRunner, authzStore, touchCountingStore, sshTestRecorder).
//
// RUN (from the repo root, after copying this file to the destination above):
//
//	nice -n 10 GOMAXPROCS=8 go test -race -count=1 -p 4 ./internal/api \
//	    -run 'TestF5_' -v
//
// No Postgres is needed: every store here is the package's in-memory authzStore.
// (If the coordinator wants the PG-backed sibling suites in the same run,
// export WARDYN_TEST_PG='postgres://USER:PASS@127.0.0.1:55432/DB?sslmode=disable'
// — unused by these probes.)
//
// STRICT lane: WARDYN_PROBE_STRICT=1 additionally runs the in-flight-write probe
// (TestF5_WebPump_WriteInFlightAtEvictionIsNotDelivered), which is EXPECTED RED
// on fa910735 — its red IS hypothesis H1 of the trace doc, not a harness bug.
//
// Expected verdicts on fa910735 (see F5-ssh-attach-takeover-race.md §4):
//
//	TestF5_WebPump_FrameOnTheWireBeforeEvictionIsDropped     GREEN  (pins the per-frame canWrite gate, attach.go:449)
//	TestF5_WebPump_WriteInFlightAtEvictionIsNotDelivered     RED    (H1; STRICT only)
//	TestF5_SSHPump_KeystrokesAfterEvictionDropped            GREEN  (pins sshgateway_channels.go:564)
//	TestF5_SSHPump_ResizeAfterEvictionDropped                RED    (H2: sshgateway_channels.go:588 gates on holder==nil, not canWrite)
//	TestF5_SSHDisplace_BlockedStderrDoesNotStrandEvictedPump RED    (H3: sshgateway_channels.go:470-473 writes stderr BEFORE cancel, unbounded)
//	TestF5_Takeover_SecurityAdminCannotEvictForeignHolder    RED    (H4 policy: helpers.go:311 isSecurityOperator arm reaches the takeover)
package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── fakes ─────────────────────────────────────────────────────────────────

// gatedSession is a runner.Session whose Write PARKS (after signalling
// `entered`) until the test closes `release`, and records a chunk as
// `delivered` only once Write COMPLETES. That is the whole instrument: it lets
// a test place the pump exactly between "canWrite() said yes" and "the bytes
// landed" — the window every race in this lane lives in — instead of hoping
// to win a scheduler race.
type gatedSession struct {
	r *io.PipeReader
	w *io.PipeWriter

	entered chan []byte   // one send per Write that started
	release chan struct{} // closed by the test to let parked Writes finish

	mu        sync.Mutex
	delivered [][]byte
	resizes   int
}

func newGatedSession() *gatedSession {
	r, w := io.Pipe()
	return &gatedSession{r: r, w: w, entered: make(chan []byte, 64), release: make(chan struct{})}
}

func (s *gatedSession) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s *gatedSession) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	s.entered <- cp
	<-s.release
	s.mu.Lock()
	s.delivered = append(s.delivered, cp)
	s.mu.Unlock()
	return len(p), nil
}

func (s *gatedSession) Resize(context.Context, uint16, uint16) error {
	s.mu.Lock()
	s.resizes++
	s.mu.Unlock()
	return nil
}

func (s *gatedSession) Close() error { _ = s.w.Close(); return s.r.Close() }

func (s *gatedSession) deliveredStrings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.delivered))
	for _, d := range s.delivered {
		out = append(out, string(d))
	}
	return out
}

func (s *gatedSession) resizeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resizes
}

var _ runner.Session = (*gatedSession)(nil)

// gatedRunner hands out ONE gatedSession per Attach, in order.
type gatedRunner struct {
	fakeRunner
	mu       sync.Mutex
	sessions []*gatedSession
}

func (r *gatedRunner) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	sess := newGatedSession()
	r.mu.Lock()
	r.sessions = append(r.sessions, sess)
	r.mu.Unlock()
	return sess, nil
}

func (r *gatedRunner) session(i int) *gatedSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.sessions) {
		return nil
	}
	return r.sessions[i]
}

// blockingStderrChannel is fakeSSHChannel with a stderr stream that NEVER
// completes a write until the test closes `unblock` — the shape of a real
// ssh(1) client whose channel window is exhausted (x/crypto/ssh's
// channel.WriteExtended blocks on remoteWin.reserve with no context and no
// deadline). It exists to probe the SSH displace() ordering: "print the reason,
// THEN cancel" (sshgateway_channels.go:470-473).
type blockingStderrChannel struct {
	*fakeSSHChannel
	unblock chan struct{}
}

type blockingStderr struct{ ch *blockingStderrChannel }

func (b blockingStderr) Write(p []byte) (int, error) {
	<-b.ch.unblock
	return len(p), nil
}

func (b blockingStderr) Read([]byte) (int, error) { return 0, io.EOF }

func (c *blockingStderrChannel) Stderr() io.ReadWriter { return blockingStderr{ch: c} }

var _ ssh.Channel = (*blockingStderrChannel)(nil)

// ─── harness ───────────────────────────────────────────────────────────────

// f5Server mirrors holderTestServer (attach_holder_test.go) with the gated
// runner swapped in.
func f5Server(t *testing.T) (*Server, *gatedRunner, *sshTestRecorder, types.AgentRun) {
	t.Helper()
	ast := newAuthzStore()
	st := &touchCountingStore{authzStore: ast}
	gr := &gatedRunner{}
	audit := &sshTestRecorder{}

	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.Runner = gr
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	run := types.AgentRun{ID: uuid.New(), CreatedBy: holderOwner, State: types.RunRunning, SandboxRef: "sbx-f5"}
	ast.mu.Lock()
	ast.runs[run.ID] = run
	ast.mu.Unlock()
	return srv, gr, audit, run
}

// waitEntered blocks until the gated session reports a Write has STARTED (and
// is now parked), returning the bytes it carries.
func waitEntered(t *testing.T, sess *gatedSession, what string) []byte {
	t.Helper()
	select {
	case p := <-sess.entered:
		return p
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s to enter Session.Write", what)
		return nil
	}
}

// drainClient keeps a concurrent reader on the client socket so Ping (the
// frame-order barrier) can complete; it returns when the socket errors.
func drainClient(c *websocket.Conn) {
	for {
		if _, _, err := c.Read(context.Background()); err != nil {
			return
		}
	}
}

func wsWrite(t *testing.T, c *websocket.Conn, typ websocket.MessageType, p []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, typ, p); err != nil {
		t.Fatalf("client write %q: %v", p, err)
	}
}

func wsPing(t *testing.T, c *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping barrier: %v", err)
	}
}

// ─── web lane ──────────────────────────────────────────────────────────────

// TestF5_WebPump_FrameOnTheWireBeforeEvictionIsDropped: a keystroke frame the
// displaced client DISPATCHED before the take-over was decided — already on the
// wire, not yet consumed because the pump is parked inside Session.Write of the
// previous frame — must be DROPPED once the eviction lands, not delivered when
// the pump gets to it. The socket is deliberately left OPEN (evictAttachHolder
// is called directly, exactly as TestAttachHolder_EvictionRevokesWriteAuthorityImmediately
// does) to model the worst case where displace()'s close handshake is stuck
// against an unresponsive peer: the ONLY thing standing between the frame and
// the tmux session is attach.go:449's canWrite() gate.
func TestF5_WebPump_FrameOnTheWireBeforeEvictionIsDropped(t *testing.T) {
	srv, gr, _, run := f5Server(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	c := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	if mode := readAttachMode(t, c); mode.ReadOnly {
		t.Fatal("the first client was told it is read-only")
	}
	go drainClient(c)
	waitFor(t, "the holder's session to open", func() bool { return gr.session(0) != nil })
	sess := gr.session(0)
	holder := srv.attachHolderFor(run.ID)
	if holder == nil {
		t.Fatal("no holder registered for the run")
	}

	// F1 enters Session.Write and parks there; F2 is dispatched while the pump
	// is parked, so it sits unread on the wire.
	wsWrite(t, c, websocket.MessageBinary, []byte("F1-before"))
	waitEntered(t, sess, "F1")
	wsWrite(t, c, websocket.MessageBinary, []byte("F2-on-the-wire"))

	// The take-over is DECIDED here: registry entry gone, evicted=true, socket
	// still open (worst-case displace).
	if got := srv.evictAttachHolder(run.ID); got != holder {
		t.Fatalf("evict returned %v, want the registered holder", got)
	}
	if holder.canWrite() {
		t.Fatal("evicted holder still reports canWrite()")
	}

	// Unpark F1; the pump now reads F2 and must drop it. F3 is dispatched
	// strictly after the eviction and must be dropped too. The Ping is the
	// frame-order barrier: the pump answers it from the same read loop, so a
	// completed Ping proves F2 and F3 were already consumed.
	close(sess.release)
	wsWrite(t, c, websocket.MessageBinary, []byte("F3-after"))
	wsPing(t, c)

	got := sess.deliveredStrings()
	for _, d := range got {
		if d == "F2-on-the-wire" || d == "F3-after" {
			t.Fatalf("evicted holder's keystrokes reached the sandbox: delivered=%q (only the pre-eviction in-flight F1 may land)", got)
		}
	}
	if len(got) != 1 || got[0] != "F1-before" {
		t.Errorf("delivered=%q, want exactly [F1-before] (F1 was already inside Session.Write when the eviction landed — see H1 for that residual)", got)
	}
	// A resize from the evicted holder must be dropped as well (attach.go:438).
	before := sess.resizeCount()
	wsWrite(t, c, websocket.MessageText, []byte(`{"type":"resize","cols":10,"rows":3}`))
	wsPing(t, c)
	if sess.resizeCount() != before {
		t.Errorf("evicted web holder resized the shared tmux session")
	}
}

// TestF5_WebPump_WriteInFlightAtEvictionIsNotDelivered (STRICT, expected RED on
// fa910735 = H1): a frame that passed canWrite() and is INSIDE Session.Write
// when the take-over is decided still lands in the tmux session afterwards. The
// frame can be up to attachReadLimit (1 MiB, attach.go:51) — one paste — and it
// arrives AFTER session.takeover was audited as done. A pass requires the pump
// to re-check eviction per chunk (or to route writes through an
// eviction-aware writer) rather than once per frame.
func TestF5_WebPump_WriteInFlightAtEvictionIsNotDelivered(t *testing.T) {
	if os.Getenv("WARDYN_PROBE_STRICT") == "" {
		t.Skip("STRICT probe: set WARDYN_PROBE_STRICT=1 to run (expected RED on fa910735; documents H1)")
	}
	srv, gr, _, run := f5Server(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	c := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	readAttachMode(t, c)
	go drainClient(c)
	waitFor(t, "the holder's session to open", func() bool { return gr.session(0) != nil })
	sess := gr.session(0)

	wsWrite(t, c, websocket.MessageBinary, []byte("in-flight"))
	waitEntered(t, sess, "the in-flight frame")
	if srv.evictAttachHolder(run.ID) == nil {
		t.Fatal("nothing to evict")
	}
	close(sess.release)
	wsPing(t, c)

	if got := sess.deliveredStrings(); len(got) != 0 {
		t.Errorf("a write in flight at eviction was DELIVERED after the take-over was decided and audited: %q (H1: canWrite is checked once per frame, before Session.Write — attach.go:449-450)", got)
	}
}

// ─── SSH lane ──────────────────────────────────────────────────────────────

// startSSHShell drives bridgeSSHShell directly (the pattern
// TestSSHAttachHolder_RegistersAndIsDisplaced uses) and returns the bridge's
// done channel.
func startSSHShell(t *testing.T, srv *Server, run types.AgentRun, ch ssh.Channel, resizeCh chan sshWindowChangeMsg) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.bridgeSSHShell(context.Background(), run.ID, holderOwner, ch, 80, 24, resizeCh)
	}()
	waitFor(t, "the ssh holder to register", func() bool { return srv.attachHolderFor(run.ID) != nil })
	return done
}

// TestF5_SSHPump_KeystrokesAfterEvictionDropped: the SSH twin of the web probe.
// The fake channel is an io.Pipe, so clientSend returns only once the pump has
// READ the bytes, and the NEXT clientSend returns only once the pump has looped
// back to Read — i.e. finished deciding what to do with the previous chunk.
func TestF5_SSHPump_KeystrokesAfterEvictionDropped(t *testing.T) {
	srv, gr, _, run := f5Server(t)
	ch := newFakeSSHChannel()
	resizeCh := make(chan sshWindowChangeMsg, 1)
	done := startSSHShell(t, srv, run, ch, resizeCh)
	waitFor(t, "the session to open", func() bool { return gr.session(0) != nil })
	sess := gr.session(0)
	holder := srv.attachHolderFor(run.ID)

	ch.clientSend("K1-before")
	waitEntered(t, sess, "K1")
	if got := srv.evictAttachHolder(run.ID); got != holder {
		t.Fatalf("evict returned %v, want the ssh holder", got)
	}
	close(sess.release)

	ch.clientSend("K2-after")
	ch.clientSend("barrier") // returns only once K2's iteration completed
	got := sess.deliveredStrings()
	for _, d := range got {
		if d == "K2-after" || d == "barrier" {
			t.Fatalf("evicted ssh holder's keystrokes reached the sandbox: %q", got)
		}
	}

	_ = ch.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the ssh bridge never returned after the channel closed")
	}
}

// TestF5_SSHPump_ResizeAfterEvictionDropped (expected RED on fa910735 = H2):
// sshShellPump's resize goroutine gates on `holder == nil`
// (sshgateway_channels.go:588), NOT holder.canWrite(), so an EVICTED ssh
// holder keeps resizing the shared tmux window under the new holder until its
// channel actually dies. The web lane gates the same frame on canWrite()
// (attach.go:438); this is the asymmetry.
func TestF5_SSHPump_ResizeAfterEvictionDropped(t *testing.T) {
	srv, gr, _, run := f5Server(t)
	ch := newFakeSSHChannel()
	resizeCh := make(chan sshWindowChangeMsg, 1)
	done := startSSHShell(t, srv, run, ch, resizeCh)
	waitFor(t, "the session to open", func() bool { return gr.session(0) != nil })
	sess := gr.session(0)
	close(sess.release) // keystrokes are not the subject here

	// bridgeSSHShell already applied the pty-req geometry once as the writer
	// (sshgateway_channels.go:477-481), so a live window-change makes it two.
	resizeCh <- sshWindowChangeMsg{Columns: 120, Rows: 40}
	waitFor(t, "the pre-eviction window-change", func() bool { return sess.resizeCount() == 2 })
	before := sess.resizeCount()

	if srv.evictAttachHolder(run.ID) == nil {
		t.Fatal("nothing to evict")
	}
	// 1-slot channel: the Nth send returns once the (N-1)th was dequeued, so
	// three sends prove the first post-eviction window-change ran to completion.
	for range 3 {
		resizeCh <- sshWindowChangeMsg{Columns: 20, Rows: 5}
	}
	if n := sess.resizeCount(); n != before {
		t.Errorf("evicted ssh holder resized the SHARED tmux session %d time(s) after the take-over (sshgateway_channels.go:588 checks holder==nil, not canWrite())", n-before)
	}

	_ = ch.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the ssh bridge never returned after the channel closed")
	}
}

// TestF5_SSHDisplace_BlockedStderrDoesNotStrandEvictedPump (expected RED on
// fa910735 = H3): the SSH displace() prints the reason on the channel's stderr
// and only THEN cancels the pump (sshgateway_channels.go:470-473), on a
// goroutine with no deadline. A displaced client whose channel window is
// exhausted (stopped reading — a suspended ssh(1)) parks that goroutine forever:
// cancel() never runs, the evicted pump keeps its exec (a tmux client on the
// shared session), its TouchRun keepalive (the idle reaper never fires) and one
// of the run's four channel slots until the TCP connection dies. Write
// authority IS revoked (evicted=true) — this probe is about the stranded
// session, not a second writer.
func TestF5_SSHDisplace_BlockedStderrDoesNotStrandEvictedPump(t *testing.T) {
	srv, gr, audit, run := f5Server(t)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	ch := &blockingStderrChannel{fakeSSHChannel: newFakeSSHChannel(), unblock: make(chan struct{})}
	defer close(ch.unblock)
	resizeCh := make(chan sshWindowChangeMsg, 1)
	done := startSSHShell(t, srv, run, ch, resizeCh)
	waitFor(t, "the session to open", func() bool { return gr.session(0) != nil })
	close(gr.session(0).release)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach/takeover", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("takeover: code = %d, body = %s", w.Code, w.Body.String())
	}
	if ev := findAudit(audit.snapshot(), run.ID, "session.takeover", "success"); ev == nil {
		t.Fatal("take-over was not audited")
	}
	if srv.attachHolderFor(run.ID) != nil {
		t.Fatal("holder still registered after the take-over")
	}

	// The take-over is decided and audited. The displaced session must END
	// within a bound even though its client never drains stderr.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Error("the displaced ssh pump is still running 3s after the take-over: displace()'s stderr write never completed, so cancel() never ran (sshgateway_channels.go:470-473) — exec, keepalive and channel slot are stranded until the TCP connection dies")
		_ = ch.Close()
		<-done
	}
}

// ─── takeover authorization ────────────────────────────────────────────────

// TestF5_Takeover_SecurityAdminCannotEvictForeignHolder (POLICY probe, expected
// RED on fa910735 = H4): handleAttachTakeover gates on getRunAuthorized ->
// ownsRunOrAdmin (helpers.go:311), whose 0.7 arm is isSecurityOperator — so a
// security_admin, who can neither mint an attach ticket for a foreign run
// (attach_ticket.go:143) nor SSH into it (sshkeys.go:112-114 stamps member),
// CAN evict its live holder. The three-tier doctrine says the security tier
// never reaches INTO a run; ending another human's terminal is a kick, not a
// read. The owner decides whether this is intended; the probe pins the
// current answer either way.
func TestF5_Takeover_SecurityAdminCannotEvictForeignHolder(t *testing.T) {
	srv, gr, audit, run := f5Server(t)
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)

	ch := newFakeSSHChannel()
	resizeCh := make(chan sshWindowChangeMsg, 1)
	done := startSSHShell(t, srv, run, ch, resizeCh)
	waitFor(t, "the session to open", func() bool { return gr.session(0) != nil })
	close(gr.session(0).release)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach/takeover", sec, "")
	if w.Code == http.StatusOK {
		t.Errorf("a security_admin (not owner, not super admin) evicted the run owner's live terminal: code=200 body=%s (H4: helpers.go:311 isSecurityOperator arm reaches POST /attach/takeover)", w.Body.String())
	}
	if ev := findAudit(audit.snapshot(), run.ID, "session.takeover", "success"); ev != nil && w.Code != http.StatusOK {
		t.Errorf("takeover refused but audited as success: %s", ev.Data)
	}

	_ = ch.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the ssh bridge never returned after the channel closed")
	}
}
