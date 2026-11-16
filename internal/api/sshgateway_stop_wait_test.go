// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// stoppableGateway serves the harness's server on a second listener, with one
// context as both its BaseCtx and the context it serves on, as wardynd's
// rootCtx is; stop ends that context, which is a daemon stop. The returned
// harness dials that listener. WaitBackground's budget is cut to 10s so a stop
// that never ends fails the test instead of stalling it.
func stoppableGateway(t *testing.T, st *sshMemStore, fr *sshFakeRunner, rec audit.Recorder, configure ...func(*Config)) (*sshTestHarness, context.CancelFunc) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	h := newSSHTestHarness(t, st, fr, append([]func(*Config){func(c *Config) { c.BaseCtx, c.Audit = ctx, rec }}, configure...)...)
	h.srv.bgWaitBudget = 10 * time.Second
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = h.srv.ServeSSHGatewayOn(ctx, ln) }()
	return &sshTestHarness{srv: h.srv, addr: ln.Addr().String(), hostPub: h.hostPub, audit: h.audit}, stop
}

// stopRows records the action of each row once its write is over, after a
// 200ms pause for the action slow names, and refuses a row whose context has
// ended, as the store does (sshTestRecorder): after a stop the recorder chain
// would spool that row, so the write being over is what a stop must wait for.
// entered closes when the slow row's write begins.
type stopRows struct {
	slow    string
	entered chan struct{}
	once    sync.Once
	mu      sync.Mutex
	over    []string
}

func (r *stopRows) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action == r.slow {
		r.once.Do(func() { close(r.entered) })
		time.Sleep(200 * time.Millisecond)
	}
	r.mu.Lock()
	r.over = append(r.over, ev.Action)
	r.mu.Unlock()
	return ctx.Err()
}

func (r *stopRows) wrote(action string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.over, action)
}

// A daemon stop waits, inside WaitBackground, for the SSH gateway's
// connections and the doors they run, so the last audit row each writes is
// over (stored, or spooled once the stopped daemon's context refuses it)
// before the pool closes and the process exits: a connection's authentication
// row, a port forward's row and an sftp transfer's row. Catches: a connection
// or door started outside goBackground, whose row the exit cuts mid-write.
func TestSSHGateway_AStopWaitsForTheLastRowOfWhatItEnds(t *testing.T) {
	for _, c := range []struct {
		name, slow string
		open       func(t *testing.T, dial func() (*ssh.Client, error))
	}{
		{"a connection's authentication", "ssh.authenticate", func(t *testing.T, dial func() (*ssh.Client, error)) {
			go func() { _, _ = dial() }()
		}},
		{"a port forward", "ssh.forward", func(t *testing.T, dial func() (*ssh.Client, error)) {
			client, err := dial()
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			go func() { _, _ = client.Dial("tcp", "127.0.0.1:9999") }()
		}},
		{"an sftp transfer", "ssh.sftp.transfer", func(t *testing.T, dial func() (*ssh.Client, error)) {
			client, err := dial()
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			sess, err := client.NewSession()
			if err != nil {
				t.Fatalf("new session: %v", err)
			}
			go func() { _ = sess.RequestSubsystem("sftp") }()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, run, principal := sshOwnedRunningRun(t)
			priv, pub := mustSSHKeypair(t)
			st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
			bridging := make(chan struct{})
			first := sync.OnceFunc(func() { close(bridging) })
			fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
				out, w := io.Pipe()
				ended := make(chan struct{})
				closeOnce := sync.OnceFunc(func() { _ = w.Close(); close(ended) })
				return &runner.ExecSession{
					Stdout: readSignal{Reader: out, first: first},
					Close:  func() error { closeOnce(); return nil },
					Wait: func() (int, error) {
						<-ended
						return 1, fmt.Errorf("exec wait: %w", context.Canceled) // the session closed under the process: no exit read
					},
				}, nil
			}}
			rec := &stopRows{slow: c.slow, entered: make(chan struct{})}
			h, stop := stoppableGateway(t, st, fr, rec)
			c.open(t, func() (*ssh.Client, error) { return sshDial(t, h, run.ID.String(), priv) })
			ready := bridging
			if c.slow == "ssh.authenticate" {
				ready = rec.entered
			}
			select {
			case <-ready:
			case <-time.After(10 * time.Second):
				t.Fatal("what the stop is to end never started")
			}
			stop()
			h.srv.WaitBackground()
			if !rec.wrote(c.slow) {
				t.Fatalf("WaitBackground returned before the %s row's write was over", c.slow)
			}
		})
	}
}

// A daemon stop closes an SSH connection, so a door with no bridge running (a
// session channel that never asked for a shell, a command or a subsystem) ends
// with it, and the stop's wait (WaitBackground) does not wait on the client.
// Catches: a stop that leaves connections open, which holds that wait for its
// whole budget while a client keeps an idle channel.
func TestSSHGateway_AStopClosesAConnectionAndItsIdleChannel(t *testing.T) {
	st, run, principal := sshOwnedRunningRun(t)
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h, stop := stoppableGateway(t, st, &sshFakeRunner{}, &sshTestRecorder{})
	client, err := sshDial(t, h, run.ID.String(), priv)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := client.NewSession(); err != nil {
		t.Fatalf("new session: %v", err)
	}
	stop()
	closed := make(chan struct{})
	go func() { _ = client.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop left the connection open")
	}
	start := time.Now()
	h.srv.WaitBackground()
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("WaitBackground took %v after the stop; want it over once the connection closed", took)
	}
}

// outcomeRows keeps every row it is given, with its outcome, and refuses one
// whose context has ended, as the store does: a stop's rows reach it on the
// stopped daemon's context, and the chain after it would spool them.
type outcomeRows struct {
	mu     sync.Mutex
	events []types.AuditEvent
}

func (r *outcomeRows) Record(ctx context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	return ctx.Err()
}

func (r *outcomeRows) outcomes(action string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, ev := range r.events {
		if ev.Action == action {
			out = append(out, ev.Outcome)
		}
	}
	return out
}

// A daemon stop ends every session channel's door with the stop's own cause,
// before the connection is closed, so each running transfer is recorded as the
// failure it is and none is read as the client's close, a success. The channels
// of one connection and the close of that connection are racing contexts, which
// more open channels expose more often, so the check opens ten and repeats.
// Catches: a close that can land before a channel's context
// has been cancelled with the stop's cause (an AfterFunc on connCtx, which is a
// sibling of each channel's context, not their parent).
func TestSSHGateway_AStopRecordsEveryOpenChannelAsAFailure(t *testing.T) {
	const channels, rounds = 10, 30
	for round := range rounds {
		st, run, principal := sshOwnedRunningRun(t)
		priv, pub := mustSSHKeypair(t)
		st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal, PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
		reading := make(chan struct{}, channels)
		fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
			out, w := io.Pipe()
			ended := make(chan struct{})
			closeOnce := sync.OnceFunc(func() { _ = w.Close(); close(ended) })
			return &runner.ExecSession{
				Stdout: readSignal{Reader: out, first: sync.OnceFunc(func() {
					select {
					case reading <- struct{}{}:
					default:
					}
				})},
				Close: func() error { closeOnce(); return nil },
				Wait: func() (int, error) {
					<-ended
					return 1, fmt.Errorf("exec wait: %w", context.Canceled) // the session closed under the process: no exit read
				},
			}, nil
		}}
		rec := &outcomeRows{}
		h, stop := stoppableGateway(t, st, fr, rec, func(c *Config) { c.SSHMaxSessionsPerRun = channels })
		client, err := sshDial(t, h, run.ID.String(), priv)
		if err != nil {
			t.Fatalf("round %d: dial: %v", round, err)
		}
		for range channels {
			sess, err := client.NewSession()
			if err != nil {
				t.Fatalf("round %d: new session: %v", round, err)
			}
			go func() { _ = sess.RequestSubsystem("sftp") }()
		}
		for range channels {
			select {
			case <-reading:
			case <-time.After(10 * time.Second):
				t.Fatalf("round %d: a channel's transfer never started", round)
			}
		}
		stop()
		h.srv.WaitBackground()
		got := rec.outcomes("ssh.sftp.transfer")
		if len(got) != channels {
			t.Fatalf("round %d: %d transfer rows after the stop, want %d (%v)", round, len(got), channels, got)
		}
		for _, o := range got {
			if o != "failure" {
				t.Fatalf("round %d: a stop was recorded as %q for a transfer it cut, want failure (rows %v)", round, o, got)
			}
		}
		_ = client.Close()
	}
}
