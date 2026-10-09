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

// bridgeSSHSFTP runs the sandbox's own sftp-server as the subsystem's backing
// process — no SFTP protocol reimplementation. A BYOI image without the
// binary surfaces as a clean channel error naming the image contract (never
// a hang): ErrExecStreamUnsupported maps to the shared honest message every
// primitive here uses, and any other ExecStream launch failure names the
// missing path directly.
func (s *Server) bridgeSSHSFTP(ctx context.Context, runID uuid.UUID, principal string, channel ssh.Channel) {
	run, msg := s.sshFreshRun(ctx, runID, principal)
	if msg != "" {
		sendChannelError(channel, msg)
		return
	}
	sess, err := s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{Argv: []string{sftpServerPath, "-e"}})
	if err != nil {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sftp.transfer",
			runID.String(), "failure", mustJSON(map[string]any{"error": err.Error()})))
		reason := sshExecStreamErrorMessage(err)
		if !errors.Is(err, runner.ErrExecStreamUnsupported) {
			reason = "sftp unavailable: the sandbox image has no " + sftpServerPath + " (" + err.Error() + ")"
		}
		sendChannelError(channel, reason)
		return
	}
	_, bytesOut := s.sshBridgeExecSession(ctx, runID, principal, channel, sess, true)
	// BaseCtx, not ctx — see bridgeSSHExec's identical trailing-write FINDING
	// comment above (same shape, same connection-teardown race, same fix).
	s.recordAudit(s.cfg.BaseCtx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sftp.transfer",
		runID.String(), "success", mustJSON(map[string]any{"bytes": bytesOut})))
}
