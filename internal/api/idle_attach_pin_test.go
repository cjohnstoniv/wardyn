// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An attached session is never idle-stopped, on every surface.
//
// The idle reaper (internal/lifecycle) stops a run whose updated_at is older than
// auto_stop_after_sec (3600 in the shipped default policy) plus lifecycle.TouchDebounce.
// A human attached to a run holds it alive ONLY through attachKeepalive, which touches the
// run on open and every attachKeepaliveInterval. These tests drive the real reaper against a
// virtual clock and the real attach surfaces, so a surface that stops touching is stopped:
//
//  1. attach and let the keepalive tick,
//  2. jump the clock two hours past the idle threshold,
//  3. let the next keepalive touch land, then run one reaper tick: the run must survive,
//  4. detach, jump past the threshold again, tick: the run must now be stopped.
//
// Step 3 fails when a surface's attachKeepalive is removed, which is the proof that the pin
// does not hold merely because the clock never reached the threshold.

const idleAutoStopSec = 3600

// idleFixture is the virtual clock, the store the surfaces touch, and the lifecycle.Store and
// lifecycle.Stopper the reaper runs over. updated_at moves only by TouchRun, as in Postgres.
type idleFixture struct {
	mu      sync.Mutex
	now     time.Time
	run     uuid.UUID
	updated time.Time
	touches int
	stopped bool
}

func newIdleFixture(run uuid.UUID) *idleFixture {
	t0 := time.Now().UTC()
	return &idleFixture{now: t0, run: run, updated: t0}
}

func (f *idleFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *idleFixture) advance(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	return f.now
}

func (f *idleFixture) touchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.touches
}

func (f *idleFixture) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// waitTouched waits (bounded, never fatal: a surface without a keepalive must reach the
// reaper assertion and fail THERE) until the run was touched at or after since.
func (f *idleFixture) waitTouched(since time.Time) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		ok := !f.updated.Before(since)
		f.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitTicking waits (bounded, never fatal) until the keepalive has ticked a few times, which
// also means the surface's own open-time touch is behind us.
func (f *idleFixture) waitTicking() {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && f.touchCount() < 3 {
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *idleFixture) ListRunningWithPolicy(context.Context) ([]lifecycle.RunSummary, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return nil, time.Time{}, nil
	}
	return []lifecycle.RunSummary{{ID: f.run, UpdatedAt: f.updated, PolicyAutoStopAfterSec: idleAutoStopSec}}, time.Time{}, nil
}

// StopRun applies the reaper's guard the way the Postgres adapter does: not when the run was
// touched after the scan's snapshot.
func (f *idleFixture) StopRun(_ context.Context, id uuid.UUID, notAfter time.Time) (lifecycle.StopOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != f.run || f.stopped || f.updated.After(notAfter) {
		return lifecycle.StopOutcome{}, nil
	}
	f.stopped = true
	return lifecycle.StopOutcome{Applied: true}, nil
}

func (f *idleFixture) StopRunMaxAge(context.Context, uuid.UUID, time.Time) (lifecycle.StopOutcome, error) {
	return lifecycle.StopOutcome{}, nil
}

// idleStore is a surface's store: everything delegates, except TouchRun moves updated_at to the
// virtual now.
type idleStore struct {
	store.Store
	f *idleFixture
}

func (s *idleStore) TouchRun(_ context.Context, id uuid.UUID) error {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if id == s.f.run {
		s.f.updated = s.f.now
		s.f.touches++
	}
	return nil
}

// reap runs one tick of the real reaper over the fixture and returns the run.autostop events.
func (f *idleFixture) reap(t *testing.T) []types.AuditEvent {
	t.Helper()
	rec := &sshTestRecorder{}
	if err := lifecycle.New(f, f, rec, lifecycle.Config{Now: f.clock}).Tick(context.Background()); err != nil {
		t.Fatalf("reaper tick: %v", err)
	}
	var out []types.AuditEvent
	for _, ev := range rec.snapshot() {
		if ev.Action == "run.autostop" {
			out = append(out, ev)
		}
	}
	return out
}

// pinHeld is steps 2-3: with the surface attached and ticking, the clock runs two hours past the
// threshold and the reaper must leave the run alone.
func (f *idleFixture) pinHeld(t *testing.T, surface string) {
	t.Helper()
	f.waitTicking()
	at := f.advance(2*time.Hour + lifecycle.TouchDebounce)
	f.waitTouched(at)
	if evs := f.reap(t); len(evs) != 0 || f.isStopped() {
		t.Fatalf("%s: the reaper stopped a run that is still attached, %s past auto_stop_after_sec (stopped=%v, events=%d); "+
			"its keepalive is not touching the run", surface, 2*time.Hour, f.isStopped(), len(evs))
	}
}

// pinDetached is step 4: once nothing is attached, idleness past the threshold stops the run.
func (f *idleFixture) pinDetached(t *testing.T, surface string) {
	t.Helper()
	f.advance(idleAutoStopSec*time.Second + lifecycle.TouchDebounce + time.Minute)
	if evs := f.reap(t); len(evs) != 1 || !f.isStopped() {
		t.Fatalf("%s: after detach plus the threshold the run was not stopped (stopped=%v, run.autostop events=%d)",
			surface, f.isStopped(), len(evs))
	}
}

const idleKeepalive = 10 * time.Millisecond

// TestIdleStop_BrowserAttachHeldOpenIsNotReaped pins the browser attach (attach.go).
func TestIdleStop_BrowserAttachHeldOpenIsNotReaped(t *testing.T) {
	srv, st, _, audit, run := holderTestServer(t)
	f := newIdleFixture(run.ID)
	srv.cfg.Store = &idleStore{Store: st, f: f}
	srv.keepaliveEvery = idleKeepalive
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	defer ts.Close()

	c := dialAttach(t, ts, srv, run.ID, holderOwner, "")
	readAttachMode(t, c)

	f.pinHeld(t, "browser attach")

	_ = c.Close(websocket.StatusNormalClosure, "")
	if waitForAudit(t, audit, run.ID, "session.detach", "success") == nil {
		t.Fatal("the browser attach never recorded session.detach")
	}
	f.pinDetached(t, "browser attach")
}

// TestIdleStop_SSHShellHeldOpenIsNotReaped pins an SSH shell (sshgateway_channels.go's
// bridgeSSHShell).
func TestIdleStop_SSHShellHeldOpenIsNotReaped(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	f := newIdleFixture(run.ID)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	fr := &sshFakeRunner{attachFn: func() (runner.Session, error) { return newFakeShellSession(), nil }}
	h := newSSHTestHarness(t, &idleStore{Store: st, f: f}, fr)
	h.srv.keepaliveEvery = idleKeepalive

	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()
	// A held stdin pipe: a session with none reads EOF at once, which ends the shell.
	if _, err := sess.StdinPipe(); err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	if waitForAudit(t, h.audit, run.ID, "session.attach", "success") == nil {
		t.Fatalf("the shell never attached; events=%s", auditDump(h.audit.snapshot(), run.ID))
	}

	f.pinHeld(t, "ssh shell")

	_ = sess.Close()
	if waitForAudit(t, h.audit, run.ID, "session.detach", "success") == nil {
		t.Fatalf("the shell never recorded session.detach; events=%s", auditDump(h.audit.snapshot(), run.ID))
	}
	f.pinDetached(t, "ssh shell")
}

// TestIdleStop_SSHExecChannelsHeldOpenAreNotReaped pins the exec, sftp-subsystem and -L
// channels, which share sshBridgeExecSession. Each holds a command that produces nothing until
// the test lets it finish, so the channel is open and silent, which is exactly the shape a long
// scp, a held tunnel or a slow remote build has.
func TestIdleStop_SSHExecChannelsHeldOpenAreNotReaped(t *testing.T) {
	cases := []struct {
		name  string
		audit string
		open  func(t *testing.T, client *ssh.Client) (closeClient func())
	}{
		{"exec", "ssh.exec", func(t *testing.T, client *ssh.Client) func() {
			sess, err := client.NewSession()
			if err != nil {
				t.Fatalf("new session: %v", err)
			}
			if err := sess.Start("make build"); err != nil {
				t.Fatalf("start exec: %v", err)
			}
			return func() { _ = sess.Close() }
		}},
		{"sftp", "ssh.sftp.transfer", func(t *testing.T, client *ssh.Client) func() {
			sess, err := client.NewSession()
			if err != nil {
				t.Fatalf("new session: %v", err)
			}
			if err := sess.RequestSubsystem("sftp"); err != nil {
				t.Fatalf("request sftp subsystem: %v", err)
			}
			return func() { _ = sess.Close() }
		}},
		{"-L forward", "ssh.forward", func(t *testing.T, client *ssh.Client) func() {
			conn, err := client.Dial("tcp", "127.0.0.1:9999")
			if err != nil {
				t.Fatalf("forward to sandbox loopback: %v", err)
			}
			return func() { _ = conn.Close() }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, run, principal := sshOwnedRunningRun(t)
			f := newIdleFixture(run.ID)
			priv, pub := mustSSHKeypair(t)
			st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
			release := make(chan struct{}) // closed to let the held command finish
			fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
				sess, w := fakeBlockingExecSession()
				go func() { <-release; _ = w.Close() }()
				return sess, nil
			}}
			h := newSSHTestHarness(t, &idleStore{Store: st, f: f}, fr)
			h.srv.keepaliveEvery = idleKeepalive
			client, err := sshDial(t, h, run.ID.String(), priv)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.Close()

			closeClient := tc.open(t, client)
			defer closeClient()
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()

			f.pinHeld(t, "ssh "+tc.name)

			close(release)
			closeClient()
			if waitForAudit(t, h.audit, run.ID, tc.audit, "success") == nil {
				t.Fatalf("the %s channel never finished; events=%s", tc.name, auditDump(h.audit.snapshot(), run.ID))
			}
			f.pinDetached(t, "ssh "+tc.name)
		})
	}
}
