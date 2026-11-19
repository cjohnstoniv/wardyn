// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// acceptedForward is a direct-tcpip open the gateway accepts onto ch. Its request stream is reqs, which the test
// closes only after the connection's end, as x/crypto closes a channel's requests at its close or the connection's.
type acceptedForward struct {
	forwardOpen
	ch   ssh.Channel
	reqs chan *ssh.Request
}

func (c *acceptedForward) Accept() (ssh.Channel, <-chan *ssh.Request, error) {
	return c.ch, c.reqs, nil
}

// readSignal is a bridged process's stdout that runs first on the bridge's first read of it: the start, with
// every refusal it can make, is over by then.
type readSignal struct {
	io.Reader
	first func()
}

func (r readSignal) Read(p []byte) (int, error) {
	r.first()
	return r.Reader.Read(p)
}

// A daemon stop under a running sftp transfer or port forward ends the bridge's context with the daemon's own
// cause, not the client's: the process it cut left no exit, so the row is a failure that says so. The client's
// close under the same running process stays the client's end, a success that names no error. Catches: any end of
// the bridge's context read as the client's close, which records a transfer the stop cut as a success.
func TestSSHGateway_ADaemonStopUnderARunningBridgeIsAFailure(t *testing.T) {
	sftp := func(srv *Server, ctx context.Context, runID uuid.UUID, principal string, _ chan *ssh.Request) {
		srv.bridgeSSHSFTP(ctx, runID, principal, &stubChannel{})
	}
	forward := func(srv *Server, ctx context.Context, runID uuid.UUID, principal string, reqs chan *ssh.Request) {
		srv.handleSSHDirectTCPIP(ctx, runID, principal, &acceptedForward{
			forwardOpen: forwardOpen{extra: ssh.Marshal(sshDirectTCPIPMsg{DestHost: "127.0.0.1", DestPort: 9999})},
			ch:          &stubChannel{},
			reqs:        reqs,
		})
	}
	for _, c := range []struct {
		name, action string
		shutdown     bool
		outcome      string
		wantErr      any
		door         func(srv *Server, ctx context.Context, runID uuid.UUID, principal string, reqs chan *ssh.Request)
	}{
		{"sftp, the daemon stopped", "ssh.sftp.transfer", true, "failure", "sftp-server ended without an exit status", sftp},
		{"sftp, the client closed", "ssh.sftp.transfer", false, "success", nil, sftp},
		{"forward, the daemon stopped", "ssh.forward", true, "failure", "the forward relay ended without an exit status", forward},
		{"forward, the client closed", "ssh.forward", false, "success", nil, forward},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, run, principal := sshOwnedRunningRun(t)
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
			rec := &sshTestRecorder{}
			srv := New(Config{BaseCtx: testBaseCtx(t), Store: st, Runner: fr, Audit: rec})
			conn, stop := context.WithCancel(t.Context())
			defer stop()
			ctx, closeCtx := context.WithCancelCause(conn)
			defer closeCtx(nil)
			done := make(chan struct{})
			reqs := make(chan *ssh.Request)
			go func() { defer close(done); c.door(srv, ctx, run.ID, principal, reqs) }()
			select {
			case <-bridging:
			case <-time.After(10 * time.Second):
				t.Fatal("the bridge never read its process's output")
			}
			if c.shutdown {
				stop()
			}
			closeCtx(errSSHChannelClosed)
			close(reqs)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the bridge never ended after its context did")
			}
			ev := findAudit(rec.snapshot(), run.ID, c.action, c.outcome)
			if ev == nil {
				t.Fatalf("no %s %s row; events=%s", c.action, c.outcome, auditDump(rec.snapshot(), run.ID))
			}
			if got := sshAuditData(t, ev)["error"]; got != c.wantErr {
				t.Errorf("row error = %v, want %v", got, c.wantErr)
			}
		})
	}
}
