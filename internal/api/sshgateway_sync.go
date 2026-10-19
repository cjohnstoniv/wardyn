// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
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
// Deny is the organisation's ignore patterns (WARDYN_SYNC_DENY), which
// `wardyn sync` applies above every ignore file in the tree, omitted when unset.
type runSyncView struct {
	Open []runSyncOpen `json:"open"`
	Deny []string      `json:"deny,omitempty"`
}

// Bounds on WARDYN_SYNC_DENY.
const (
	maxSyncDenyPatterns = 64
	maxSyncDenyPattern  = 256
)

// SplitSyncDeny splits WARDYN_SYNC_DENY on commas, trimming spaces and
// dropping empty items. It checks nothing: ParseSyncDeny is boot's check.
func SplitSyncDeny(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ParseSyncDeny is SplitSyncDeny, refusing more than 64 patterns, one over 256
// bytes, invalid UTF-8 or a control character.
func ParseSyncDeny(v string) ([]string, error) {
	out := SplitSyncDeny(v)
	if len(out) > maxSyncDenyPatterns {
		return nil, fmt.Errorf("WARDYN_SYNC_DENY: %d patterns, at most %d", len(out), maxSyncDenyPatterns)
	}
	for _, p := range out {
		if len(p) > maxSyncDenyPattern || !utf8.ValidString(p) || strings.ContainsFunc(p, unicode.IsControl) {
			return nil, fmt.Errorf("WARDYN_SYNC_DENY: pattern %q must be at most %d bytes of UTF-8 with no control characters", p, maxSyncDenyPattern)
		}
	}
	return out, nil
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
	return runSyncView{Open: open, Deny: s.cfg.SyncDeny}
}

// bridgeSSHSync runs the sandbox's own sftp-server started in the validated
// directory, and records one ssh.sync.transfer row when the channel ends. The
// directory is a start point, not a boundary: sftp-server reaches whatever the
// agent uid can.
func (s *Server) bridgeSSHSync(ctx context.Context, runID uuid.UUID, principal string, channel ssh.Channel, env map[string]string, pass *sshSyncPass) {
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
		extra["laptop_deletes_reported"] = pass.laptopDeletes()
		s.recordStreamAudit(s.cfg.BaseCtx, sandboxRef, s.auditEvent(&runID, types.ActorHuman, principal, "ssh.sync.transfer",
			runID.String(), outcome, mustJSON(extra)))
	}
	fail := func(errText, clientMsg string) {
		record("failure", map[string]any{"error": errText, "bytes_in": 0, "bytes_out": 0, "remove_requests": 0, "rename_requests": 0})
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
	pass.start(dir)
	ops := &sftpOpCounter{Channel: channel}
	exit, bytesIn, bytesOut := s.sshBridgeExecSession(ctx, runID, principal, ops, withSFTPLaunchNote(sess, channel.Stderr()), true)
	removes, renames := ops.counts()
	extra := map[string]any{"bytes_in": bytesIn, "bytes_out": bytesOut, "remove_requests": removes, "rename_requests": renames}
	outcome := "success"
	if exit != 0 {
		outcome = "failure"
		extra["error"] = fmt.Sprintf("sftp-server exited %d", exit)
	}
	record(outcome, extra)
}

// sshSyncPassRequest is the channel request `wardyn sync` sends after each pass
// on a wardyn-sync channel: the client's own account of the pass, which the
// gateway cannot see in the sftp bytes. Its laptop deletes are summed into the
// session's ssh.sync.transfer row, and a complete pass (or a failed one) is the
// first sync of a run's local folder landing (or failing; run_local_folder.go).
const sshSyncPassRequest = "wardyn-sync-pass"

// maxSyncPassReport bounds one report's JSON.
const maxSyncPassReport = 4096

type sshSyncPassReport struct {
	Complete        bool   `json:"complete"`
	Error           string `json:"error"`
	DeletedOnLaptop int    `json:"deleted_on_laptop"`
}

// sshSyncPass is one wardyn-sync channel's reports. Dir is empty until the
// bridge has started sftp-server, and a report before then is refused.
type sshSyncPass struct {
	mu      sync.Mutex
	dir     string
	deletes int
}

func (p *sshSyncPass) start(dir string) {
	p.mu.Lock()
	p.dir = dir
	p.mu.Unlock()
}

func (p *sshSyncPass) laptopDeletes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deletes
}

// sshSyncPassReported takes one report and says whether it was accepted.
func (s *Server) sshSyncPassReported(ctx context.Context, runID uuid.UUID, principal string, pass *sshSyncPass, payload []byte) bool {
	var m struct{ Report string }
	if ssh.Unmarshal(payload, &m) != nil || len(m.Report) > maxSyncPassReport {
		return false
	}
	var r sshSyncPassReport
	if json.Unmarshal([]byte(m.Report), &r) != nil || r.DeletedOnLaptop < 0 {
		return false
	}
	pass.mu.Lock()
	dir := pass.dir
	if dir != "" {
		pass.deletes = min(pass.deletes+r.DeletedOnLaptop, maxSyncEntriesReported)
	}
	pass.mu.Unlock()
	if dir == "" {
		return false
	}
	if r.Complete || r.Error != "" {
		s.localFolderPassed(ctx, runID, principal, dir, r.Complete, r.Error)
	}
	return true
}

// maxSyncEntriesReported caps the summed laptop deletes one session records.
const maxSyncEntriesReported = 1 << 30

// sftpOpCounter counts the remove, rmdir and rename requests the client sends
// through a wardyn-sync channel, by reading the sftp packet framing (a 4-byte
// length, then the type) on the bytes relayed into the sandbox. These are
// requests, not outcomes: sftp-server answers them, the gateway does not.
type sftpOpCounter struct {
	ssh.Channel
	mu      sync.Mutex
	head    []byte // the current packet's first bytes, its length included, up to sftpPeek
	left    uint32 // bytes of the current packet not yet seen after its length
	removes int
	renames int
}

const (
	sftpRemove     = 13
	sftpRmdir      = 15
	sftpRename     = 18
	sftpExtended   = 200
	sftpPosixMove  = "posix-rename@openssh.com"
	sftpPeek       = 4 + 1 + 4 + 4 + len(sftpPosixMove) // length, type, request id, name length, name
	sftpLengthSize = 4
)

func (c *sftpOpCounter) Read(p []byte) (int, error) {
	n, err := c.Channel.Read(p)
	c.mu.Lock()
	c.feed(p[:n])
	c.mu.Unlock()
	return n, err
}

func (c *sftpOpCounter) feed(p []byte) {
	for len(p) > 0 {
		if len(c.head) < sftpLengthSize {
			take := min(sftpLengthSize-len(c.head), len(p))
			c.head, p = append(c.head, p[:take]...), p[take:]
			if len(c.head) == sftpLengthSize {
				if c.left = binary.BigEndian.Uint32(c.head); c.left == 0 {
					c.head = c.head[:0]
				}
			}
			continue
		}
		take := int(min(uint64(c.left), uint64(len(p))))
		if room := sftpPeek - len(c.head); room > 0 {
			c.head = append(c.head, p[:min(room, take)]...)
		}
		p, c.left = p[take:], c.left-uint32(take)
		if c.left == 0 {
			c.count(c.head[sftpLengthSize:])
			c.head = c.head[:0]
		}
	}
}

func (c *sftpOpCounter) count(body []byte) {
	switch {
	case len(body) == 0:
	case body[0] == sftpRemove, body[0] == sftpRmdir:
		c.removes++
	case body[0] == sftpRename:
		c.renames++
	case body[0] == sftpExtended && len(body) >= 9:
		if n := binary.BigEndian.Uint32(body[5:9]); int(n) == len(sftpPosixMove) && string(body[9:]) == sftpPosixMove {
			c.renames++
		}
	}
}

func (c *sftpOpCounter) counts() (removes, renames int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.removes, c.renames
}
