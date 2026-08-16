// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// countingShellSession is a runner.Session that RECORDS what was written to it
// (keystrokes) and how often it was resized, and never echoes. That is the
// whole point: the read-only contract is a NEGATIVE — an observer's input must
// not reach the sandbox — so the test needs a session that can prove nothing
// arrived, plus feed() to push PTY output the other way and prove an observer
// still streams it.
type countingShellSession struct {
	r *io.PipeReader
	w *io.PipeWriter

	mu      sync.Mutex
	writes  [][]byte
	resizes int
}

func newCountingShellSession() *countingShellSession {
	r, w := io.Pipe()
	return &countingShellSession{r: r, w: w}
}

func (s *countingShellSession) Read(p []byte) (int, error) { return s.r.Read(p) }

func (s *countingShellSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.writes = append(s.writes, append([]byte(nil), p...))
	s.mu.Unlock()
	return len(p), nil
}

func (s *countingShellSession) Resize(context.Context, uint16, uint16) error {
	s.mu.Lock()
	s.resizes++
	s.mu.Unlock()
	return nil
}

func (s *countingShellSession) Close() error { _ = s.w.Close(); return s.r.Close() }

// feed pushes PTY output toward the client (blocks until the pump reads it).
func (s *countingShellSession) feed(b string) { _, _ = io.WriteString(s.w, b) }

func (s *countingShellSession) written() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func (s *countingShellSession) resizeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resizes
}

var _ runner.Session = (*countingShellSession)(nil)

// holderTestRunner hands out a fresh countingShellSession per Attach and keeps
// them in order, so a test can inspect "the second client's session". Everything
// that is not Attach comes from the package's existing fakeRunner.
type holderTestRunner struct {
	fakeRunner
	mu       sync.Mutex
	sessions []*countingShellSession
}

func (r *holderTestRunner) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	sess := newCountingShellSession()
	r.mu.Lock()
	r.sessions = append(r.sessions, sess)
	r.mu.Unlock()
	return sess, nil
}

func (r *holderTestRunner) session(i int) *countingShellSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i >= len(r.sessions) {
		return nil
	}
	return r.sessions[i]
}

// touchCountingStore counts TouchRun calls. Both pumps TouchRun on EVERY client
// frame — including a read-only observer's dropped ones — which makes the count
// a deterministic barrier for "the server has consumed the frame I sent",
// turning the read-only assertion from a sleep-and-hope into a real ordering.
type touchCountingStore struct {
	*authzStore
	mu sync.Mutex
	n  int
}

func (s *touchCountingStore) TouchRun(context.Context, uuid.UUID) error {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return nil
}

func (s *touchCountingStore) touches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// fakeSSHChannel is the minimum ssh.Channel a bridgeSSHShell test needs: a pipe
// for client->server bytes, buffers for the two server->client streams. It
// exists so the SSH lane's holder registration can be driven directly, without
// standing up the whole gateway (whose harness does not expose its *Server).
type fakeSSHChannel struct {
	in  *io.PipeReader
	inW *io.PipeWriter

	mu     sync.Mutex
	out    bytes.Buffer
	errOut bytes.Buffer

	closeOnce sync.Once
}

func newFakeSSHChannel() *fakeSSHChannel {
	r, w := io.Pipe()
	return &fakeSSHChannel{in: r, inW: w}
}

func (c *fakeSSHChannel) Read(p []byte) (int, error) { return c.in.Read(p) }

func (c *fakeSSHChannel) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Write(p)
}

func (c *fakeSSHChannel) Close() error {
	c.closeOnce.Do(func() {
		_ = c.inW.Close()
		_ = c.in.Close()
	})
	return nil
}

func (c *fakeSSHChannel) CloseWrite() error { return nil }

func (c *fakeSSHChannel) SendRequest(string, bool, []byte) (bool, error) { return true, nil }

func (c *fakeSSHChannel) Stderr() io.ReadWriter { return (*fakeSSHChannelErr)(c) }

// clientSend delivers keystrokes as if typed by the ssh client.
func (c *fakeSSHChannel) clientSend(s string) { _, _ = io.WriteString(c.inW, s) }

func (c *fakeSSHChannel) stderrString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errOut.String()
}

// fakeSSHChannelErr is the channel's extended-data (stderr) stream, sharing the
// parent's mutex so the gateway writing it never races the test reading it.
type fakeSSHChannelErr fakeSSHChannel

func (e *fakeSSHChannelErr) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.errOut.Write(p)
}

func (e *fakeSSHChannelErr) Read(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.errOut.Read(p)
}

var _ ssh.Channel = (*fakeSSHChannel)(nil)

// ─── harness ───────────────────────────────────────────────────────────────

// holderTestServer builds a Server wired for attach: a RUNNING run owned by
// alice, a runner that hands out inspectable sessions, SSO cookie auth (so
// admin/member callers are distinguishable), and a THREAD-SAFE audit recorder —
// the attach pumps record from their own goroutines while the test polls, which
// the package's plain recRecorder would race under -race.
func holderTestServer(t *testing.T) (*Server, *touchCountingStore, *holderTestRunner, *sshTestRecorder, types.AgentRun) {
	t.Helper()
	ast := newAuthzStore()
	st := &touchCountingStore{authzStore: ast}
	fr := &holderTestRunner{}
	audit := &sshTestRecorder{}

	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.Runner = fr
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	run := types.AgentRun{ID: uuid.New(), CreatedBy: holderOwner, State: types.RunRunning, SandboxRef: "sbx-1"}
	ast.mu.Lock()
	ast.runs[run.ID] = run
	ast.mu.Unlock()
	return srv, st, fr, audit, run
}

const (
	holderOwner  = "alice@example.com"
	holderSecond = "bob@example.com"
)

// waitFor polls cond until it holds, failing the test after a bounded wait. The
// attach handlers do their work on their own goroutines, so every assertion
// about registry state is inherently "eventually".
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// dialAttach opens the attach WebSocket the way the browser does: a
// single-use ?ticket= minted for principal (the lane that must keep working).
func dialAttach(t *testing.T, ts *httptest.Server, srv *Server, runID uuid.UUID, principal, query string) *websocket.Conn {
	t.Helper()
	tok, err := mintAttachTicket(context.Background(), srv.cfg.Store, runID, types.ActorHuman, principal, oidc.RoleAdmin, time.Now())
	if err != nil {
		t.Fatalf("mint ticket: %v", err)
	}
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/runs/" + runID.String() + "/attach?ticket=" + tok + query
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial attach (%s): %v", principal, err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// readAttachMode reads the attach-mode control frame the server sends on open.
// It is a TEXT frame and it is the FIRST frame, always — the handler writes it
// before the pump starts, so no PTY output can overtake it.
func readAttachMode(t *testing.T, c *websocket.Conn) attachModeMsg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read attach-mode frame: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("first frame type = %v, want text (the attach-mode control frame)", typ)
	}
	var msg attachModeMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("decode attach-mode frame %q: %v", data, err)
	}
	if msg.Type != "attach-mode" {
		t.Fatalf("control frame type = %q, want attach-mode", msg.Type)
	}
	return msg
}

func getHolder(t *testing.T, srv *Server, runID uuid.UUID, cookie *http.Cookie) attachHolderView {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+runID.String()+"/attach-holder", cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET attach-holder: code = %d, body = %s", w.Code, w.Body.String())
	}
	var v attachHolderView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode attach-holder body %q: %v", w.Body.String(), err)
	}
	return v
}

// ─── registry unit tests ───────────────────────────────────────────────────

// TestAttachHolderRegistry_RoundTrip is the core contract: one writer at a
// time, a second registration is admitted READ-ONLY (and registers nothing), an
// observer's release cannot evict the holder, and the holder's own release
// leaves no phantom behind.
func TestAttachHolderRegistry_RoundTrip(t *testing.T) {
	srv := New(Config{Audit: &recRecorder{}, AdminToken: adminToken})
	runID := uuid.New()

	if got := srv.attachHolderFor(runID); got != nil {
		t.Fatalf("fresh registry already holds %v", got)
	}
	if v := srv.attachHolderFor(runID).view(); v.Held {
		t.Fatal("nil holder must view as held:false")
	}

	first := &attachHolder{principal: holderOwner, source: attachSourceWeb, since: time.Unix(1, 0).UTC(), cols: 80, rows: 24}
	readOnly, releaseFirst := srv.registerAttachHolder(runID, first)
	if readOnly {
		t.Fatal("first attach was admitted read-only; nobody held the PTY")
	}

	second := &attachHolder{principal: holderSecond, source: attachSourceSSH, since: time.Unix(2, 0).UTC()}
	readOnly, releaseSecond := srv.registerAttachHolder(runID, second)
	if !readOnly {
		t.Fatal("second attach was admitted as a writer; two clients would fight over one tmux session")
	}
	if got := srv.attachHolderFor(runID); got != first {
		t.Fatalf("holder changed under a second attach: %+v", got)
	}

	// Live geometry: the handshake value is stale after the first resize.
	first.setSize(132, 50)
	v := srv.attachHolderFor(runID).view()
	if !v.Held || v.Principal != holderOwner || v.Source != attachSourceWeb || v.Cols != 132 || v.Rows != 50 {
		t.Fatalf("holder view = %+v, want held web holder %s at 132x50", v, holderOwner)
	}
	if v.Since == nil || !v.Since.Equal(first.since) {
		t.Fatalf("holder view since = %v, want %v", v.Since, first.since)
	}

	// The OBSERVER detaching must not evict the writer.
	releaseSecond()
	if got := srv.attachHolderFor(runID); got != first {
		t.Fatalf("an observer's release evicted the holder: %+v", got)
	}

	// The writer detaching leaves nothing behind.
	releaseFirst()
	if got := srv.attachHolderFor(runID); got != nil {
		t.Fatalf("phantom holder after release: %+v", got)
	}
	releaseFirst() // idempotent: the deferred release must be safe twice
}

// TestAttachHolderRegistry_ReleaseNeverEvictsSuccessor: after a take-over
// evicts a holder and a fresh client claims the run, the DISPLACED handler's
// A displaced holder must lose WRITE AUTHORITY at eviction, not whenever its
// socket finishes dying.
//
// This is the regression test for the one real hole a review found in this
// file. The pumps gate writes on the *attachHolder they captured at attach
// time; evictAttachHolder only removed the map entry, and displace() closes the
// socket rather than cancelling the pump (deliberately — a cancelled context
// sends no close frame). coder/websocket's Close does a full handshake whose
// second half blocks on the read mutex the displaced pump holds, so between
// "take-over returned 200" and "the old socket actually died" the OLD client
// could still write into the same tmux session as the new one. Two writers is
// precisely the state this file exists to prevent.
//
// Asserting on canWrite() rather than on the close status is the point: the
// existing take-over test already checks the close, and it passed throughout.
func TestAttachHolder_EvictionRevokesWriteAuthorityImmediately(t *testing.T) {
	srv := New(Config{Audit: &recRecorder{}, AdminToken: adminToken})
	runID := uuid.New()

	holder := &attachHolder{principal: holderOwner, source: attachSourceWeb}
	readOnly, release := srv.registerAttachHolder(runID, holder)
	if readOnly {
		t.Fatal("first client should be the writer")
	}
	if !holder.canWrite() {
		t.Fatal("the registered holder must be able to write before any take-over")
	}

	// The take-over path, with NO socket teardown at all — modelling the worst
	// case the review described: a peer that never echoes the close, leaving
	// displace() blocked for its full multi-second budget.
	if got := srv.evictAttachHolder(runID); got != holder {
		t.Fatalf("evict returned %v, want the registered holder", got)
	}

	if holder.canWrite() {
		t.Error("displaced holder can still write after eviction — two clients " +
			"would be driving the same tmux session while the old socket closes")
	}
	// A read-only observer (nil holder) must also be refused, and the same
	// helper has to answer both questions or the pumps grow two gates.
	var observer *attachHolder
	if observer.canWrite() {
		t.Error("a nil holder (read-only observer) must never write")
	}
	// Releasing a holder that was already displaced must not disturb the map.
	release()
	if h := srv.attachHolderFor(runID); h != nil {
		t.Errorf("after eviction the run should have no holder, got %+v", h)
	}
}

// deferred release finally runs. It must not delete its successor — that would
// silently hand the run back to "nobody attached" while a human is typing.
func TestAttachHolderRegistry_ReleaseNeverEvictsSuccessor(t *testing.T) {
	srv := New(Config{Audit: &recRecorder{}, AdminToken: adminToken})
	runID := uuid.New()

	displaced := &attachHolder{principal: holderOwner, source: attachSourceWeb}
	_, releaseDisplaced := srv.registerAttachHolder(runID, displaced)

	if got := srv.evictAttachHolder(runID); got != displaced {
		t.Fatalf("evict returned %+v, want the registered holder", got)
	}
	if got := srv.evictAttachHolder(runID); got != nil {
		t.Fatal("evict returned a holder twice; two concurrent take-overs would both audit")
	}

	successor := &attachHolder{principal: holderSecond, source: attachSourceWeb}
	if readOnly, _ := srv.registerAttachHolder(runID, successor); readOnly {
		t.Fatal("the post-take-over attach was admitted read-only; the eviction did not free the run")
	}

	releaseDisplaced()
	if got := srv.attachHolderFor(runID); got != successor {
		t.Fatalf("the displaced holder's release evicted its successor: %+v", got)
	}
}

// TestAttachHolderRegistry_ConcurrentRaceFree hammers the registry from many
// goroutines (run with -race): one daemon serves many runs at once, and the
// live geometry is written by a pump goroutine while an HTTP reader renders it.
func TestAttachHolderRegistry_ConcurrentRaceFree(t *testing.T) {
	srv := New(Config{Audit: &recRecorder{}, AdminToken: adminToken})
	runs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runID := runs[i%len(runs)]
			for n := 0; n < 200; n++ {
				h := &attachHolder{principal: holderOwner, source: attachSourceWeb, since: time.Now()}
				_, release := srv.registerAttachHolder(runID, h)
				h.setSize(uint16(n%200), uint16(n%50)) //nolint:gosec // bounded by the modulus
				_ = srv.attachHolderFor(runID).view()
				if n%17 == 0 {
					_ = srv.evictAttachHolder(runID)
				}
				release()
			}
		}(i)
	}
	wg.Wait()

	for _, runID := range runs {
		if got := srv.attachHolderFor(runID); got != nil {
			t.Fatalf("run %s left a phantom holder after every client released: %+v", runID, got)
		}
	}
}

// TestAttachTakeoverReason pins the close-frame reason the UI matches on, and
// its RFC 6455 byte cap: coder/websocket refuses to send an over-long reason at
// all, so an unbounded principal would cost the displaced client the very
// signal that tells it not to reconnect.
func TestAttachTakeoverReason(t *testing.T) {
	if got := attachTakeoverReason(holderOwner); got != "taken over by "+holderOwner {
		t.Fatalf("reason = %q", got)
	}
	long := attachTakeoverReason(strings.Repeat("é", 200))
	if len(long) > wsCloseReasonMax {
		t.Fatalf("reason is %d bytes, over the %d-byte close-frame cap", len(long), wsCloseReasonMax)
	}
	if !strings.HasPrefix(long, attachTakeoverReasonPrefix) {
		t.Fatalf("truncation ate the prefix the UI matches on: %q", long)
	}
	// Truncation must land on a rune boundary — invalid UTF-8 in a close frame
	// is a client's right to reject.
	if !json.Valid([]byte(`"` + long + `"`)) {
		t.Fatalf("truncated reason is not valid UTF-8: %q", long)
	}
}

// ─── endpoint tests ────────────────────────────────────────────────────────

// TestAttachHolderEndpoints_ForeignRun404: both endpoints are owner-or-admin,
// and a non-owning member gets the byte-identical 404 a missing run would — no
// existence oracle, and no "is anyone watching?" oracle over someone else's run.
func TestAttachHolderEndpoints_ForeignRun404(t *testing.T) {
	srv, _, _, _, run := holderTestServer(t)
	stranger := ssoSession(t, "sub-mallory", "mallory@corp.example", oidc.RoleMember)
	owner := ssoSession(t, holderOwner, holderOwner, oidc.RoleMember)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/runs/" + run.ID.String() + "/attach-holder"},
		{http.MethodPost, "/api/v1/runs/" + run.ID.String() + "/attach/takeover"},
	} {
		w := doSSO(t, srv, tc.method, tc.path, stranger, "")
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "run not found") {
			t.Errorf("%s %s as a non-owner: code = %d body = %s, want 404 run not found",
				tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	// The owner reaches the handler itself (held:false, since nobody attached).
	if v := getHolder(t, srv, run.ID, owner); v.Held {
		t.Errorf("attach-holder = %+v, want held:false for an unattached run", v)
	}
}

// TestAttachTakeover_NoHolderIsRejected: taking over nothing is a client bug,
// and a 200 there would teach the UI that "take over" always works.
func TestAttachTakeover_NoHolderIsRejected(t *testing.T) {
	srv, _, _, audit, run := holderTestServer(t)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach/takeover", admin, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("takeover with no holder: code = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if ev := findAudit(audit.snapshot(), run.ID, "session.takeover", "success"); ev != nil {
		t.Error("a take-over that displaced nobody was audited as a success")
	}
}

// TestAttachWS_SecondClientReadOnlyThenTakeover is the whole lane end to end
// over a REAL WebSocket: holder registration, the attach-mode control frame,
// live geometry from resize frames, a second client admitted read-only with its
// input dropped SERVER-side (while still streaming output), and an audited
// take-over that closes the displaced socket with a reason the UI can read.
func TestAttachWS_SecondClientReadOnlyThenTakeover(t *testing.T) {
	srv, st, fr, audit, run := holderTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	owner := ssoSession(t, holderOwner, holderOwner, oidc.RoleMember)

	// ── first client: the holder ──
	c1 := dialAttach(t, ts, srv, run.ID, holderOwner, "&cols=80&rows=24")
	mode1 := readAttachMode(t, c1)
	if mode1.ReadOnly {
		t.Fatal("the FIRST client was told it is read-only")
	}
	if mode1.Holder == nil || mode1.Holder.Principal != holderOwner || mode1.Holder.Source != attachSourceWeb {
		t.Fatalf("attach-mode holder = %+v, want the web holder %s", mode1.Holder, holderOwner)
	}
	waitFor(t, "the holder to register", func() bool { return srv.attachHolderFor(run.ID) != nil })

	if v := getHolder(t, srv, run.ID, owner); !v.Held || v.Principal != holderOwner || v.Source != attachSourceWeb || v.Cols != 80 {
		t.Fatalf("GET attach-holder = %+v, want the web holder %s at 80 cols", v, holderOwner)
	}

	// Live geometry: a resize control frame must move the registry, or the
	// browser renders a size the operator abandoned ten minutes ago.
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wcancel()
	if err := c1.Write(wctx, websocket.MessageText, []byte(`{"type":"resize","cols":132,"rows":50}`)); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	waitFor(t, "the resize to reach the registry", func() bool {
		v := srv.attachHolderFor(run.ID).view()
		return v.Cols == 132 && v.Rows == 50
	})

	// The holder's keystrokes DO reach the sandbox (the control for the negative
	// asserted below).
	if err := c1.Write(wctx, websocket.MessageBinary, []byte("echo hi\r")); err != nil {
		t.Fatalf("write keystrokes: %v", err)
	}
	waitFor(t, "the holder's keystrokes to reach the session", func() bool { return fr.session(0).written() > 0 })

	// ── second client: read-only ──
	c2 := dialAttach(t, ts, srv, run.ID, holderSecond, "")
	mode2 := readAttachMode(t, c2)
	if !mode2.ReadOnly {
		t.Fatal("a SECOND client was admitted writable; two clients now fight over one tmux session")
	}
	if mode2.Holder == nil || mode2.Holder.Principal != holderOwner {
		t.Fatalf("read-only client was not told who holds the PTY: %+v", mode2.Holder)
	}
	if got := srv.attachHolderFor(run.ID).view().Principal; got != holderOwner {
		t.Fatalf("holder changed to %q when the observer connected", got)
	}
	waitFor(t, "the observer's session to open", func() bool { return fr.session(1) != nil })
	observed := fr.session(1)

	// Its input is dropped SERVER-side. The touch counter is the barrier: both
	// frames are consumed by the pump (which TouchRuns on every client frame)
	// before we assert nothing reached the session.
	before := st.touches()
	if err := c2.Write(wctx, websocket.MessageBinary, []byte("rm -rf /\r")); err != nil {
		t.Fatalf("observer write: %v", err)
	}
	if err := c2.Write(wctx, websocket.MessageText, []byte(`{"type":"resize","cols":20,"rows":5}`)); err != nil {
		t.Fatalf("observer resize: %v", err)
	}
	waitFor(t, "the observer's frames to be consumed", func() bool { return st.touches() >= before+2 })
	if n := observed.written(); n != 0 {
		t.Errorf("observer's keystrokes reached the sandbox (%d writes) — read-only is not enforced server-side", n)
	}
	if n := observed.resizeCount(); n != 0 {
		t.Errorf("observer resized the SHARED tmux session (%d resizes) — that clamps the holder's terminal", n)
	}
	if v := srv.attachHolderFor(run.ID).view(); v.Cols != 132 || v.Rows != 50 {
		t.Errorf("observer's resize overwrote the holder's geometry: %+v", v)
	}

	// ...but it DOES stream output: read-only is not blind.
	go observed.feed("hello from the pty")
	rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer rcancel()
	typ, data, err := c2.Read(rctx)
	if err != nil {
		t.Fatalf("observer read: %v", err)
	}
	if typ != websocket.MessageBinary || !strings.Contains(string(data), "hello from the pty") {
		t.Errorf("observer got %v %q, want the PTY output streamed", typ, data)
	}

	// ── take-over ──
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach/takeover", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("takeover: code = %d, body = %s", w.Code, w.Body.String())
	}
	var body struct {
		TakenOver      bool   `json:"taken_over"`
		PreviousHolder string `json:"previous_holder"`
		PreviousSource string `json:"previous_source"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode takeover body %q: %v", w.Body.String(), err)
	}
	if !body.TakenOver || body.PreviousHolder != holderOwner || body.PreviousSource != attachSourceWeb {
		t.Fatalf("takeover body = %+v, want the displaced web holder %s", body, holderOwner)
	}

	// The displaced socket is closed with a reason the client can READ — this is
	// how the UI tells a take-over from a network blip and declines to reconnect.
	_, _, err = c1.Read(rctx)
	if err == nil {
		t.Fatal("the displaced client's socket stayed open")
	}
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Fatalf("displaced close status = %v, want %v (1008)", got, websocket.StatusPolicyViolation)
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) || !strings.HasPrefix(ce.Reason, attachTakeoverReasonPrefix) {
		t.Fatalf("displaced close reason = %q, want the %q prefix the UI matches on", ce.Reason, attachTakeoverReasonPrefix)
	}

	// Audited, with the previous holder NAMED (taking a live terminal from
	// another human is exactly the act that must be attributable).
	ev := waitForAudit(t, audit, run.ID, "session.takeover", "success")
	if ev == nil {
		t.Fatalf("no session.takeover audit; events = %s", auditDump(audit.snapshot(), run.ID))
	}
	if !strings.Contains(string(ev.Data), `"previous_holder":"`+holderOwner+`"`) {
		t.Errorf("session.takeover data = %s, want previous_holder %s", ev.Data, holderOwner)
	}
	if ev.Actor == "" || ev.Actor == holderOwner {
		t.Errorf("session.takeover actor = %q, want the taking-over admin", ev.Actor)
	}

	// No phantom: once the displaced pump ends, the run is free for the next
	// attach to claim as a WRITER.
	waitFor(t, "the displaced holder to unregister", func() bool { return srv.attachHolderFor(run.ID) == nil })
	if v := getHolder(t, srv, run.ID, owner); v.Held {
		t.Errorf("attach-holder = %+v after the displaced client left, want held:false", v)
	}
	if ev := waitForAudit(t, audit, run.ID, "session.detach", "success"); ev == nil {
		t.Error("the displaced session emitted no session.detach")
	}
}

// ─── SSH lane ──────────────────────────────────────────────────────────────

// TestSSHAttachHolder_RegistersAndIsDisplaced: an SSH-gateway PTY is the SAME
// shared tmux session, so it registers in the SAME registry with source "ssh".
// Without this the browser confidently reports "nobody is attached" while a CLI
// holder is typing — and the run page silently starts competing for the PTY.
func TestSSHAttachHolder_RegistersAndIsDisplaced(t *testing.T) {
	srv, _, fr, audit, run := holderTestServer(t)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	ch := newFakeSSHChannel()
	resizeCh := make(chan sshWindowChangeMsg, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.bridgeSSHShell(context.Background(), run.ID, holderOwner, ch, 80, 24, resizeCh)
	}()

	waitFor(t, "the ssh holder to register", func() bool { return srv.attachHolderFor(run.ID) != nil })
	if v := srv.attachHolderFor(run.ID).view(); v.Source != attachSourceSSH || v.Principal != holderOwner || v.Cols != 80 {
		t.Fatalf("holder view = %+v, want the ssh holder %s at 80 cols", v, holderOwner)
	}

	// window-change keeps the registry's geometry live, same as the web resize.
	resizeCh <- sshWindowChangeMsg{Columns: 200, Rows: 60}
	waitFor(t, "window-change to reach the registry", func() bool {
		v := srv.attachHolderFor(run.ID).view()
		return v.Cols == 200 && v.Rows == 60
	})
	waitFor(t, "the session to be resized", func() bool { return fr.session(0).resizeCount() > 0 })

	// The CLI holder is visible to the browser's endpoint...
	if v := getHolder(t, srv, run.ID, admin); !v.Held || v.Source != attachSourceSSH {
		t.Fatalf("GET attach-holder = %+v, want the ssh holder", v)
	}
	// ...and displaceable from it, with the reason printed on the ssh client's
	// own stderr (its equivalent of the WebSocket close frame).
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/attach/takeover", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("takeover: code = %d, body = %s", w.Code, w.Body.String())
	}
	waitFor(t, "the displaced ssh client to be told why", func() bool {
		return strings.Contains(ch.stderrString(), attachTakeoverReasonPrefix)
	})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the displaced ssh shell bridge never returned")
	}
	if got := srv.attachHolderFor(run.ID); got != nil {
		t.Fatalf("phantom ssh holder after the bridge returned: %+v", got)
	}
	if ev := findAudit(audit.snapshot(), run.ID, "session.takeover", "success"); ev == nil {
		t.Error("the ssh take-over was not audited")
	}
}

// TestSSHAttachHolder_SecondIsReadOnly: an SSH client arriving while the web
// terminal holds the PTY gets the same treatment the browser does — output
// streams, keystrokes are dropped server-side, and the shared tmux window is
// never resized under the holder.
func TestSSHAttachHolder_SecondIsReadOnly(t *testing.T) {
	srv, st, fr, _, run := holderTestServer(t)

	// A web client already holds it.
	held := &attachHolder{principal: holderOwner, source: attachSourceWeb, since: time.Now(), cols: 100, rows: 40, displace: func(string) {}}
	if readOnly, release := srv.registerAttachHolder(run.ID, held); readOnly {
		t.Fatal("could not seed the web holder")
	} else {
		defer release()
	}

	ch := newFakeSSHChannel()
	resizeCh := make(chan sshWindowChangeMsg, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.bridgeSSHShell(context.Background(), run.ID, holderSecond, ch, 80, 24, resizeCh)
	}()

	waitFor(t, "the read-only notice on the ssh channel", func() bool {
		return strings.Contains(ch.stderrString(), "read-only")
	})
	waitFor(t, "the observer's session to open", func() bool { return fr.session(0) != nil })
	observed := fr.session(0)

	before := st.touches()
	ch.clientSend("rm -rf /\n")
	resizeCh <- sshWindowChangeMsg{Columns: 20, Rows: 5}
	waitFor(t, "the observer's keystrokes to be consumed", func() bool { return st.touches() > before })
	if n := observed.written(); n != 0 {
		t.Errorf("read-only ssh client's keystrokes reached the sandbox (%d writes)", n)
	}
	if n := observed.resizeCount(); n != 0 {
		t.Errorf("read-only ssh client resized the shared tmux session (%d resizes)", n)
	}
	if v := srv.attachHolderFor(run.ID).view(); v.Principal != holderOwner || v.Cols != 100 {
		t.Errorf("the observer disturbed the holder record: %+v", v)
	}

	_ = ch.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the read-only ssh bridge never returned")
	}
	if got := srv.attachHolderFor(run.ID); got != held {
		t.Fatalf("the read-only ssh client's teardown disturbed the holder: %+v", got)
	}
}
