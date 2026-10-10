// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sftpServerPath is the sandbox binary the sftp subsystem execs — the
// documented BYOI image contract (docs/SSH.md).
const sftpServerPath = "/usr/lib/openssh/sftp-server"

// execSFTPServer starts the sandbox's own sftp-server (plus args) and, on a
// launch failure, returns the message the client sees: a BYOI image without
// the binary surfaces as a clean channel error naming the image contract
// (never a hang), and ErrExecStreamUnsupported maps to the shared honest
// message every primitive here uses.
func (s *Server) execSFTPServer(ctx context.Context, run types.AgentRun, args ...string) (*runner.ExecSession, string, error) {
	sess, err := s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{Argv: append([]string{sftpServerPath, "-e"}, args...)})
	if err == nil {
		return sess, "", nil
	}
	reason := sshExecStreamErrorMessage(err)
	if !errors.Is(err, runner.ErrExecStreamUnsupported) {
		reason = "sftp unavailable: the sandbox image has no " + sftpServerPath + " (" + err.Error() + ")"
	}
	return nil, reason, err
}

// bridgeSSHSFTP runs the sandbox's own sftp-server as the subsystem's backing
// process — no SFTP protocol reimplementation.
func (s *Server) bridgeSSHSFTP(ctx context.Context, runID uuid.UUID, principal string, channel ssh.Channel) {
	run, msg := s.sshFreshRun(ctx, runID, principal)
	if msg != "" {
		sendChannelError(channel, msg)
		return
	}
	sess, reason, err := s.execSFTPServer(ctx, run)
	if err != nil {
		s.recordStreamAudit(ctx, run.SandboxRef, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sftp.transfer",
			runID.String(), "failure", mustJSON(map[string]any{"error": err.Error()})))
		sendChannelError(channel, reason)
		return
	}
	_, bytesIn, bytesOut := s.sshBridgeExecSession(ctx, runID, principal, channel, sess, true)
	// BaseCtx, not ctx — see bridgeSSHExec's identical trailing-write FINDING
	// comment above (same shape, same connection-teardown race, same fix).
	// "bytes" keeps its pre-0.9 meaning (sandbox to client); bytes_in/bytes_out
	// are the explicit pair.
	s.recordStreamAudit(s.cfg.BaseCtx, run.SandboxRef, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sftp.transfer",
		runID.String(), "success", mustJSON(map[string]any{"bytes": bytesOut, "bytes_in": bytesIn, "bytes_out": bytesOut})))
}
