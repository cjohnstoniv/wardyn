// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// An exec or port-forward start its client's own close cuts records that close as its error, not the start lock's
// busy sentence, which names a cause that was not there; it stays a failure, as nothing ran. One the daemon's stop
// cuts, and one the fence's own wait bound refuses as the client closes, keep the lock's sentence. Catches: a cut
// start's error read as the lock's refusal whatever ended its wait.
func TestSSHGateway_AnExecOrForwardStartItsClientsCloseCutNamesTheClose(t *testing.T) {
	doors := []struct {
		action string
		door   func(srv *Server, ctx context.Context, runID uuid.UUID, principal string)
	}{
		{"ssh.exec", func(srv *Server, ctx context.Context, runID uuid.UUID, principal string) {
			srv.bridgeSSHExec(ctx, runID, principal, &stubChannel{}, "true", nil)
		}},
		{"ssh.forward", func(srv *Server, ctx context.Context, runID uuid.UUID, principal string) {
			srv.handleSSHDirectTCPIP(ctx, runID, principal, &forwardOpen{extra: ssh.Marshal(sshDirectTCPIPMsg{DestHost: "127.0.0.1", DestPort: 9999})})
		}},
	}
	for _, d := range doors {
		for _, c := range []struct {
			name               string
			shutdown, timedOut bool
			wantErr            string
		}{
			{"the client closed", false, false, errSSHChannelClosed.Error()},
			{"the daemon stopped", true, false, lockUnavailableMsg},
			{"the lock wait timed out as the client closed", false, true, lockUnavailableMsg},
		} {
			t.Run(d.action+", "+c.name, func(t *testing.T) {
				conn, stop := context.WithCancel(t.Context())
				defer stop()
				ctx, closeCtx := context.WithCancelCause(conn)
				defer closeCtx(nil)
				srv, rec, run, principal := sshStartCutRig(t, func() {
					if c.shutdown {
						stop()
					}
					closeCtx(errSSHChannelClosed) // the client's end, as handleSSHConn's and handleSSHSessionChannel's contexts end
				})
				if c.timedOut {
					srv.locks.override.(*cutsAtTheStartLock).err = db.ErrLockBusy
				}
				d.door(srv, ctx, run.ID, principal)
				ev := findAudit(rec.snapshot(), run.ID, d.action, "failure")
				if ev == nil {
					t.Fatalf("no %s failure row; events=%s", d.action, auditDump(rec.snapshot(), run.ID))
				}
				if got := sshAuditData(t, ev)["error"]; got != c.wantErr {
					t.Errorf("row error = %v, want %q", got, c.wantErr)
				}
			})
		}
	}
}
