// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
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
// be read is blocked, and says so once in the log; a run with no recorded policy carries no rule, so it is
// allowed, as runDataOutRule reads it for every other path.
func (s *Server) sshDataOut(ctx context.Context, runID uuid.UUID) types.TransferRule {
	rule, _, err := s.runDataOutRule(ctx, runID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: a run's data_out rule is unreadable; its SSH file transfer, commands and port forwards are refused",
			"run_id", runID, "err", err)
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
// reply is not passed on. The audit row is written as soon as the guard sees it; the client's message and the
// channel's close wait for the Read that returns sshsftp.ErrData, so the replies before it reach the client.
func (s *Server) sshSyncReplyGuard(runID uuid.UUID, principal string, channel ssh.Channel, out io.Reader, rule types.TransferRule) io.Reader {
	guard := &sshsftp.ReplyGuard{R: out, OnData: func() {
		s.auditDataOutDenied(s.cfg.BaseCtx, runID, types.ActorHuman, principal, "sync_down", rule)
	}}
	return &syncStopOnData{r: guard, stop: func() {
		sendChannelError(channel, "wardyn: this run's data_out rule keeps content in, so `wardyn sync` only sends files to the run")
	}}
}

// syncStopOnData calls stop once, at the first Read of r that returns sshsftp.ErrData. A ReplyGuard returns it
// only once every byte before the DATA reply has been read, and the caller has written those on by then.
type syncStopOnData struct {
	r    io.Reader
	stop func()
	once sync.Once
}

func (d *syncStopOnData) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if errors.Is(err, sshsftp.ErrData) {
		d.once.Do(d.stop)
	}
	return n, err
}
