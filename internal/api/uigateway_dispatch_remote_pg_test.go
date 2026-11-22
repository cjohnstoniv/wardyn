// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// heldConn is the org's end of the runner link with a stall: while held, every frame write waits, so the
// frame being written keeps the link's writer, as a socket whose far end stopped reading does.
type heldConn struct {
	runnerwire.Conn
	mu      sync.Mutex
	hold    chan struct{}
	blocked atomic.Int32
}

func (c *heldConn) WriteFrame(ctx context.Context, f runnerwire.Frame) error {
	c.mu.Lock()
	h := c.hold
	c.mu.Unlock()
	if h != nil {
		c.blocked.Add(1)
		defer c.blocked.Add(-1)
		select {
		case <-h:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return c.Conn.WriteFrame(ctx, f)
}

func (c *heldConn) stall() {
	c.mu.Lock()
	c.hold = make(chan struct{})
	c.mu.Unlock()
}

func (c *heldConn) resume() {
	c.mu.Lock()
	h := c.hold
	c.hold = nil
	c.mu.Unlock()
	if h != nil {
		close(h)
	}
}

// callLog is the runner's end of the link, noting every start CALL and every RESET it reads.
type callLog struct {
	runnerwire.Conn
	mu     sync.Mutex
	starts []dispatchStart
	resets atomic.Int32
}

func (c *callLog) ReadFrame(ctx context.Context) (runnerwire.Frame, error) {
	f, err := c.Conn.ReadFrame(ctx)
	if err != nil {
		return f, err
	}
	switch f.Type {
	case runnerwire.TypeCall:
		var call runnerwire.Call
		if runnerwire.Unmarshal(f.Payload, &call) == nil && (call.Method == runnerwire.MethodExecStream ||
			call.Method == runnerwire.MethodAttach || call.Method == runnerwire.MethodDialDesktop) {
			c.mu.Lock()
			c.starts = append(c.starts, dispatchStart{call.Method, time.Now()})
			c.mu.Unlock()
		}
	case runnerwire.TypeReset:
		c.resets.Add(1)
	}
	return f, nil
}

func (c *callLog) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.starts)
}

// runnerSide is the served runner's substrate: the fake, with execs of its own. A relay exec (socat) dials
// the rig's backend, after waiting on hold when that is set; any other exec echoes its stdin.
type runnerSide struct {
	*runnertest.Fake
	backend string
	hold    chan struct{}
}

func (r *runnerSide) ExecStream(ctx context.Context, _ string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if spec.Argv[0] != "socat" {
		inR, inW := io.Pipe()
		return &runner.ExecSession{Stdin: inW, Stdout: inR, Wait: func() (int, error) { return 0, nil },
			Close: func() error { _ = inW.Close(); return inR.Close() }}, nil
	}
	if r.hold != nil {
		select {
		case <-r.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	conn, err := net.Dial("tcp", strings.TrimPrefix(r.backend, "http://"))
	if err != nil {
		return nil, err
	}
	return &runner.ExecSession{Stdin: conn, Stdout: conn, Wait: func() (int, error) { return 0, nil }, Close: conn.Close}, nil
}

// remoteRouted is a rig's runner with its relay execs, terminal openings and desktop dials sent over the
// remote substrate (refs gain the runner's prefix); ExecStream's errors are noted.
type remoteRouted struct {
	runner.Runner
	sub     *remote.Substrate
	prefix  string
	execErr atomic.Value
}

func (r *remoteRouted) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	sess, err := r.sub.ExecStream(ctx, r.prefix+ref, spec)
	if err != nil {
		r.execErr.Store(err)
	}
	return sess, err
}

func (r *remoteRouted) Attach(ctx context.Context, ref string, opts runner.AttachOptions) (runner.Session, error) {
	return r.sub.Attach(ctx, r.prefix+ref, opts)
}

func (r *remoteRouted) DialDesktop(ctx context.Context, ref string) (io.ReadWriteCloser, error) {
	return r.sub.DialDesktop(ctx, r.prefix+ref)
}

// remoteLink is a remote substrate over a real runnerwire Peer pair to a served fake runner, with an open
// echo stream ("the other stream") whose frames can be made to hold the link's writer.
type remoteLink struct {
	link   *remote.Link
	routed *remoteRouted
	org    *heldConn
	calls  *callLog
	side   *runnerSide
	other  *runner.ExecSession
	sent   atomic.Int32
}

// newRemoteLink routes srv's runner over a new link; backend is where the runner's relay execs dial.
func newRemoteLink(t *testing.T, srv *Server, backend string) *remoteLink {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	org, run := runnerwire.Loopback()
	rl := &remoteLink{link: &remote.Link{}, org: &heldConn{Conn: org}, calls: &callLog{Conn: run},
		side: &runnerSide{Fake: runnertest.NewFake(), backend: backend}}
	sub := remote.New("runner-1", rl.link, remote.Options{})
	peer := rl.link.Attach(ctx, rl.org)
	served := runnertest.Serve(ctx, rl.calls, "runner-1", rl.side)
	t.Cleanup(func() { rl.org.resume(); cancel(); peer.Close(); served.Peer.Close() })
	if err := served.PublishCaps(ctx, placement.Capacity{CPUMillisMax: 4000, MemoryMiBMax: 8192}, nil, "test"); err != nil {
		t.Fatal(err)
	}
	waitWithin(t, 5*time.Second, "the runner's caps", func() bool { _, ok := sub.Caps(); return ok })
	rl.routed = &remoteRouted{Runner: srv.cfg.Runner, sub: sub, prefix: placement.RunnerRefPrefix("runner-1")}
	srv.cfg.Runner = rl.routed
	other, err := sub.ExecStream(ctx, rl.routed.prefix+"other", runner.ExecSpec{Argv: []string{"cat"}})
	if err != nil {
		t.Fatalf("open the other stream: %v", err)
	}
	rl.other = other
	return rl
}

// holdWriter stalls the link and has the other stream's next frame take its writer. It runs on a gate's
// writer too, so it reports rather than failing the test.
func (rl *remoteLink) holdWriter() error {
	rl.org.stall()
	n := rl.sent.Add(1)
	go func() { _, _ = fmt.Fprintf(rl.other.Stdin, "other-%d\n", n) }()
	for deadline := time.Now().Add(5 * time.Second); rl.org.blocked.Load() < 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			return errors.New("the other stream's frame never held the writer")
		}
	}
	return nil
}

// otherInOrder reads the other stream's echo of every line sent on it, in order.
func (rl *remoteLink) otherInOrder(t *testing.T) {
	t.Helper()
	got := make(chan error, 1)
	go func() {
		r := bufio.NewReader(rl.other.Stdout)
		for i := int32(1); i <= rl.sent.Load(); i++ {
			line, err := r.ReadString('\n')
			if err != nil {
				got <- err
				return
			}
			if want := fmt.Sprintf("other-%d\n", i); line != want {
				got <- fmt.Errorf("the other stream's line %d is %q, want %q", i, line, want)
				return
			}
		}
		got <- nil
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the other stream's frames did not all arrive")
	}
}

// countingBackend is the dispatch backend with its requests counted.
func countingBackend(n *atomic.Int32) http.Handler {
	inner := dispatchBackend()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1); inner.ServeHTTP(w, r) })
}

func TestIssuerPortalRemoteOpening_RefusedAtTheWriter_PG(t *testing.T) {
	t.Run("the relay exec", func(t *testing.T) {
		rig := newDispatchRig(t, dispatchBackend())
		rl := newRemoteLink(t, rig.srv, rig.backend.URL)
		c := rig.session(rig.grant)
		rig.srv.markUIAppReady(rig.run.ID.String() + "/code")
		if err := rl.holdWriter(); err != nil {
			t.Fatal(err)
		}
		before := rl.calls.count()
		by := time.Now().Add(rig.fresh)
		ans := rig.relayAsync(context.Background(), "/", c)
		time.Sleep(time.Until(by) + 200*time.Millisecond)
		rl.org.resume()
		r := answerWithin(t, ans, 10*time.Second)
		if r.Code != http.StatusServiceUnavailable || dispatchReason(r) != uiDeniedReasonDelegationUnavailable {
			t.Fatalf("a relay exec held behind the writer past the dispatch time: %d %s, want the dispatch 503", r.Code, r.Body.String())
		}
		if err, _ := rl.routed.execErr.Load().(error); !errors.Is(err, runner.ErrStartCutoff) {
			t.Fatalf("the substrate's ExecStream answered %v, want runner.ErrStartCutoff", err)
		}
		rl.otherInOrder(t)
		if n := rl.calls.count() - before; n != 0 {
			t.Fatalf("the runner received %d start CALLs, want 0", n)
		}
	})

	t.Run("the terminal's Attach", func(t *testing.T) {
		arig, _ := newDispatchAttachRig(t)
		rl := newRemoteLink(t, arig.srv, "http://127.0.0.1:1")
		if err := rl.holdWriter(); err != nil {
			t.Fatal(err)
		}
		before := rl.calls.count()
		by := time.Now().Add(dispatchFresh)
		c := arig.dial(t)
		time.Sleep(time.Until(by) + 200*time.Millisecond)
		rl.org.resume()
		if binary, _ := frames(c, 10*time.Second); binary != 0 {
			t.Fatalf("%d sandbox output frames from an opening refused at the writer, want 0", binary)
		}
		if n := rl.calls.count() - before; n != 0 {
			t.Fatalf("the runner received %d start CALLs, want 0", n)
		}
	})

	t.Run("the desktop's DialDesktop", func(t *testing.T) {
		rig, _, url := newDispatchDesktopRig(t)
		rl := newRemoteLink(t, rig.srv, "http://127.0.0.1:1")
		if err := rl.holdWriter(); err != nil {
			t.Fatal(err)
		}
		before := rl.calls.count()
		by := time.Now().Add(rig.fresh)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		v := dialDesktop(t, ctx, rig.uiHarness, url, rig.desktopCookie())
		time.Sleep(time.Until(by) + 200*time.Millisecond)
		rl.org.resume()
		if n := viewerFrames(v, 10*time.Second); n != 0 {
			t.Fatalf("%d display frames from a dial refused at the writer, want 0", n)
		}
		if n := rl.calls.count() - before; n != 0 {
			t.Fatalf("the runner received %d start CALLs, want 0", n)
		}
	})

	t.Run("received, then held", func(t *testing.T) {
		rig := newDispatchRig(t, dispatchBackend())
		rl := newRemoteLink(t, rig.srv, rig.backend.URL)
		rl.side.hold = make(chan struct{})
		t.Cleanup(func() { close(rl.side.hold) })
		c := rig.session(rig.grant)
		rig.srv.markUIAppReady(rig.run.ID.String() + "/code")
		before, resets := rl.calls.count(), rl.calls.resets.Load()
		by := time.Now().Add(rig.fresh)
		ans := rig.relayAsync(context.Background(), "/", c)
		waitWithin(t, 5*time.Second, "the runner to receive the relay exec's CALL", func() bool { return rl.calls.count() > before })
		r := answerWithin(t, ans, time.Until(by)+time.Second)
		if late := time.Since(by); r.Code != http.StatusServiceUnavailable || dispatchReason(r) != uiDeniedReasonDelegationUnavailable || late > time.Second {
			t.Fatalf("a received, held relay exec: %d %s, %s past the dispatch time; want the dispatch 503 within 1s", r.Code, r.Body.String(), late)
		}
		waitWithin(t, 5*time.Second, "the call's RESET at the runner", func() bool { return rl.calls.resets.Load() > resets })
		if !rl.link.Online() {
			t.Fatal("the link went offline")
		}
	})
}

// heldAudit is a recorder whose ui.dispatch rows wait until free is closed.
type heldAudit struct {
	*safeRecorder
	free chan struct{}
	mu   sync.Mutex
	rows []types.AuditEvent
}

func (a *heldAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action == "ui.dispatch" {
		<-a.free
		a.mu.Lock()
		a.rows = append(a.rows, ev)
		a.mu.Unlock()
	}
	return a.safeRecorder.Record(ctx, ev)
}

func (a *heldAudit) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.rows)
}

func TestUIRelayIssuerPortal_RemoteClientAnsweredWhileLinkHeld_PG(t *testing.T) {
	// setup is a remote run whose gate's writer, before its check, has the other stream take the link's
	// writer: the request's first write then waits for it at the dispatch time.
	setup := func(t *testing.T) (*dispatchRig, *remoteLink, *atomic.Int32, *http.Cookie, time.Time) {
		t.Helper()
		var served atomic.Int32
		rig := newDispatchRig(t, countingBackend(&served))
		rl := newRemoteLink(t, rig.srv, rig.backend.URL)
		c := rig.session(rig.grant)
		rig.srv.markUIAppReady(rig.run.ID.String() + "/code")
		var once sync.Once
		rig.srv.uiDispatch.beforeAdmit = func() {
			once.Do(func() {
				if err := rl.holdWriter(); err != nil {
					t.Error(err)
				}
			})
		}
		return rig, rl, &served, c, time.Now().Add(rig.fresh)
	}

	t.Run("a transient stall, then resume", func(t *testing.T) {
		for _, holdRecord := range []bool{false, true} {
			t.Run(fmt.Sprintf("refusal record held %v", holdRecord), func(t *testing.T) {
				rig, rl, served, c, by := setup(t)
				audit := &heldAudit{safeRecorder: rig.audit, free: make(chan struct{})}
				rig.srv.cfg.Audit = audit
				held, free := make(chan struct{}), make(chan struct{})
				if holdRecord {
					var once sync.Once
					rig.srv.uiDispatch.refuseHook = func() { once.Do(func() { close(held) }); <-free }
				}
				ans := rig.relayAsync(context.Background(), "/", c)
				if holdRecord {
					select {
					case <-held:
					case <-time.After(time.Until(by) + 2*time.Second):
						t.Fatal("the gate's 503 was never recorded")
					}
					select {
					case r := <-ans:
						t.Fatalf("an answer (%d) while the gate's 503 record was held", r.Code)
					case <-time.After(200 * time.Millisecond):
					}
					close(free)
				}
				r := answerWithin(t, ans, time.Until(by)+time.Second)
				if r.Code != http.StatusServiceUnavailable || dispatchReason(r) != uiDeniedReasonDelegationUnavailable {
					t.Fatalf("the browser's answer with the link held: %d %s, want the dispatch 503, never 502", r.Code, r.Body.String())
				}
				if late := time.Since(by); !holdRecord && late > time.Second {
					t.Fatalf("the 503 came %s after the dispatch time, want within 1s", late)
				}
				if rl.org.blocked.Load() == 0 {
					t.Fatal("the other stream released the link's writer before the browser's answer")
				}
				rl.org.resume() // before the keepalive could fail the link
				rl.otherInOrder(t)
				waitWithin(t, 5*time.Second, "the slot, with the outcome recorder held", func() bool { return slots(rig.srv, rig.run.ID) == 0 })
				if n := served.Load(); n != 0 {
					t.Fatalf("the backend saw %d requests: a frame of the request was admitted after the stop", n)
				}
				if !rl.link.Online() {
					t.Fatal("the link went offline")
				}
				close(audit.free)
				waitWithin(t, 5*time.Second, "the outcome row", func() bool { return audit.count() >= 1 })
				time.Sleep(200 * time.Millisecond)
				audit.mu.Lock()
				rows := append([]types.AuditEvent(nil), audit.rows...)
				audit.mu.Unlock()
				if len(rows) != 1 {
					t.Fatalf("%d outcome rows, want exactly 1", len(rows))
				}
				row := string(rows[0].Data)
				for _, k := range []string{`"bytes_done"`, `"partial"`, `"started_at"`, `"write_returned_at"`, `"stop_returned_at"`} {
					if !strings.Contains(row, k) {
						t.Fatalf("the outcome row %s lacks %s", row, k)
					}
				}
			})
		}
	})

	// The keepalive and the call timeout run at their production values here: internal/api cannot scale
	// either, so the link fails within MissedPings+1 intervals of the stall's start (60s at 15s).
	t.Run("b a persistent stall, then link failure", func(t *testing.T) {
		rig, rl, _, c, by := setup(t)
		start := time.Now()
		r := answerWithin(t, rig.relayAsync(context.Background(), "/", c), time.Until(by)+time.Second)
		answered := time.Now()
		if r.Code != http.StatusServiceUnavailable || dispatchReason(r) != uiDeniedReasonDelegationUnavailable {
			t.Fatalf("the browser's answer on a stalled link: %d %s", r.Code, r.Body.String())
		}
		time.Sleep(3 * time.Second)
		if slots(rig.srv, rig.run.ID) == 0 {
			t.Fatal("the slot was freed while the stall lasted")
		}
		bound := time.Duration(runnerwire.MissedPings+1) * runnerwire.PingInterval
		waitWithin(t, bound+5*time.Second, "the link to fail", func() bool { return !rl.link.Online() })
		failed := time.Now()
		waitWithin(t, time.Second, "the slot within 1s of the link's failure", func() bool { return slots(rig.srv, rig.run.ID) == 0 })
		freed := time.Now()
		if _, err := rl.other.Stdout.Read(make([]byte, 1)); err == nil {
			t.Fatal("the other stream outlived the link")
		}
		t.Logf("the browser's 503 came %s after the dispatch time; the slot was freed %s after the stall began (the link failed at %s, bound %s)",
			answered.Sub(by), freed.Sub(start), failed.Sub(start), bound)
		if failed.Sub(start) > bound+time.Second {
			t.Fatalf("the link failed %s after the stall began, want within %s", failed.Sub(start), bound)
		}
	})

	t.Run("close first", func(t *testing.T) {
		var served atomic.Int32
		rig := newDispatchRig(t, countingBackend(&served))
		rl := newRemoteLink(t, rig.srv, rig.backend.URL)
		c := rig.session(rig.grant)
		rig.srv.markUIAppReady(rig.run.ID.String() + "/code")
		var once sync.Once
		admitted := make(chan struct{})
		rig.srv.uiDispatch.beforeAdmit = func() {
			once.Do(func() {
				if err := rl.holdWriter(); err != nil {
					t.Error(err)
				}
				close(admitted)
			})
		}
		by := time.Now().Add(rig.fresh)
		ctx, cancel := context.WithCancel(context.Background())
		ans := rig.relayAsync(ctx, "/", c)
		<-admitted
		time.Sleep(50 * time.Millisecond) // the request's first write is now waiting for the link's writer
		cancel()
		gone := time.Now()
		answerWithin(t, ans, time.Second)
		if !gone.Before(by) {
			t.Fatal("the client went away after the dispatch time")
		}
		if rl.org.blocked.Load() == 0 {
			t.Fatal("the link's writer was released before the handler returned")
		}
		rl.org.resume()
		rl.otherInOrder(t)
		waitWithin(t, 5*time.Second, "the slot", func() bool { return slots(rig.srv, rig.run.ID) == 0 })
		if n := served.Load(); n != 0 {
			t.Fatalf("the backend saw %d requests after the client left", n)
		}
	})
}
