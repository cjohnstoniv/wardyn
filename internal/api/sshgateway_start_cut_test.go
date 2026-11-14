// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// cutsAtTheStartLock is a Locker whose wait for one run's start lock first runs cut, which ends the waiting
// context, and then answers as a wait that context ended does. Every other key is the wrapped Locker's.
type cutsAtTheStartLock struct {
	db.Locker
	key db.LockKey
	cut func()
}

func (l *cutsAtTheStartLock) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	if k != l.key {
		return l.Locker.Lock(ctx, k, wait)
	}
	l.cut()
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

// sshStartCutRig is a server whose SSH doors' start lock for run ends the door's context while it waits (cut), and
// whose audit refuses a row written on an ended context, as a store does.
func sshStartCutRig(t *testing.T, cut func(id uuid.UUID) func()) (*Server, *sshTestRecorder, types.AgentRun, string) {
	t.Helper()
	st, run, principal := sshOwnedRunningRun(t)
	rec := &sshTestRecorder{}
	srv := New(Config{BaseCtx: testBaseCtx(t), Store: st, Runner: &sshFakeRunner{}, Audit: rec})
	srv.locks.override = &cutsAtTheStartLock{Locker: db.NewLocalLocker(), key: runStartKey(run.ID), cut: cut(run.ID)}
	return srv, rec, run, principal
}

// A start cut because the door's context ended (the client gone, the daemon stopping) still records its failure
// row on each of the three SSH doors, with the cut in it: the row is written on the context's values, not its end,
// under one audit row's bound. Catches: the row written on the ended channel context, which a store refuses.
func TestSSHGateway_AStartFailureRowLandsWhenTheClientIsGone(t *testing.T) {
	for _, c := range []struct {
		action string
		door   func(srv *Server, ctx context.Context, runID uuid.UUID, principal string)
	}{
		{"ssh.sftp.transfer", func(srv *Server, ctx context.Context, runID uuid.UUID, principal string) {
			srv.bridgeSSHSFTP(ctx, runID, principal, &stubChannel{})
		}},
		{"ssh.exec", func(srv *Server, ctx context.Context, runID uuid.UUID, principal string) {
			srv.bridgeSSHExec(ctx, runID, principal, &stubChannel{}, "true", nil)
		}},
		{"ssh.forward", func(srv *Server, ctx context.Context, runID uuid.UUID, principal string) {
			srv.handleSSHDirectTCPIP(ctx, runID, principal, &forwardOpen{extra: ssh.Marshal(sshDirectTCPIPMsg{DestHost: "127.0.0.1", DestPort: 9999})})
		}},
	} {
		t.Run(c.action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			srv, rec, run, principal := sshStartCutRig(t, func(uuid.UUID) func() { return cancel })
			c.door(srv, ctx, run.ID, principal)
			ev := findAudit(rec.snapshot(), run.ID, c.action, "failure")
			if ev == nil {
				t.Fatalf("no %s failure row; events=%s", c.action, auditDump(rec.snapshot(), run.ID))
			}
			if d := sshAuditData(t, ev); d["error"] != lockUnavailableMsg {
				t.Errorf("row data = %v, want the cut start's error", d)
			}
		})
	}
}
