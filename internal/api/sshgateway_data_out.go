// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/sshsftp"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sftpServerDeny is the sftp-server request names a data_out rule turns off for `wardyn sync`: read (file
// content) and readlink (a symlink's target). foldersync calls no ReadLink, and realpath stays on because the
// sync client's RealPath check needs it.
const sftpServerDeny = "read,readlink"

// sshDataOut is the run's data_out rule for one SSH connection, read once when it opens. A rule that cannot
// be read, or a run with no recorded policy, is blocked, and says so once in the log.
func (s *Server) sshDataOut(ctx context.Context, runID uuid.UUID) types.TransferRule {
	rule, found, err := s.runDataOutRule(ctx, runID)
	if err != nil || !found {
		slog.WarnContext(ctx, "wardynd: a run's data_out rule is unreadable; its SSH file transfer, commands and port forwards are refused",
			"run_id", runID, "found", found, "err", err)
		return types.TransferBlocked
	}
	return rule
}

// sshRefuseDataOut refuses a direct-tcpip channel when the connection's rule keeps content in, and says so on
// the channel's rejection and in the audit log.
func (s *Server) sshRefuseDataOut(ctx context.Context, runID uuid.UUID, principal string, ch ssh.NewChannel, rule types.TransferRule) bool {
	if rule.DataOut() == types.TransferAllowed {
		return false
	}
	s.auditDataOutDenied(ctx, runID, types.ActorHuman, principal, "port_forward", rule)
	_ = ch.Reject(ssh.Prohibited, fmt.Sprintf("wardyn: this run's data_out rule (%s) keeps content in, so port forwards are off", rule))
	return true
}

// sshDataOutRefused refuses a file transfer ("sftp") or a command ("exec") on g's channel when its rule keeps
// content in, writing the audit row and the reason to the channel's stderr; the caller then replies false. The
// shell (path "") is a display and is never refused.
func (s *Server) sshDataOutRefused(g *sshSessionGate, path string) bool {
	rule := g.slot.dataOut
	if path == "" || rule.DataOut() == types.TransferAllowed {
		return false
	}
	what := "file transfer"
	if path == "exec" {
		what = "commands"
	}
	s.auditDataOutDenied(g.ctx, g.runID, types.ActorHuman, g.principal, path, rule)
	sendChannelError(g.channel, fmt.Sprintf("wardyn: this run's data_out rule (%s) keeps content in, so SSH %s are off; the terminal still works", rule, what))
	return true
}

// sshSyncReplyGuard wraps a sync session's sftp-server output so that the first file-content reply ends the
// sync: a stock server started with -P read sends none, so this holds against an image's own sftp-server. The
// reply is not passed on; the audit row and the client's message follow.
func (s *Server) sshSyncReplyGuard(runID uuid.UUID, principal string, channel ssh.Channel, out io.Reader, rule types.TransferRule) io.Reader {
	var once sync.Once
	return &sshsftp.ReplyGuard{R: out, OnData: func() {
		once.Do(func() {
			s.auditDataOutDenied(s.cfg.BaseCtx, runID, types.ActorHuman, principal, "sync_down", rule)
			sendChannelError(channel, "wardyn: this run's data_out rule keeps content in, so `wardyn sync` only sends files to the run")
		})
	}}
}
