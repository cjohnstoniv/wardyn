// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// What a new SSH channel must pass before it bridges: a free per-run slot and a run that is live.
// Split out of sshgateway.go for scripts/check-file-size.sh.

const (
	sshCapReasonChannels  = "per-run concurrent channel cap"
	sshCapReasonSync      = "per-run concurrent sync channel cap"
	sshCapReasonPrincipal = "per-person connection cap"
)

// sshAcquireCount increments counts[runID] unless it is already at limit.
// Callers that get true MUST decrement it exactly once (sshReleaseCount).
func (s *Server) sshAcquireCount(counts *map[uuid.UUID]int, runID uuid.UUID, limit int) bool {
	s.sshSessionsMu.Lock()
	defer s.sshSessionsMu.Unlock()
	if *counts == nil {
		*counts = map[uuid.UUID]int{}
	}
	if (*counts)[runID] >= limit {
		return false
	}
	(*counts)[runID]++
	return true
}

func (s *Server) sshReleaseCount(counts map[uuid.UUID]int, runID uuid.UUID) {
	s.sshSessionsMu.Lock()
	defer s.sshSessionsMu.Unlock()
	counts[runID]--
	if counts[runID] <= 0 {
		delete(counts, runID)
	}
}

// sshAcquireSession reports whether runID may open one more concurrent SSH
// channel of the shared kind — "session" (shell/exec/sftp) OR "direct-tcpip"
// (-L forward), same counter — bounded by Config.SSHMaxSessionsPerRun. Callers
// that get true MUST call sshReleaseSession exactly once when that channel's
// handling ends.
func (s *Server) sshAcquireSession(runID uuid.UUID) bool {
	return s.sshAcquireCount(&s.sshSessions, runID, s.cfg.SSHMaxSessionsPerRun)
}

func (s *Server) sshReleaseSession(runID uuid.UUID) { s.sshReleaseCount(s.sshSessions, runID) }

func (s *Server) sshAcquireSync(runID uuid.UUID) bool {
	return s.sshAcquireCount(&s.sshSyncSessions, runID, maxSSHSyncSessionsPerRun)
}

func (s *Server) sshReleaseSync(runID uuid.UUID) { s.sshReleaseCount(s.sshSyncSessions, runID) }

// sshChannelSlot is the per-run budget one "session" channel holds: the shared
// one (a shell, exec, sftp), or the separate wardyn-sync one. Touched only from
// the channel's own goroutine, so it needs no lock.
type sshChannelSlot struct {
	s       *Server
	runID   uuid.UUID
	sync    bool
	dataOut types.TransferRule // the run's data_out rule when the connection opened
}

// sshOpenSlot admits a new "session" channel. The shared budget first; when it
// is full the channel is admitted on a sync slot instead, because nothing says
// what a session channel is for until its first request — refusing it here
// would let four idle shells starve every sync. Nil when both are full.
func (s *Server) sshOpenSlot(runID uuid.UUID) *sshChannelSlot {
	if s.sshAcquireSession(runID) {
		return &sshChannelSlot{s: s, runID: runID}
	}
	if s.sshAcquireSync(runID) {
		return &sshChannelSlot{s: s, runID: runID, sync: true}
	}
	return nil
}

// toSync moves the channel onto a sync slot for a wardyn-sync request, freeing
// its shared one. False (slot unchanged) when the sync cap is full.
func (sl *sshChannelSlot) toSync() bool {
	if sl.sync {
		return true
	}
	if !sl.s.sshAcquireSync(sl.runID) {
		return false
	}
	sl.s.sshReleaseSession(sl.runID)
	sl.sync = true
	return true
}

func (sl *sshChannelSlot) release() {
	if sl.sync {
		sl.s.sshReleaseSync(sl.runID)
		return
	}
	sl.s.sshReleaseSession(sl.runID)
}

// sshFreshRun re-fetches run fresh (state may have changed since the SSH
// connection authenticated — auth checks OWNERSHIP, which is immutable, but
// not liveness) and fails closed unless a Runner is wired AND the run is
// RUNNING with a live sandbox — the same precondition handleAttachWS
// enforces before every attach. The nil-Runner guard lives in liveRunForExec, which this calls first, not in each
// of the four callers (bridgeSSHShell/bridgeSSHExec/bridgeSSHSFTP/
// handleSSHDirectTCPIP all call this first): under the supported `-runner
// none` (headless) mode, s.cfg.Runner is a nil INTERFACE value, and calling
// ANY method on it — Attach, ExecStream — panics immediately (no concrete
// type to dispatch to); one guard here closes that for every bridge instead
// of three call sites that could individually forget it.
//
// Returns a non-empty, caller-facing message on failure; callers must return
// immediately and surface it however fits their channel's lifecycle stage
// (sendChannelError on an already-accepted channel, NewChannel.Reject
// otherwise).
func (s *Server) sshFreshRun(ctx context.Context, runID uuid.UUID, principal string) (types.AgentRun, string) {
	run, msg := s.liveRunForExec(ctx, runID, sshLiveRunWords)
	if msg != "" {
		return types.AgentRun{}, msg
	}
	// sshAuth refuses a background task at the door; this is the fresh read
	// every channel makes, so a connection opened before the run's shape was
	// read, or a bridge called on its own, still refuses. It comes before the
	// thaw, so a refused channel never resumes a paused run.
	if rf := backgroundOnlyRefusal(run, "SSH access"); rf != nil {
		return types.AgentRun{}, rf.body.Error
	}
	if msg := s.thawForCommand(ctx, run, principal); msg != "" {
		return types.AgentRun{}, msg
	}
	return run, ""
}

// liveRunWords are one door's refusals for a run that cannot take an exec now; notRunning takes the run's state.
type liveRunWords struct{ noRunner, notFound, notRunning, ended string }

// sshLiveRunWords are an SSH channel's refusals, as the gateway has always written them.
var sshLiveRunWords = liveRunWords{
	noRunner:   "no runner configured; ssh unavailable",
	notFound:   "run not found",
	notRunning: "run is not RUNNING; ssh unavailable (state=%s)",
	ended:      "run has ended; ssh unavailable",
}

// liveRunForExec is the fresh read every path that execs into a sandbox makes first (sshFreshRun, and
// POST /runs/{id}/exec's execFreshRun): the run, refused in words' terms unless a Runner is wired and the run is
// RUNNING with a sandbox and not kept. It holds the nil-Runner guard: under `-runner none` s.cfg.Runner is a nil
// interface, and any method call on it panics. It does not refuse a background run: each caller decides that.
func (s *Server) liveRunForExec(ctx context.Context, runID uuid.UUID, words liveRunWords) (types.AgentRun, string) {
	if s.cfg.Runner == nil {
		return types.AgentRun{}, words.noRunner
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return types.AgentRun{}, words.notFound
	}
	if run.State != types.RunRunning || run.SandboxRef == "" {
		return types.AgentRun{}, fmt.Sprintf(words.notRunning, run.State)
	}
	// A kept run is RUNNING with its agent stopped: nothing to attach to.
	if runIsKept(run) {
		return types.AgentRun{}, words.ended
	}
	return run, ""
}

// thawForCommand thaws run before an exec into it: "" when the exec may go on, else thawRefusal's words. Every
// exec goes into the sandbox, which the daemon refuses while it is paused, and a person connecting is presence, so
// a paused run is resumed (run_pause.go).
func (s *Server) thawForCommand(ctx context.Context, run types.AgentRun, principal string) string {
	return thawRefusal(s.thawForExec(ctx, run, types.ActorHuman, principal, "presence"))
}
