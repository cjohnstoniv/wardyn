// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// tailRig is localPipe's two channels without the CLI around them: the organisation's end of the control
// WebSocket, which a test closes, and the runner's end of the local socket, which it writes to. show
// blocks until release, as a terminal that is slow to take bytes does.
type tailRig struct {
	org     *websocket.Conn
	runner  net.Conn
	output  func(ctx context.Context) error
	shown   func() string
	first   chan struct{} // closed once show has its first bytes
	release func()
}

func newTailRig(t *testing.T) *tailRig {
	t.Helper()
	orgC := make(chan *websocket.Conn, 1)
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		orgC <- c
		<-stop
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	r := &tailRig{org: <-orgC, runner: runner, first: make(chan struct{})}
	gate := make(chan struct{})
	var once, firstOnce sync.Once
	r.release = func() { once.Do(func() { close(gate) }) }
	var mu sync.Mutex
	var buf bytes.Buffer
	r.shown = func() string { mu.Lock(); defer mu.Unlock(); return buf.String() }
	show := func(b []byte) error {
		firstOnce.Do(func() { close(r.first) })
		<-gate
		mu.Lock()
		buf.Write(b)
		mu.Unlock()
		return nil
	}
	pipe := localPipe(ws, &localConn{conn: cli}, []byte(`{"type":"attach-mode","transport":"local"}`))
	r.output = func(ctx context.Context) error { return pipe.output(ctx, show, func([]byte) {}) }
	t.Cleanup(func() {
		r.release()
		close(stop)
		_ = ws.CloseNow()
		_ = cli.Close()
		_ = runner.Close()
		_ = ln.Close()
		srv.Close()
	})
	return r
}

// run starts the output half and returns its result once it ends.
func (r *tailRig) run(ctx context.Context) <-chan error {
	res := make(chan error, 1)
	go func() { res <- r.output(ctx) }()
	return res
}

// runnerSends queues terminal frames, and when end is set the runner's end frame, on the local socket.
func (r *tailRig) runnerSends(t *testing.T, end string, data ...string) {
	t.Helper()
	for _, d := range data {
		if err := writeLocalFrame(r.runner, localData, []byte(d)); err != nil {
			t.Fatal(err)
		}
	}
	if end != "" {
		if err := writeLocalJSONForTest(r.runner, localEnd, map[string]string{"reason": end}); err != nil {
			t.Fatal(err)
		}
	}
}

// orgCloses closes the control socket from the organisation's side and lets the CLI read it.
func (r *tailRig) orgCloses(code websocket.StatusCode, reason string) {
	go func() { _ = r.org.Close(code, reason) }()
	time.Sleep(150 * time.Millisecond)
}

func setLocalEndGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := localEndGrace
	t.Cleanup(func() { localEndGrace = old })
	localEndGrace = d
}

func mustEnd(t *testing.T, res <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-res:
		return err
	case <-time.After(within):
		t.Fatalf("the output half did not end within %v", within)
		return nil
	}
}

// A session that ends on its own: the organisation's normal close can overtake terminal bytes the runner
// has already sent. The detach must wait until they are shown.
func TestLocalPipe_NaturalEndShowsTheQueuedTail(t *testing.T) {
	setLocalEndGrace(t, 30*time.Second)
	r := newTailRig(t)
	r.runnerSends(t, "shell_exited", "one", "tail")
	res := r.run(context.Background())
	<-r.first // show holds "one"; "tail" and the end are queued behind it
	r.orgCloses(websocket.StatusNormalClosure, "")
	select {
	case err := <-res:
		t.Fatalf("the output half ended with %v before the queued tail was shown (shown %q)", err, r.shown())
	case <-time.After(200 * time.Millisecond):
	}
	r.release()
	if err := mustEnd(t, res, 5*time.Second); !isNormalClose(err) {
		t.Errorf("result = %v, want a clean detach: the runner's shell exited", err)
	}
	if got := r.shown(); got != "onetail" {
		t.Errorf("shown %q, want %q", got, "onetail")
	}
}

// When the runner's last output does not arrive in localEndGrace, the CLI says so instead of detaching.
func TestLocalPipe_ExpiredDrainSaysTheOutputIsIncomplete(t *testing.T) {
	setLocalEndGrace(t, 150*time.Millisecond)
	r := newTailRig(t)
	r.runnerSends(t, "", "one", "tail")
	res := r.run(context.Background())
	<-r.first // show never returns until the test ends
	r.orgCloses(websocket.StatusNormalClosure, "")
	err := mustEnd(t, res, 5*time.Second)
	if err == nil || isNormalClose(err) || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("result = %v, want an error saying the output may be incomplete", err)
	}
}

// Forced stops do not wait for the drain: a cancelled session (signal) and a close that is not a natural
// end (take-over, sign-out, lease lost) both return at once, with the tail still unshown.
func TestLocalPipe_ForcedStopsStayPrompt(t *testing.T) {
	t.Run("cancel during the drain", func(t *testing.T) {
		setLocalEndGrace(t, 30*time.Second)
		r := newTailRig(t)
		r.runnerSends(t, "", "one", "tail")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		res := r.run(ctx)
		<-r.first
		r.orgCloses(websocket.StatusNormalClosure, "")
		cancel()
		if err := mustEnd(t, res, 2*time.Second); !errors.Is(err, context.Canceled) {
			t.Errorf("result = %v, want the cancellation", err)
		}
	})
	t.Run("a close that is not a natural end", func(t *testing.T) {
		setLocalEndGrace(t, 30*time.Second)
		r := newTailRig(t)
		r.runnerSends(t, "", "one", "tail")
		res := r.run(context.Background())
		<-r.first
		r.orgCloses(4401, "session ended")
		if err := mustEnd(t, res, 2*time.Second); websocket.CloseStatus(err) != 4401 {
			t.Errorf("result = %v, want the organisation's 4401 close at once", err)
		}
	})
}

// The wait for the runner's last output is the 2 seconds the CHANGELOG states, and it stays under
// attachCloseGrace, which is when the session stops waiting for the output half.
func TestLocalEndGrace_IsTwoSecondsAndUnderTheCloseGrace(t *testing.T) {
	if localEndGrace != 2*time.Second {
		t.Errorf("localEndGrace = %v, want 2s", localEndGrace)
	}
	if localEndGrace >= attachCloseGrace {
		t.Errorf("localEndGrace %v must stay under attachCloseGrace %v", localEndGrace, attachCloseGrace)
	}
}

// Only the runner's own clean end says the tail was shown. A local stream that breaks inside a frame, or
// ends for another reason, after the organisation's normal close is not a clean detach.
func TestLocalPipe_ABrokenLocalStreamIsNotACleanDetach(t *testing.T) {
	for name, runnerEnds := range map[string]func(t *testing.T, r *tailRig){
		"cut inside a frame": func(t *testing.T, r *tailRig) {
			if _, err := r.runner.Write([]byte{localData, 0, 0, 0, 4, 't', 'a'}); err != nil { // 4 bytes promised, 2 sent
				t.Fatal(err)
			}
			_ = r.runner.Close()
		},
		"an end that is not clean": func(t *testing.T, r *tailRig) { r.runnerSends(t, "runner_link_lost") },
	} {
		t.Run(name, func(t *testing.T) {
			setLocalEndGrace(t, 30*time.Second)
			r := newTailRig(t)
			r.runnerSends(t, "", "one")
			runnerEnds(t, r)
			res := r.run(context.Background())
			<-r.first
			r.orgCloses(websocket.StatusNormalClosure, "")
			r.release()
			if err := mustEnd(t, res, 5*time.Second); err == nil || isNormalClose(err) {
				t.Errorf("result = %v, want an error: the runner did not say the session ended cleanly", err)
			}
			if got := r.shown(); got != "one" {
				t.Errorf("shown %q, want %q", got, "one")
			}
		})
	}
}

// The organisation's socket ends with no close frame when the runner ended the session. The queued tail is
// still shown before the output half ends, and the runner's clean end is a clean detach.
func TestLocalPipe_ADroppedControlSocketStillShowsTheQueuedTail(t *testing.T) {
	setLocalEndGrace(t, 30*time.Second)
	r := newTailRig(t)
	r.runnerSends(t, "shell_exited", "one", "tail")
	res := r.run(context.Background())
	<-r.first
	_ = r.org.CloseNow()
	select {
	case err := <-res:
		t.Fatalf("the output half ended with %v before the queued tail was shown (shown %q)", err, r.shown())
	case <-time.After(300 * time.Millisecond):
	}
	r.release()
	if err := mustEnd(t, res, 5*time.Second); !isNormalClose(err) {
		t.Errorf("result = %v, want a clean detach: the runner's shell exited", err)
	}
	if got := r.shown(); got != "onetail" {
		t.Errorf("shown %q, want %q", got, "onetail")
	}
}

// The runner ends the session first and the organisation's socket then ends with no close frame, which
// names no reason: the runner's reason is the end shown. Its clean end is a clean detach, and any other
// end is still an error.
func TestLocalPipe_ARunnerEndThenADroppedControlSocketShowsTheRunnersEnd(t *testing.T) {
	for reason, wantClean := range map[string]bool{"shell_exited": true, "ended_by_organisation": false} {
		t.Run(reason, func(t *testing.T) {
			setLocalEndGrace(t, 30*time.Second)
			r := newTailRig(t)
			r.runnerSends(t, reason, "one")
			r.release()
			res := r.run(context.Background())
			time.Sleep(150 * time.Millisecond) // the runner's end is read; the output half waits for the organisation
			_ = r.org.CloseNow()
			err := mustEnd(t, res, 5*time.Second)
			if wantClean && !isNormalClose(err) {
				t.Errorf("result = %v, want a clean detach: the runner's shell exited", err)
			}
			if !wantClean && (err == nil || !strings.Contains(err.Error(), reason)) {
				t.Errorf("result = %v, want the runner's end %q", err, reason)
			}
			if got := r.shown(); got != "one" {
				t.Errorf("shown %q, want %q", got, "one")
			}
		})
	}
}

// The runner ends the session first and the organisation's close then comes with a code: the organisation
// stopped the session, so its close is the end shown, even after the runner's clean end. A normal close
// names no stop, so an end of the runner's that is not clean is still the error shown.
func TestLocalPipe_ARunnerEndThenACodedCloseShowsTheOrganisationsStop(t *testing.T) {
	for _, tc := range []struct {
		reason string
		code   websocket.StatusCode
	}{
		{"shell_exited", 4401},
		{"run_ended", websocket.StatusPolicyViolation},
		{"shell_exited", 4409},
		{"shell_exited", websocket.StatusInternalError},
		{"runner_link_lost", websocket.StatusNormalClosure},
	} {
		t.Run(tc.reason+" then "+tc.code.String(), func(t *testing.T) {
			setLocalEndGrace(t, 30*time.Second)
			r := newTailRig(t)
			r.runnerSends(t, tc.reason, "one")
			r.release()
			res := r.run(context.Background())
			time.Sleep(150 * time.Millisecond) // the runner's end is read; the output half waits for the organisation
			r.orgCloses(tc.code, "stopped")
			err := mustEnd(t, res, 5*time.Second)
			if tc.code == websocket.StatusNormalClosure {
				if err == nil || isNormalClose(err) || !strings.Contains(err.Error(), tc.reason) {
					t.Errorf("result = %v, want the runner's end %q", err, tc.reason)
				}
				return
			}
			if websocket.CloseStatus(err) != tc.code {
				t.Errorf("result = %v, want the organisation's close %d", err, tc.code)
			}
		})
	}
}

// Every close the organisation gives a session it stopped returns at once, with the tail unshown.
func TestLocalPipe_EveryStopCodeStaysPrompt(t *testing.T) {
	for _, code := range []websocket.StatusCode{4401, websocket.StatusPolicyViolation, 4409, websocket.StatusInternalError} {
		setLocalEndGrace(t, 30*time.Second)
		r := newTailRig(t)
		r.runnerSends(t, "shell_exited", "one", "tail")
		res := r.run(context.Background())
		<-r.first
		r.orgCloses(code, "stopped")
		if err := mustEnd(t, res, 2*time.Second); websocket.CloseStatus(err) != code {
			t.Errorf("close %d: result = %v, want that close at once", code, err)
		}
	}
}
