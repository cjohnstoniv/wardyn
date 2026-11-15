// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Every door that starts a command, a stream or a display in a run's sandbox starts it under the run's start lock,
// on the run as it stands there (run_admit.go). Each test below ends the run at that lock (startEndsRun): an end
// that takes no operation lock lands just before the door's last read, and the door starts nothing.

// endedRun is run as its end wrote it.
func endedRun(run types.AgentRun) types.AgentRun {
	run.State = types.RunCompleted
	return run
}

// countingExecs is an SSH runner whose execs are counted and echo.
func countingExecs() (*sshFakeRunner, *atomic.Int32) {
	var n atomic.Int32
	return &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
		n.Add(1)
		return fakeEchoExecSession(), nil
	}}, &n
}

func TestStartFence_SSHExecRefusesARunThatEndsAtItsStartLock(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	fr, execs := countingExecs()
	srv := New(Config{BaseCtx: testBaseCtx(t), Store: st, Runner: fr})
	l := startEndsRun(srv, run.ID, func() { st.putRun(endedRun(run)) })
	ch := newFakeSSHChannel()
	srv.bridgeSSHExec(t.Context(), run.ID, principal, ch, "true", nil)
	if got := ch.stderrString(); !strings.Contains(got, "run is not RUNNING; ssh unavailable (state=COMPLETED)") {
		t.Fatalf("stderr = %q, want the ended run's state", got)
	}
	if n, asked := execs.Load(), l.asked.Load(); n != 0 || asked != 1 {
		t.Fatalf("execs = %d after %d start lock waits, want none after one", n, asked)
	}
}

// forwardOpen is one direct-tcpip channel open; Reject keeps its words.
type forwardOpen struct {
	extra  []byte
	reject string
}

func (c *forwardOpen) Accept() (ssh.Channel, <-chan *ssh.Request, error) {
	return nil, nil, errors.New("not accepted")
}
func (c *forwardOpen) Reject(_ ssh.RejectionReason, msg string) error { c.reject = msg; return nil }
func (c *forwardOpen) ChannelType() string                            { return "direct-tcpip" }
func (c *forwardOpen) ExtraData() []byte                              { return c.extra }

func TestStartFence_SSHForwardRefusesARunThatEndsAtItsStartLock(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	fr, execs := countingExecs()
	srv := New(Config{BaseCtx: testBaseCtx(t), Store: st, Runner: fr})
	l := startEndsRun(srv, run.ID, func() { st.putRun(endedRun(run)) })
	open := &forwardOpen{extra: ssh.Marshal(sshDirectTCPIPMsg{DestHost: "127.0.0.1", DestPort: 9999})}
	srv.handleSSHDirectTCPIP(t.Context(), run.ID, principal, open)
	if !strings.Contains(open.reject, "run is not RUNNING; ssh unavailable (state=COMPLETED)") {
		t.Fatalf("rejected with %q, want the ended run's state", open.reject)
	}
	if n, asked := execs.Load(), l.asked.Load(); n != 0 || asked != 1 {
		t.Fatalf("execs = %d after %d start lock waits, want none after one", n, asked)
	}
}

// A person's sftp channel starts its sftp-server under the fence; a computer folder's pass, which an end runs on a
// run that is no longer live, starts it with no fence and no last read.
func TestStartFence_SFTPFencesAPersonsChannelNotAFolderPass(t *testing.T) {
	st, run, _ := sshOwnedRunningRun(t)
	fr, execs := countingExecs()
	srv := New(Config{BaseCtx: testBaseCtx(t), Store: st, Runner: fr})
	l := startEndsRun(srv, run.ID, func() { st.putRun(endedRun(run)) })
	if _, reason, err := srv.execSFTPServer(t.Context(), run, true, "-d", "%d"); err == nil || reason != "sftp unavailable: the sandbox is gone" || execs.Load() != 0 {
		t.Fatalf("a person's channel = %q %v after %d execs, want the gone sandbox's words and none", reason, err, execs.Load())
	}
	if sess, reason, err := srv.execSFTPServer(t.Context(), endedRun(run), false, "-d", "/home/agent/work"); err != nil || sess == nil || execs.Load() != 1 {
		t.Fatalf("a folder pass = %q %v after %d execs, want one started", reason, err, execs.Load())
	}
	if asked := l.asked.Load(); asked != 1 {
		t.Fatalf("start lock waits = %d, want one (the person's channel only)", asked)
	}
}

func TestStartFence_AttachRefusesARunThatEndsAtItsStartLock(t *testing.T) {
	var attaches atomic.Int32
	fr := &sshFakeRunner{attachFn: func() (runner.Session, error) {
		attaches.Add(1)
		return newFakeShellSession(), nil
	}}
	st, run, _ := sshOwnedRunningRun(t)
	srv := New(Config{BaseCtx: testBaseCtx(t), Store: st, Runner: fr})
	l := startEndsRun(srv, run.ID, func() { st.putRun(endedRun(run)) })
	h := &attachHolder{}
	h.writable.Store(true)
	t.Cleanup(func() { _ = h.mux.Close() })
	err := srv.establishExec(t.Context(), run, h)
	if !errors.As(err, new(startRefused)) || err.Error() != "run is not RUNNING; cannot attach (state=COMPLETED)" {
		t.Fatalf("attach = %v, want the ended run's refusal", err)
	}
	if n, asked := attaches.Load(), l.asked.Load(); n != 0 || asked != 1 {
		t.Fatalf("attaches = %d after %d start lock waits, want none after one", n, asked)
	}
}

// An attach holds the fence for its own bound, attachPrepareTimeout, not an exec's: one slower than
// execStartBudget still opens.
func TestStartFence_AnAttachHasItsOwnBudget(t *testing.T) {
	defer func(b time.Duration) { execStartBudget = b }(execStartBudget)
	execStartBudget = 20 * time.Millisecond
	fr := &sshFakeRunner{attachFn: func() (runner.Session, error) {
		time.Sleep(150 * time.Millisecond)
		return newFakeShellSession(), nil
	}}
	st, run, _ := sshOwnedRunningRun(t)
	srv := New(Config{BaseCtx: testBaseCtx(t), Store: st, Runner: fr})
	h := &attachHolder{}
	h.writable.Store(true)
	t.Cleanup(func() { _ = h.mux.Close() })
	if err := srv.establishExec(t.Context(), run, h); err != nil {
		t.Fatalf("an attach slower than an exec's budget = %v, want it opened", err)
	}
}

func TestStartFence_LocalAttachGrantsNothingOnARunThatEndsAtItsStartLock(t *testing.T) {
	rig := newLocalRig(t, localRigOpts{})
	l := startEndsRun(rig.srv, rig.run.ID, func() {
		endRun(rig.st.authzStore, rig.run.ID, func(r *types.AgentRun) { r.State = types.RunCompleted })
	})
	c := rig.dial(t)
	_, _, err := c.Read(context.Background())
	if websocket.CloseStatus(err) != attachLocalGrantFailedCode {
		t.Fatalf("socket ended with %v, want close %d", err, attachLocalGrantFailedCode)
	}
	if n, asked := rig.sub.calls(), l.asked.Load(); n != 0 || asked != 1 {
		t.Fatalf("grants = %d after %d start lock waits, want none after one", n, asked)
	}
}

// The launcher and the relay each start under the fence: with the app's port not yet known to listen the launcher
// is the first start, and with it known the relay is.
func TestStartFence_UIStartsRefuseARunThatEndsAtTheirStartLock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready bool
	}{{"launcher", false}, {"relay", true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newUIHarness(t, okBackend())
			cookie := h.openSession()
			var execs atomic.Int32
			inner := h.runner.execFn
			h.runner.execFn = func(spec runner.ExecSpec) (*runner.ExecSession, error) {
				execs.Add(1)
				return inner(spec)
			}
			if tc.ready {
				h.srv.markUIAppReady(h.run.ID.String() + "/code")
			}
			l := startEndsRun(h.srv, h.run.ID, func() { h.store.putRun(endedRun(h.run)) })
			rec := h.relay("/", cookie, nil)
			var body errorBody
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != http.StatusConflict || body.Reason != reasonUIGatewayNotRunning || body.Error != "run is not RUNNING; the UI app is gone (state=COMPLETED)" {
				t.Fatalf("relay = %d %s, want 409 %s with the ended run's state", rec.Code, rec.Body.String(), reasonUIGatewayNotRunning)
			}
			if n, asked := execs.Load(), l.asked.Load(); n != 0 || asked == 0 {
				t.Fatalf("execs = %d after %d start lock waits, want none after the first", n, asked)
			}
		})
	}
}

// A UI start refused at its last read is answered as uiDial's first read answers the same run; a fence that cannot
// be taken is the lock's 503; the fence's other refusals say what they are.
func TestStartFence_UIExecFailureAnswersTheFencesRefusals(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		status      int
		reason, msg string
	}{
		{"kept at the last read", startRefused(uiLiveRunWords.ended), http.StatusConflict, reasonUIGatewayRunKept, "run has ended; the UI app is gone"},
		{"ended at the last read", startRefused(fmt.Sprintf(uiLiveRunWords.notRunning, types.RunCompleted)), http.StatusConflict, reasonUIGatewayNotRunning, "run is not RUNNING; the UI app is gone (state=COMPLETED)"},
		{"gone at the last read", startRefused(uiLiveRunWords.notFound), http.StatusNotFound, reasonRunNotFound, "run not found"},
		{"no start lock", startUnfenced{db.ErrLockNoCapacity}, http.StatusServiceUnavailable, reasonLockUnavailable, lockUnavailableMsg},
		{"too slow", errStartTooSlow, http.StatusBadGateway, reasonUIGatewayExecFailed, errStartTooSlow.Error()},
		{"lock lost", errStartLockLost, http.StatusBadGateway, reasonUIGatewayExecFailed, errStartLockLost.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var de *uiDialError
			if err := uiExecFailure(t.Context(), reasonUIGatewayExecFailed, "start ui relay exec", tc.err); !errors.As(err, &de) ||
				de.status != tc.status || de.reason != tc.reason || de.msg != tc.msg {
				t.Fatalf("uiExecFailure(%v) = %+v, want %d %s %q", tc.err, de, tc.status, tc.reason, tc.msg)
			}
		})
	}
}

func TestStartFence_DesktopDialOpensNoDisplayOnARunThatEndsAtItsStartLock(t *testing.T) {
	h, dr := newDesktopHarness(t)
	c := desktopCookie(t, h.enterDesktop(h.ticket(h.run.ID, h.owner, oidc.RoleUser)))
	l := startEndsRun(h.srv, h.run.ID, func() { h.store.putRun(endedRun(h.run)) })
	srv := httptest.NewServer(h.gateway)
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+desktopPath(h, uiDesktopRFB),
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {c.Name + "=" + c.Value}}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	_, _, err = ws.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != desktopCloseRunEnded {
		t.Fatalf("stream = %v, want close %d", err, desktopCloseRunEnded)
	}
	if n, asked := len(dr.dialled), l.asked.Load(); n != 0 || asked != 1 {
		t.Fatalf("dials = %d after %d start lock waits, want none after one", n, asked)
	}
}

// The sign-in's display starts under the fence; the steps before it and the rollback after it never wait on it.
func TestStartFence_SigninDisplayNeverStartsOnARunThatEndsAtItsStartLock(t *testing.T) {
	f := newSigninFixture(t)
	l := startEndsRun(f.srv, f.run.ID, func() { f.st.putRun(endedRun(f.run)) })
	if code, body := f.post(t, "/desktop-signin", ""); code != http.StatusBadGateway || !strings.Contains(body, reasonDesktopSigninStartFailed) {
		t.Fatalf("begin = %d %s, want 502 %s", code, body, reasonDesktopSigninStartFailed)
	}
	if seen := f.rn.seen(); slices.Contains(seen, "person:start") || !slices.Contains(seen, "agent:pause-agent") {
		t.Fatalf("calls = %v, want the pause and no display start", seen)
	}
	if asked := l.asked.Load(); asked != 1 {
		t.Fatalf("start lock waits = %d, want one (the display's start)", asked)
	}
}

func TestStartFence_SigninRelayRefusesARunThatEndsAtItsStartLock(t *testing.T) {
	f := newSigninFixture(t)
	f.begin(t)
	l := startEndsRun(f.srv, f.run.ID, func() { f.st.putRun(endedRun(f.run)) })
	released := false
	_, err := f.srv.desktopSigninDial(t.Context(), f.run, uiSession{Run: f.run.ID, App: desktopSigninApp, Principal: signinOwner},
		func() { released = true })
	var de *uiDialError
	if !errors.As(err, &de) || de.status != http.StatusConflict || de.reason != reasonUIGatewayNotRunning {
		t.Fatalf("sign-in relay = %v, want 409 %s", err, reasonUIGatewayNotRunning)
	}
	if asked := l.asked.Load(); !released || asked != 1 {
		t.Fatalf("released = %v after %d start lock waits, want the slot released after one", released, asked)
	}
}

// endsAtTheNthStart makes the nth wait for key run end first.
type endsAtTheNthStart struct {
	db.Locker
	key   db.LockKey
	n     int32
	end   func()
	asked atomic.Int32
}

func (l *endsAtTheNthStart) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	if k == l.key && l.asked.Add(1) == l.n {
		l.end()
	}
	return l.Locker.Lock(ctx, k, wait)
}

// TestStartFence_SigninShareRunsNothingOnARunThatEndsAtItsStartLock: a share whose run ends at its export's start
// lock runs no export, and one whose run ends at its import's runs no import; each answers 409
// desktop_signin_not_running and records no share.
// Catches: the export or the import started without the start lock.
func TestStartFence_SigninShareRunsNothingOnARunThatEndsAtItsStartLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   int32
		ran  []string
	}{{"export", 1, nil}, {"import", 2, []string{"person:export"}}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSigninFixture(t)
			f.begin(t)
			f.rn.stdout["export"] = `[]`
			before := len(f.rn.seen())
			l := &endsAtTheNthStart{Locker: db.NewLocalLocker(), key: runStartKey(f.run.ID), n: tc.at, end: func() { f.st.putRun(endedRun(f.run)) }}
			f.srv.locks.override = l
			if code, body := f.post(t, "/desktop-signin/shares", `{"site":"site.test"}`); code != http.StatusConflict || !strings.Contains(body, reasonDesktopSigninNotRunning) {
				t.Fatalf("share = %d %s, want 409 %s", code, body, reasonDesktopSigninNotRunning)
			}
			if got := f.rn.seen()[before:]; !slices.Equal(got, tc.ran) {
				t.Fatalf("calls after the begin = %v, want %v", got, tc.ran)
			}
			if n := l.asked.Load(); n != tc.at {
				t.Fatalf("start lock waits = %d, want %d", n, tc.at)
			}
			f.st.smu.Lock()
			shares := len(f.st.shares)
			f.st.smu.Unlock()
			if shares != 0 {
				t.Fatalf("%d shares recorded, want none", shares)
			}
		})
	}
}
