// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Pause and resume (long-holds design rev 4, §3, RL-7). When nobody is at a run
// its agent container is frozen in place (runner.Freezer) and the run keeps its
// RunState, memory, files and proxy. Only the agent is frozen: the proxy keeps
// renewing its token and answering decisions, and a paused run is not
// contained — kill still is.
//
// active_at is the presence clock. A person typing or opening an exec moves it
// (and thaws a paused run first); the agent's egress decisions and the bytes
// its proxy moves move it too, but never thaw: the agent cannot act while it is
// frozen, so anything arriving then is traffic already in flight.
//
// A run pauses on one of two rules, and only on a confinement class whose
// freeze is verified (runner.Capabilities.Freeze); an unverified or unknown
// class is never paused:
//   - waiting: it has an open request, and nothing has happened for
//     pauseWaitingAfter;
//   - idle: its profile set pause_idle_after_sec, it has no open request,
//     nothing has happened for that long (floored at pauseDelayFloor), and its
//     CPU is quiet. A run with an open request pauses only as waiting, whose
//     close path resumes it.

const (
	// pauseWaitingAfter is how long a run with an open request waits, with
	// nothing happening, before it pauses.
	pauseWaitingAfter = 900 * time.Second
	// pauseDelayFloor is the shortest quiet period that ever pauses a run:
	// longer than the longest egress hold (600 s) plus the touch debounce. A
	// credential re-auth hold can run to 1800 s (WARDYN_CREDENTIAL_REAUTH_TIMEOUT's
	// ceiling), past this floor; the rules cover it instead: its request is open,
	// so the idle rule never pauses the run, and the waiting rule counts it only
	// once it is older than store.ReauthHoldMax, when the hold is over.
	pauseDelayFloor = 630 * time.Second
	// presenceStampEvery coalesces active_at writes to one per run per minute.
	// It is far below pauseDelayFloor, so a run stamped within it is never a
	// pause candidate.
	presenceStampEvery = 60 * time.Second
	// idleSamplesPerTick bounds the CPU reads one sweep makes; the candidates
	// are taken round-robin, so every idle run is read in turn.
	idleSamplesPerTick = 4
	// idleQuietCorePercent is the CPU use, as a percent of one core, below
	// which an idle run counts as quiet. The reading includes the sampling
	// script's own few milliseconds.
	idleQuietCorePercent = 10.0
)

// pauseClocks is the process-local state the pause keeps: the two stamp
// debounces, the failed-thaw backoffs and the idle round-robin cursor. Presence
// and agent activity are debounced apart, so an agent stamp never swallows the
// stamp that tells a person's keystroke the run is paused.
type pauseClocks struct {
	mu        sync.Mutex
	presence  map[uuid.UUID]time.Time
	activity  map[uuid.UUID]time.Time
	thawRetry map[uuid.UUID]thawBackoff
	cursor    uuid.UUID
	probes    map[uuid.UUID]*probeState
	diskRoot  map[uuid.UUID]diskRootSample
}

// thawRetryFloor is the first wait before an input retries a thaw that failed;
// each failure after it doubles the wait, up to presenceStampEvery.
const thawRetryFloor = time.Second

// thawBackoff is one run whose thaw failed: next is the earliest an input
// retries it, wait the interval that failure set.
type thawBackoff struct {
	next time.Time
	wait time.Duration
}

// thawGate reports whether a failed thaw of id is outstanding, and whether its
// backoff still holds at now.
func (c *pauseClocks) thawGate(id uuid.UUID, now time.Time) (pending, hold bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.thawRetry[id]
	return ok, ok && now.Before(b.next)
}

// thawFailed notes a failed thaw (or a failed read of the paused run) of id at
// now and sets when the next input retries it.
func (c *pauseClocks) thawFailed(id uuid.UUID, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.thawRetry == nil || len(c.thawRetry) > 4096 {
		c.thawRetry = map[uuid.UUID]thawBackoff{}
	}
	wait := thawRetryFloor
	if b, ok := c.thawRetry[id]; ok {
		wait = min(b.wait*2, presenceStampEvery)
	}
	c.thawRetry[id] = thawBackoff{next: now.Add(wait), wait: wait}
}

// thawDone forgets id's failed thaw: it was thawed, or found not paused.
func (c *pauseClocks) thawDone(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.thawRetry, id)
}

// probeState is one run's tally of Wardyn's own disk walks in flight and idle
// CPU samples in progress. The two must never overlap: a walk's CPU inside the
// sample's window is Wardyn's own probe read as the agent's activity.
type probeState struct{ walks, samples int }

// diskRootSample is the last root-filesystem walk's result for one run: kb is
// "" when the walk read nothing (it timed out), which is cached all the same so
// a sandbox too large to walk in time is not walked again by every poll.
type diskRootSample struct {
	kb string
	at time.Time
}

// runDiskRootTTL is how long a root walk's result stands in for a new one. The
// figure is an uncapped, informational one (the walk counts the image too), and
// a minute is well inside how fast it moves.
const runDiskRootTTL = time.Minute

// probeWait is how often beginSample looks for the walks it is waiting out.
const probeWait = 10 * time.Millisecond

// beginWalk tallies one Sandbox-widget read of run id and reports whether it
// may walk a disk tree: not while an idle sample is under way. The read is
// counted either way, so a sample that opens after it waits it out. Always
// paired with endWalk.
func (c *pauseClocks) beginWalk(id uuid.UUID) (mayWalk bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.probe(id)
	p.walks++
	return p.samples == 0
}

// abandonedWalkGrace is how long a read whose request ended (a closed tab, the
// exec deadline) stays tallied: its `timeout 2 du` may still be running in the
// sandbox, so the sample waits that out too.
var abandonedWalkGrace = 2 * time.Second

// endWalk ends a read beginWalk counted. abandoned is true when the request
// ended before the read did.
func (c *pauseClocks) endWalk(id uuid.UUID, abandoned bool) {
	if abandoned {
		time.AfterFunc(abandonedWalkGrace, func() { c.endWalk(id, false) })
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.probe(id)
	p.walks--
	c.dropIdleProbe(id, p)
}

// beginSample claims run id's CPU window for an idle sample: no new walk starts
// from now, and it returns once the ones in flight have ended. False when ctx
// ended first; a true return must be paired with endSample.
func (c *pauseClocks) beginSample(ctx context.Context, id uuid.UUID) bool {
	c.mu.Lock()
	c.probe(id).samples++
	c.mu.Unlock()
	for {
		c.mu.Lock()
		drained := c.probe(id).walks == 0
		c.mu.Unlock()
		if drained {
			return true
		}
		select {
		case <-ctx.Done():
			c.endSample(id)
			return false
		case <-time.After(probeWait):
		}
	}
}

func (c *pauseClocks) endSample(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.probe(id)
	p.samples--
	c.dropIdleProbe(id, p)
}

// probe returns id's tally, creating it. Callers hold mu.
func (c *pauseClocks) probe(id uuid.UUID) *probeState {
	if c.probes == nil {
		c.probes = map[uuid.UUID]*probeState{}
	}
	p, ok := c.probes[id]
	if !ok {
		p = &probeState{}
		c.probes[id] = p
	}
	return p
}

// dropIdleProbe forgets a tally with nothing in it, so the map holds only runs
// with a walk or a sample in flight. Callers hold mu.
func (c *pauseClocks) dropIdleProbe(id uuid.UUID, p *probeState) {
	if p.walks == 0 && p.samples == 0 {
		delete(c.probes, id)
	}
}

// rootSample returns run id's last root walk and whether it is still inside
// runDiskRootTTL at now.
func (c *pauseClocks) rootSample(id uuid.UUID, now time.Time) (kb string, fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d, ok := c.diskRoot[id]
	return d.kb, ok && now.Sub(d.at) < runDiskRootTTL
}

func (c *pauseClocks) setRootSample(id uuid.UUID, kb string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.diskRoot == nil || len(c.diskRoot) > 4096 {
		c.diskRoot = map[uuid.UUID]diskRootSample{}
	}
	c.diskRoot[id] = diskRootSample{kb: kb, at: now}
}

// stampDue reports whether id's last stamp is older than presenceStampEvery,
// in the presence clock or the agent-activity one.
func (c *pauseClocks) stampDue(presence bool, id uuid.UUID, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	last, ok := c.clock(presence)[id]
	return !ok || now.Sub(last) >= presenceStampEvery
}

// stamped notes a stamp that reached the store. A failed write is never
// noted, so the next event retries it.
func (c *pauseClocks) stamped(presence bool, id uuid.UUID, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.clock(presence)
	if len(m) > 4096 {
		clear(m)
	}
	m[id] = now
}

// clock returns the map for one clock, creating it. Callers hold mu.
func (c *pauseClocks) clock(presence bool) map[uuid.UUID]time.Time {
	if c.presence == nil {
		c.presence, c.activity = map[uuid.UUID]time.Time{}, map[uuid.UUID]time.Time{}
	}
	if presence {
		return c.presence
	}
	return c.activity
}

// markPresent records that a person is at the run — typing into it, opening an
// exec, or pressing Resume — and thaws it first when it is paused. The error is
// only a paused run that could not be thawed.
func (s *Server) markPresent(ctx context.Context, runID uuid.UUID, actorType types.ActorType, principal, reason string) error {
	_, err := s.stampPresence(ctx, runID, actorType, principal, reason)
	return err
}

// stampPresence is markPresent, reporting whether it resumed the run.
//
// The stamp debounce keeps a healthy run to one write a window. It never
// swallows a thaw that is owed: a thaw that failed (or a read of the paused run
// that failed) is retried by the next input once its short backoff has passed,
// and reports its failure each time, so a person typing into a frozen agent is
// not left waiting out the whole window. An input inside the backoff does
// nothing and reports nothing; thawForExec still thaws directly then.
func (s *Server) stampPresence(ctx context.Context, runID uuid.UUID, actorType types.ActorType, principal, reason string) (bool, error) {
	pauser, ok := s.cfg.Store.(store.RunPauser)
	if !ok {
		return false, nil
	}
	now := s.cfg.Now()
	pending, hold := s.pause.thawGate(runID, now)
	if hold || (!pending && !s.pause.stampDue(true, runID, now)) {
		return false, nil
	}
	paused, err := pauser.StampRunActive(ctx, runID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: stamping a run's presence failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return false, nil
	}
	s.pause.stamped(true, runID, now)
	if !paused {
		s.pause.thawDone(runID)
		return false, nil
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err == nil {
		err = s.resumeRun(ctx, pauser, run, actorType, principal, reason)
	}
	if err != nil {
		s.pause.thawFailed(runID, now)
		return false, err
	}
	s.pause.thawDone(runID)
	return true, nil
}

// thawForExec is markPresent for a path about to exec into the sandbox, given
// the run it just read: a paused run is thawed whatever the stamp debounce
// says, because the daemon refuses an exec into a paused container.
func (s *Server) thawForExec(ctx context.Context, run types.AgentRun, actorType types.ActorType, principal, reason string) error {
	if resumed, err := s.stampPresence(ctx, run.ID, actorType, principal, reason); err != nil || resumed {
		return err
	}
	pauser, ok := s.cfg.Store.(store.RunPauser)
	if !ok || run.PausedAt == nil {
		return nil
	}
	return s.resumeRun(ctx, pauser, run, actorType, principal, reason)
}

// noteAgentActive moves the presence clock for the agent's own activity: an
// egress decision, or the proxy reporting bytes moved. It never thaws.
func (s *Server) noteAgentActive(ctx context.Context, runID uuid.UUID) {
	pauser, ok := s.cfg.Store.(store.RunPauser)
	if !ok {
		return
	}
	now := s.cfg.Now()
	if !s.pause.stampDue(false, runID, now) {
		return
	}
	if _, err := pauser.StampRunActive(ctx, runID); err == nil {
		s.pause.stamped(false, runID, now)
	}
}

// runPausedReadMsg is the run page widgets' answer for a paused run. They poll,
// and a poll is not a person, so reading never thaws a run (an open tab would
// otherwise keep it awake); the daemon would refuse the exec anyway.
const runPausedReadMsg = "run is paused; resume it to read its sandbox"

// presenceReader calls mark before handing on bytes a person sent, so a paused
// run is thawed before they reach it.
type presenceReader struct {
	r    io.Reader
	mark func()
}

func (p presenceReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.mark()
	}
	return n, err
}

// ruleSourceApprovalsPoll mirrors internal/egress/proxy's ruleSourceApprovals:
// the toolgate's 2-second poll of the request it is blocked on.
const ruleSourceApprovalsPoll = "brokered:approvals"

// agentActivityDecision reports whether a decision with this rule_source is
// the agent doing something. Two are not: the toolgate polling a request it is
// blocked on (that is waiting, and would keep a waiting run from ever pausing),
// and a re-auth hold timing out (nobody came; see shouldTouch).
func agentActivityDecision(ruleSource string) bool {
	return ruleSource != ruleSourceApprovalsPoll && ruleSource != ruleSourceCredentialReauthTimeout
}

// approvalClosed resumes a paused run once it has no open request left: the
// agent has an answer to act on. Every writer that moves a request out of
// PENDING calls it; the pause sweep's backstop catches a waiting pause whose
// request closed without one (the expiry sweeper). It resumes an idle pause
// too: a request raised by traffic in flight when the run froze is still the
// agent's to answer.
func (s *Server) approvalClosed(ctx context.Context, runID uuid.UUID) {
	pauser, ok := s.cfg.Store.(store.RunPauser)
	if !ok {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || run.PausedAt == nil {
		return
	}
	if open, err := pauser.RunHasOpenRequest(ctx, runID); err != nil || open {
		return
	}
	_ = s.resumeRun(ctx, pauser, run, types.ActorSystem, "wardynd", "request_closed")
}

// errResumedSandboxGone is resumeRun's answer when the thaw succeeded but the
// sandbox is no longer running: a thaw of a missing container is a no-op
// success, so it is the status re-read that tells the two apart.
var errResumedSandboxGone = errors.New("the run's sandbox is not running")

// resumeRun thaws a paused run, re-reads its sandbox's status, then clears its
// pause. Thawing first means a failure leaves the run marked paused, so the
// next input (after its short backoff), Resume or exec tries again, and the
// sweep too for a waiting pause with no request open; clearing first could
// leave a frozen agent nobody knows about. The re-read keeps a vanished sandbox from
// reading as a resumed run: its pause stays until the watcher or the
// reconciler settles the run.
func (s *Server) resumeRun(ctx context.Context, pauser store.RunPauser, run types.AgentRun,
	actorType types.ActorType, principal, reason string) error {
	data := map[string]any{"reason": reason, "paused_reason": run.PausedReason, "paused_at": run.PausedAt}
	if f, ok := s.cfg.Runner.(runner.Freezer); ok && run.SandboxRef != "" {
		err := f.ThawSandbox(ctx, run.SandboxRef)
		if err == nil {
			err = s.thawedSandboxRunning(ctx, run.SandboxRef)
		}
		if err != nil && !errors.Is(err, runner.ErrFreezeUnsupported) {
			data["error"] = err.Error()
			s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, principal, "run.resume",
				run.ID.String(), "failure", mustJSON(data)))
			return err
		}
	}
	cleared, err := pauser.ClearRunPaused(ctx, run.ID)
	if err != nil {
		return err
	}
	if cleared {
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, principal, "run.resume",
			run.ID.String(), "success", mustJSON(data)))
	}
	return nil
}

// thawedSandboxRunning re-reads a just-thawed sandbox's status: nil only when
// it is RUNNING.
func (s *Server) thawedSandboxRunning(ctx context.Context, ref string) error {
	st, err := s.cfg.Runner.Status(ctx, ref)
	if err != nil {
		return err
	}
	if st.State != types.RunRunning {
		return fmt.Errorf("%w (state=%s)", errResumedSandboxGone, st.State)
	}
	return nil
}

// sweepRunPauses is one pass of the pause over every live run: pause the ones
// nobody is at, and resume a waiting one whose requests have all closed. Every
// write is a conditional UPDATE, so each replica can run it on its own tick.
func (s *Server) sweepRunPauses(ctx context.Context) error {
	pauser, ok := s.cfg.Store.(store.RunPauser)
	if !ok || s.cfg.Runner == nil {
		return nil
	}
	if _, ok := s.cfg.Runner.(runner.Freezer); !ok {
		return nil
	}
	cands, now, err := pauser.ListPauseCandidates(ctx)
	if err != nil {
		return err
	}
	if now.IsZero() {
		now = s.cfg.Now()
	}
	var (
		freeze  map[types.ConfinementClass]bool
		idle    []types.AgentRun
		capsErr error
	)
	freezable := func(class types.ConfinementClass) bool {
		if freeze == nil && capsErr == nil {
			var caps runner.Capabilities
			caps, capsErr = s.cfg.Runner.Capabilities(ctx)
			freeze = caps.Freeze
		}
		// Fail closed: no capability read, or a class absent from the map, is
		// a class nobody verified a freeze on.
		return capsErr == nil && freeze[class]
	}
	for _, c := range cands {
		run := c.Run
		if run.PausedAt != nil {
			// Only a waiting pause is the backstop's: an idle pause is marked
			// with no request open, so no open request is the state it paused
			// in, not a reason to wake.
			if run.PausedReason == types.PauseWaiting && !c.OpenRequest {
				_ = s.resumeRun(ctx, pauser, run, types.ActorSystem, "wardynd", "request_closed")
			}
			continue
		}
		quiet := now.Sub(activeSince(run))
		switch {
		case c.WaitingRequest && quiet >= pauseWaitingAfter && freezable(run.ConfinementClass):
			s.pauseRun(ctx, pauser, run, types.PauseWaiting, quiet)
		case !c.OpenRequest && run.RunLimits.PauseIdleAfterSec > 0 &&
			quiet >= max(time.Duration(run.RunLimits.PauseIdleAfterSec)*time.Second, pauseDelayFloor) &&
			freezable(run.ConfinementClass):
			idle = append(idle, run)
		}
	}
	for _, run := range s.nextIdleSamples(idle) {
		if s.runCPUQuiet(ctx, run) {
			s.pauseRun(ctx, pauser, run, types.PauseIdle, now.Sub(activeSince(run)))
		}
	}
	return nil
}

// activeSince is when anything last happened in run: its presence clock, or
// its creation for a run nothing has stamped yet.
func activeSince(run types.AgentRun) time.Time {
	if run.ActiveAt != nil {
		return *run.ActiveAt
	}
	return run.CreatedAt
}

// nextIdleSamples picks up to idleSamplesPerTick runs, round-robin by id from
// where the last pass stopped, so a large idle set is read in turn instead of
// the same few every pass.
func (s *Server) nextIdleSamples(runs []types.AgentRun) []types.AgentRun {
	if len(runs) <= idleSamplesPerTick {
		return runs
	}
	slices.SortFunc(runs, func(a, b types.AgentRun) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	s.pause.mu.Lock()
	defer s.pause.mu.Unlock()
	start, _ := slices.BinarySearchFunc(runs, s.pause.cursor.String(), func(r types.AgentRun, c string) int {
		if r.ID.String() <= c {
			return -1
		}
		return 1
	})
	out := make([]types.AgentRun, 0, idleSamplesPerTick)
	for i := range idleSamplesPerTick {
		out = append(out, runs[(start+i)%len(runs)])
	}
	s.pause.cursor = out[len(out)-1].ID
	return out
}

// runCPUQuiet reads run's CPU use through the cgroup exec idiom
// (run_resources.go) and reports whether it is below idleQuietCorePercent of
// one core. Anything it cannot read counts as busy: a run is never paused on a
// reading nobody took.
func (s *Server) runCPUQuiet(ctx context.Context, run types.AgentRun) bool {
	// Wardyn's own disk walk (an open run page's Sandbox widget) is CPU in this
	// window too, and the cgroup cannot tell it from the agent's. So the window
	// is claimed first: in-flight walks finish before it opens, and none starts
	// inside it. Not claimed in time reads as busy, like any reading not taken.
	claim, stop := context.WithTimeout(ctx, runResourcesExecTimeout)
	ok := s.pause.beginSample(claim, run.ID)
	stop()
	if !ok {
		return false
	}
	defer s.pause.endSample(run.ID)
	ctx, cancel := context.WithTimeout(ctx, runResourcesExecTimeout)
	defer cancel()
	// Only the cpu keys are read here; `filesystem` picks the script's disk
	// arm that is one statfs, never the 2-second walk the others can take.
	kv, err := s.execRunResourcesScript(ctx, run, types.StorageEnforcementFilesystem)
	if err != nil {
		return false
	}
	u1, ok1 := kvInt64(kv, "cpu_usage_usec_1")
	u2, ok2 := kvInt64(kv, "cpu_usage_usec_2")
	p1, ok3 := kvFloat64(kv, "uptime_1")
	p2, ok4 := kvFloat64(kv, "uptime_2")
	if !ok1 || !ok2 || !ok3 || !ok4 || p2 <= p1 || u2 < u1 {
		return false
	}
	return float64(u2-u1)/((p2-p1)*1e6)*100 < idleQuietCorePercent
}

// pauseRun freezes run, then marks it paused. Freezing first and marking with a
// compare on the presence clock means a keystroke, or the request closing,
// between the sweep's read and the mark wins: the mark fails and the agent is
// thawed again.
func (s *Server) pauseRun(ctx context.Context, pauser store.RunPauser, run types.AgentRun, reason types.PauseReason, quiet time.Duration) {
	f := s.cfg.Runner.(runner.Freezer)
	if err := f.FreezeSandbox(ctx, run.SandboxRef); err != nil {
		if !errors.Is(err, runner.ErrFreezeUnsupported) {
			slog.WarnContext(ctx, "wardynd: pausing a run failed",
				slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
		return
	}
	applied, err := pauser.MarkRunPaused(ctx, run.ID, reason, run.ActiveAt)
	if err == nil && applied {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.pause",
			run.ID.String(), "success", mustJSON(map[string]any{
				"reason": reason, "quiet_sec": int64(quiet.Seconds()), "active_at": run.ActiveAt,
			})))
		// A paused run holds no Azure DevOps token; its resume creates one.
		// After the mark, so a resolve racing it sees the pause and creates none.
		s.revokeRunPATs(ctx, run.ID, adoPATRevokePause)
		return
	}
	if terr := f.ThawSandbox(ctx, run.SandboxRef); terr != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.pause",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"reason": reason, "thaw_error": terr.Error(),
			})))
	}
}

// handleResumeRun serves POST /runs/{id}/resume: the person presses Resume.
// Owner or super admin, like the run's end: thawing a foreign run keeps its
// sandbox busy, a write outside the security tier's inspect-or-stop.
func (s *Server) handleResumeRun(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin)
	if !ok {
		return
	}
	if run.State != types.RunRunning || runIsKept(run) || run.SandboxRef == "" {
		writeErrorReason(w, http.StatusConflict, reasonRunResumeNotRunning, "run is not running; there is nothing to resume (state="+string(run.State)+")")
		return
	}
	actorType, principal := actorFromRequest(r)
	if err := s.thawForExec(r.Context(), run, actorType, principal, "resume"); err != nil {
		writeErrorReason(w, http.StatusBadGateway, reasonRunResumeFailed, "resuming the run failed; try again")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": run.ID, "paused": false})
}

// handleInternalActivity serves POST /internal/activity: the proxy reporting
// that bytes moved on one of the run's tunnel or MITM streams in the last
// minute, so a long download or a model stream in I/O wait is not "nobody
// there". The run is the verified token's, never one the caller names.
func (s *Server) handleInternalActivity(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return
	}
	s.noteAgentActive(r.Context(), claims.RunID)
	w.WriteHeader(http.StatusNoContent)
}
