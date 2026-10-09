// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// sshSyncSubsystem is the subsystem `wardyn sync` requests: the sandbox's
	// own sftp-server started in a chosen directory (docs/SSH.md).
	sshSyncSubsystem = "wardyn-sync"
	// sshSyncDirEnv and sshSyncDirectionEnv are the only env names honoured,
	// and only on a channel that then requests sshSyncSubsystem.
	sshSyncDirEnv       = "WARDYN_SYNC_DIR"
	sshSyncDirectionEnv = "WARDYN_SYNC_DIRECTION"
	// sshSyncDirRoot bounds the start directory, and sshSyncDefaultDir is the
	// workspace the sandbox contract names when none is given.
	sshSyncDirRoot    = "/home/agent/"
	sshSyncDefaultDir = "/home/agent/work"
)

func sshSyncEnvName(name string) bool {
	return name == sshSyncDirEnv || name == sshSyncDirectionEnv
}

// sshSyncDir validates the client-offered start directory: absolute, under
// sshSyncDirRoot, no ".." segment, and no byte sftp-server would expand or a
// terminal would mistake ('%' starts one of its -d tokens). Empty is the
// default. The result is cleaned.
func sshSyncDir(v string) (string, error) {
	if v == "" {
		return sshSyncDefaultDir, nil
	}
	if !path.IsAbs(v) || strings.Contains(v, "%") || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("%s must be an absolute path without control characters or '%%'", sshSyncDirEnv)
	}
	for _, seg := range strings.Split(v, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%s must not contain a '..' segment", sshSyncDirEnv)
		}
	}
	clean := path.Clean(v)
	if !strings.HasPrefix(clean, sshSyncDirRoot) {
		return "", fmt.Errorf("%s must be under %s", sshSyncDirEnv, sshSyncDirRoot)
	}
	return clean, nil
}

// sshSyncDirection is what the client declared for the audit row; anything but
// push or pull is "unknown". It is the client's word, not something the
// gateway can observe: it only relays sftp bytes.
func sshSyncDirection(v string) string {
	if v == "push" || v == "pull" {
		return v
	}
	return "unknown"
}

// bridgeSSHSync runs the sandbox's own sftp-server started in the validated
// directory, and records one ssh.sync.transfer row when the channel ends. The
// directory is a start point, not a boundary: sftp-server reaches whatever the
// agent uid can.
func (s *Server) bridgeSSHSync(ctx context.Context, runID uuid.UUID, principal string, channel ssh.Channel, env map[string]string) {
	direction := sshSyncDirection(env[sshSyncDirectionEnv])
	dir, err := sshSyncDir(env[sshSyncDirEnv])
	data := func(extra map[string]any) []byte {
		extra["direction"] = direction
		if err == nil {
			extra["dir"] = dir
		}
		return mustJSON(extra)
	}
	if err != nil {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sync.transfer",
			runID.String(), "failure", data(map[string]any{"error": err.Error(), "bytes_in": 0, "bytes_out": 0})))
		sendChannelError(channel, "wardyn-sync: "+err.Error())
		return
	}
	run, msg := s.sshFreshRun(ctx, runID, principal)
	if msg != "" {
		sendChannelError(channel, msg)
		return
	}
	sess, reason, err := s.execSFTPServer(ctx, run, "-d", dir)
	if err != nil {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sync.transfer",
			runID.String(), "failure", data(map[string]any{"error": err.Error(), "bytes_in": 0, "bytes_out": 0})))
		sendChannelError(channel, reason)
		return
	}
	exit, bytesIn, bytesOut := s.sshBridgeExecSession(ctx, runID, principal, channel, sess, true)
	// BaseCtx, not ctx: see bridgeSSHExec's trailing-write comment.
	extra := map[string]any{"bytes_in": bytesIn, "bytes_out": bytesOut}
	outcome := "success"
	if exit != 0 {
		outcome = "failure"
		extra["error"] = fmt.Sprintf("sftp-server exited %d", exit)
	}
	s.recordAudit(s.cfg.BaseCtx, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sync.transfer",
		runID.String(), outcome, data(extra)))
}
