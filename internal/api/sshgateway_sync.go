// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
// sshSyncDirRoot, no ".." segment, valid UTF-8 with no control character (C0 or
// C1), and no '%' (it starts one of sftp-server's -d tokens). Empty is the
// default. The result is cleaned.
func sshSyncDir(v string) (string, error) {
	if v == "" {
		return sshSyncDefaultDir, nil
	}
	if !path.IsAbs(v) || strings.Contains(v, "%") || !utf8.ValidString(v) || strings.ContainsFunc(v, unicode.IsControl) {
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

// runSyncOpen is one live wardyn-sync channel as GET /runs/{id} shows it.
type runSyncOpen struct {
	Dir      string    `json:"dir"`
	OpenedAt time.Time `json:"opened_at"`
}

// runSyncView is the run detail's sync field: the sessions open now on THIS
// replica, from the per-run sync budget (a replica that does not hold the
// channel shows none). Dir is the directory the client requested, not proof
// the sandbox started there. Ended sessions are the ssh.sync.transfer rows.
type runSyncView struct {
	Open []runSyncOpen `json:"open"`
}

// sshSyncOpened records a live sync channel and returns the func that removes it.
func (s *Server) sshSyncOpened(runID uuid.UUID, dir string) func() {
	e := &runSyncOpen{Dir: dir, OpenedAt: time.Now().UTC()}
	s.sshSessionsMu.Lock()
	defer s.sshSessionsMu.Unlock()
	if s.sshSyncOpen == nil {
		s.sshSyncOpen = map[uuid.UUID][]*runSyncOpen{}
	}
	s.sshSyncOpen[runID] = append(s.sshSyncOpen[runID], e)
	return func() {
		s.sshSessionsMu.Lock()
		defer s.sshSessionsMu.Unlock()
		if left := slices.DeleteFunc(s.sshSyncOpen[runID], func(o *runSyncOpen) bool { return o == e }); len(left) > 0 {
			s.sshSyncOpen[runID] = left
		} else {
			delete(s.sshSyncOpen, runID)
		}
	}
}

// runSyncOpenView lists runID's open sync channels, oldest first, never nil.
func (s *Server) runSyncOpenView(runID uuid.UUID) runSyncView {
	s.sshSessionsMu.Lock()
	open := make([]runSyncOpen, 0, len(s.sshSyncOpen[runID]))
	for _, e := range s.sshSyncOpen[runID] {
		open = append(open, *e)
	}
	s.sshSessionsMu.Unlock()
	slices.SortFunc(open, func(a, b runSyncOpen) int {
		if c := a.OpenedAt.Compare(b.OpenedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Dir, b.Dir)
	})
	return runSyncView{Open: open}
}

// bridgeSSHSync runs the sandbox's own sftp-server started in the validated
// directory, and records one ssh.sync.transfer row when the channel ends. The
// directory is a start point, not a boundary: sftp-server reaches whatever the
// agent uid can.
func (s *Server) bridgeSSHSync(ctx context.Context, runID uuid.UUID, principal string, channel ssh.Channel, env map[string]string) {
	direction := sshSyncDirection(env[sshSyncDirectionEnv])
	dir, dirErr := sshSyncDir(env[sshSyncDirEnv])
	// BaseCtx, not ctx: the client may close the channel the moment it sees the
	// subsystem ack, which cancels ctx; see bridgeSSHExec's trailing-write comment.
	var sandboxRef string
	record := func(outcome string, extra map[string]any) {
		extra["direction"] = direction
		if dirErr == nil {
			extra["dir"] = dir
		}
		s.recordStreamAudit(s.cfg.BaseCtx, sandboxRef, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sync.transfer",
			runID.String(), outcome, mustJSON(extra)))
	}
	fail := func(errText, clientMsg string) {
		record("failure", map[string]any{"error": errText, "bytes_in": 0, "bytes_out": 0})
		sendChannelError(channel, clientMsg)
	}
	if dirErr != nil {
		fail(dirErr.Error(), "wardyn-sync: "+dirErr.Error())
		return
	}
	run, msg := s.sshFreshRun(ctx, runID, principal)
	if msg != "" {
		fail(msg, msg)
		return
	}
	sandboxRef = run.SandboxRef
	sess, reason, err := s.execSFTPServer(ctx, run, "-d", dir)
	if err != nil {
		fail(err.Error(), reason)
		return
	}
	defer s.sshSyncOpened(runID, dir)()
	exit, bytesIn, bytesOut := s.sshBridgeExecSession(ctx, runID, principal, channel, sess, true)
	extra := map[string]any{"bytes_in": bytesIn, "bytes_out": bytesOut}
	outcome := "success"
	if exit != 0 {
		outcome = "failure"
		extra["error"] = fmt.Sprintf("sftp-server exited %d", exit)
	}
	record(outcome, extra)
}
