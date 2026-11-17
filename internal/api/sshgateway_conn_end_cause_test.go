// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// waitsOutTheConnection is a Locker whose wait for one run's start lock lasts until the waiting context ends; it
// then reports that context's cause on cause and answers as a wait that context ended does. Every other key is
// the wrapped Locker's.
type waitsOutTheConnection struct {
	db.Locker
	key     db.LockKey
	once    sync.Once
	waiting chan struct{}
	cause   chan error
}

func (l *waitsOutTheConnection) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	if k != l.key {
		return l.Locker.Lock(ctx, k, wait)
	}
	l.once.Do(func() { close(l.waiting) })
	<-ctx.Done()
	select {
	case l.cause <- context.Cause(ctx):
	default:
	}
	return nil, nil, ctx.Err()
}

// startLockStore is the gateway test store with its locks answered by l (Server.locker reads a store's Locker).
type startLockStore struct {
	*sshMemStore
	l db.Locker
}

func (s startLockStore) Locker() db.Locker { return s.l }

// A connection that ends while the daemon runs ends its doors' contexts as the client's close
// (errSSHChannelClosed), whichever of the connection's end and a channel's own end the gateway sees first. A port
// forward's start, which waits on the connection's context, then ends with that cause; an sftp start the close
// cut is the client's end, a success with nothing moved. Catches: the connection's context ended with no cause,
// which reads as a daemon stop.
func TestSSHGateway_AConnectionEndIsTheClientsClose(t *testing.T) {
	for _, c := range []struct {
		name, action, outcome string
		wantErr               any
		open                  func(t *testing.T, client *ssh.Client)
	}{
		{"a forward's start", "ssh.forward", "failure", errSSHChannelClosed.Error(), func(t *testing.T, client *ssh.Client) {
			go func() { _, _ = client.Dial("tcp", "127.0.0.1:9999") }()
		}},
		{"an sftp start", "ssh.sftp.transfer", "success", nil, func(t *testing.T, client *ssh.Client) {
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
			l := &waitsOutTheConnection{Locker: db.NewLocalLocker(), key: runStartKey(run.ID), waiting: make(chan struct{}), cause: make(chan error, 1)}
			fr := &sshFakeRunner{execFn: func(runner.ExecSpec) (*runner.ExecSession, error) {
				return nil, errors.New("the start lock never answered, so no process may start")
			}}
			h := newSSHTestHarness(t, startLockStore{st, l}, fr)
			client, err := sshDial(t, h, run.ID.String(), priv)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			c.open(t, client)
			select {
			case <-l.waiting:
			case <-time.After(10 * time.Second):
				t.Fatal("the door never waited on its start lock")
			}
			_ = client.Close()
			select {
			case cause := <-l.cause:
				if !errors.Is(cause, errSSHChannelClosed) {
					t.Errorf("the start lock wait ended with cause %v, want the client's close (%v)", cause, errSSHChannelClosed)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the start lock wait never ended after the client closed")
			}
			ev := waitForAudit(t, h.audit, run.ID, c.action, c.outcome)
			if ev == nil {
				t.Fatalf("no %s %s row; events=%s", c.action, c.outcome, auditDump(h.audit.snapshot(), run.ID))
			}
			if d := sshAuditData(t, ev); d["error"] != c.wantErr {
				t.Errorf("row data = %v, want error %v", d, c.wantErr)
			}
		})
	}
}

// readFunc is a process's stdout whose reads a test scripts.
type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

// TestSSHGateway_BridgeOutcomeIsJudgedWhenOutputEnds drives noteBridgeEnd the way
// sshBridgeExecSession does (Stdout to its end, then Wait): a client that closes
// after the process's output ended, while the bridge waits out its stderr, did
// not end a process that had already failed; one that closed while the process
// was still writing did.
func TestSSHGateway_BridgeOutcomeIsJudgedWhenOutputEnds(t *testing.T) {
	for name, tc := range map[string]struct {
		cancelAtRead bool // the client closes before stdout's read returns
		cancelAtWait bool // the client closes between stdout's end and Wait
		stdoutErr    error
		code         int
		want         string
	}{
		"exits 0, no close": {stdoutErr: io.EOF, want: "success"},
		"exits 1 and the client closes in the stderr wait": {stdoutErr: io.EOF, cancelAtWait: true, code: 1, want: "failure"},
		"the client closes while the process writes":       {cancelAtRead: true, stdoutErr: errors.New("closed"), cancelAtWait: true, code: 1, want: "success"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			sess := &runner.ExecSession{
				Stdout: readFunc(func([]byte) (int, error) {
					if tc.cancelAtRead {
						cancel(errSSHChannelClosed)
					}
					return 0, tc.stdoutErr
				}),
				Wait: func() (int, error) {
					if tc.cancelAtWait {
						cancel(errSSHChannelClosed)
					}
					return tc.code, nil
				},
			}
			var end bridgeEnd
			noted := noteBridgeEnd(ctx, sess, &end)
			_, _ = io.Copy(io.Discard, noted.Stdout)
			code, err := noted.Wait()
			if err != nil {
				t.Fatalf("Wait = %v", err)
			}
			if got := bridgeOutcome(code, end, "sftp-server", map[string]any{}); got != tc.want {
				t.Errorf("outcome = %q, want %q (end=%+v)", got, tc.want, end)
			}
		})
	}
}
