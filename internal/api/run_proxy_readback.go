// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Policy readback (docs/operations/run-lifetime.md). Every proxy start is recorded as pending before
// its proxy starts (store.RunProxyStarts), under its generation and the fresh run token's jti. The
// driver answers the SHA-256 of the exact bytes it handed the proxy (runner.Sandbox.ProxyConfigSHA256 at
// create, runner.ProxyReviverHashed at revive); the proxy posts the SHA-256 of what it loaded to
// /internal/proxy/loaded. A report is held until the driver's hash is in, and the start is enforced only
// when both are in and equal. A driver that answers without a hash, a different hash, or no report by
// the driver's answer plus proxyReportGrace stops the proxy. The clock starts at the driver's answer: a
// create still running has no readback deadline (bounding a hung create is a follow-up). A start is
// legacy, and runs unconfirmed as before, only on positive evidence of an older runner or proxy image.

// proxyReportGrace is how long after the driver's answer the proxy's report may take.
const proxyReportGrace = 60 * time.Second

// readbackRelease is the first release whose runners and proxies take part in the readback.
const readbackRelease = "0.10.0-rc.18"

// readbackReleaseRE is a release (X.Y.Z) or release candidate (X.Y.Z-rc.N). It is the pattern
// internal/runner/k8s's releaseTagRE and cmd/wardyn-runnerd's releaseVersion hold: change one, change all.
var readbackReleaseRE = regexp.MustCompile(`^([0-9]+)\.([0-9]+)\.([0-9]+)(?:-rc\.([0-9]+))?$`)

// firstRenewGateRelease is the first release whose runner and proxy image make a replacement proxy wait
// for its first token renewal before it serves. A live egress edit needs both.
const firstRenewGateRelease = "0.10.0-rc.19"

// releaseBefore reports whether v parses as a release older than floor. Anything that does
// not parse (empty, a development tag, a digest) is not evidence, and reads as not older. A build from
// a checkout reports its release with a "+<commit>[-dirty]" suffix (internal/version), which is dropped
// first: such a build at an older release has no hash to report, so it reads as that release.
func releaseBefore(v, floor string) bool {
	v, _, _ = strings.Cut(strings.TrimPrefix(strings.TrimSpace(v), "v"), "+")
	a, ok := releaseRank(v)
	if !ok {
		return false
	}
	b, _ := releaseRank(floor)
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func releaseRank(v string) ([4]int, bool) {
	var p [4]int
	m := readbackReleaseRE.FindStringSubmatch(v)
	if m == nil {
		return p, false
	}
	for i := range 3 {
		p[i], _ = strconv.Atoi(m[i+1])
	}
	p[3] = math.MaxInt // a release sorts after each of its candidates
	if m[4] != "" {
		p[3], _ = strconv.Atoi(m[4])
	}
	return p, true
}

// proxyImageRelease is image's tag when it names a release, else "" (an untagged or digest-only
// reference, or a development tag), as internal/runner/k8s's imageReleaseTag reads it.
func proxyImageRelease(image string) string {
	ref, _, _ := strings.Cut(image, "@")
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i:], "/") || !readbackReleaseRE.MatchString(ref[i+1:]) {
		return ""
	}
	return ref[i+1:]
}

// olderThanFirstRenewGate applies proxyStartMode's evidence rule against firstRenewGateRelease: true only
// for a remote runner whose registered Version, or an in-process driver whose proxy image tag, is a
// release older than it. A version that cannot be read (an unreadable runner row, an empty Version, a
// digest-only image) is not checked: it is not evidence, so it passes.
func (s *Server) olderThanFirstRenewGate(ctx context.Context, run types.AgentRun) bool {
	if run.RunnerID != nil {
		rs, ok := s.cfg.Store.(runnerGetter)
		if !ok {
			return false
		}
		row, err := rs.GetRunner(ctx, *run.RunnerID)
		return err == nil && releaseBefore(row.Version, firstRenewGateRelease)
	}
	tag := proxyImageRelease(s.cfg.ProxyImage)
	return tag != "" && releaseBefore(tag, firstRenewGateRelease)
}

// proxyStartMode is legacy only on positive evidence: a remote runner whose registered Version is a
// release older than readbackRelease, or an in-process driver whose proxy image tag is. Anything else,
// an unreadable runner row included, is expected. It needs only the runner read, not all of
// store.RunnerStore: a store that can read the row has the evidence.
func (s *Server) proxyStartMode(ctx context.Context, run types.AgentRun) string {
	if run.RunnerID != nil {
		rs, ok := s.cfg.Store.(runnerGetter)
		if !ok {
			return store.ProxyStartExpected
		}
		row, err := rs.GetRunner(ctx, *run.RunnerID)
		if err == nil && releaseBefore(row.Version, readbackRelease) {
			return store.ProxyStartLegacy
		}
		return store.ProxyStartExpected
	}
	if tag := proxyImageRelease(s.cfg.ProxyImage); tag != "" && releaseBefore(tag, readbackRelease) {
		return store.ProxyStartLegacy
	}
	return store.ProxyStartExpected
}

// proxyStartRec is a start as its caller holds it: active is false when the store keeps no readback,
// and every other call is then a no-op.
type proxyStartRec struct {
	active bool
	gen    int
	mode   string
}

// beginProxyStart records run's pending start under gen and jti. A revive calls it only after the old
// token lineage is retired, so no report on the old lineage can land on it.
func (s *Server) beginProxyStart(ctx context.Context, run types.AgentRun, gen int, jti string) (proxyStartRec, error) {
	st, ok := s.cfg.Store.(store.RunProxyStarts)
	if !ok || gen < 1 { // gen 0: a store without generations, whose starts could not be fenced
		return proxyStartRec{}, nil
	}
	mode := s.proxyStartMode(ctx, run)
	if _, err := st.BeginProxyStart(ctx, run.ID, gen, jti, mode); err != nil {
		return proxyStartRec{}, err
	}
	return proxyStartRec{active: true, gen: gen, mode: mode}, nil
}

// abandonProxyStart marks a revive's start abandoned. It runs before the old lineage is restored, so the
// kept proxy's tokens can never confirm it.
func (s *Server) abandonProxyStart(ctx context.Context, runID uuid.UUID, rec proxyStartRec) {
	st, ok := s.cfg.Store.(store.RunProxyStarts)
	if !ok || !rec.active {
		return
	}
	if err := st.AbandonProxyStart(ctx, runID, rec.gen); err != nil {
		slog.WarnContext(ctx, "wardynd: abandoning an unused proxy start failed",
			slog.String("run_id", runID.String()), slog.Int("generation", rec.gen), slog.Any("err", err))
	}
}

// proxyStartAnswered records the driver's successful answer for rec, with its hash. false is a start
// that must not run: the hash is missing or differs from a report already held, the start is no longer
// the run's active one (a late answer), or its answer could not be recorded. The caller stops the proxy.
// A pending start gets its deadline watchdog.
func (s *Server) proxyStartAnswered(ctx context.Context, runID uuid.UUID, rec proxyStartRec, sha string) (bool, string, *store.ProxyStartChange) {
	st, ok := s.cfg.Store.(store.RunProxyStarts)
	if !ok || !rec.active {
		return true, "", nil
	}
	ch, err := st.AnswerProxyStart(ctx, runID, rec.gen, sha, proxyReportGrace)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: recording a proxy start's answer failed; reading it back",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		var read bool
		if ch, read = s.recoverProxyAnswer(ctx, st, runID, rec, sha); !read {
			s.auditReadback(ctx, runID, "run.policy.unconfirmed", "failure", rec.gen, map[string]any{"reason": "answer_unrecorded"})
			return false, "answer_unrecorded", nil
		}
	}
	if !ch.Active {
		s.auditReadback(ctx, runID, "run.policy.unconfirmed", "failure", rec.gen, map[string]any{"reason": "start_superseded"})
		return false, "start_superseded", nil
	}
	switch ch.Settled {
	case types.ProxyStartUnconfirmed:
		s.auditReadback(ctx, runID, "run.policy.unconfirmed", "success", rec.gen, map[string]any{"reason": store.ProxyStartLegacy})
	case types.ProxyStartEnforced:
		s.auditReadback(ctx, runID, "run.policy.enforced", "success", rec.gen, map[string]any{"config_sha256": ch.Start.ExpectedSHA256})
	case types.ProxyStartFailed:
		return false, ch.Reason, &ch
	default:
		s.watchProxyStart(runID, rec.gen)
	}
	return true, "", nil
}

// recoverProxyAnswer is the outcome of an answer whose write returned an error, read back from the
// start, never inferred: the write may have committed. An answer that committed stands (enforced,
// legacy unconfirmed, or pending on its deadline). One that did not is answered again with a
// deadline already passed, and read back failed, so the sweep lists it until its stop is confirmed.
// read is false when neither can be read back. Only an answer refused twice (the start read back
// still pending with no deadline) leaves it pending, with nothing retrying it. A read that errors,
// finds no start, or finds another generation leaves its state unknown to this answer.
func (s *Server) recoverProxyAnswer(ctx context.Context, st store.RunProxyStarts, runID uuid.UUID, rec proxyStartRec, sha string) (ch store.ProxyStartChange, read bool) {
	reread := func() (store.ProxyStart, bool) {
		p, found, err := st.ActiveProxyStart(ctx, runID)
		return p, err == nil && found && p.Generation == rec.gen
	}
	p, ok := reread()
	if ok && p.State == types.ProxyStartPending && p.Deadline == nil && rec.mode == store.ProxyStartExpected {
		if _, err := st.AnswerProxyStart(ctx, runID, rec.gen, sha, -time.Second); err != nil {
			slog.WarnContext(ctx, "wardynd: failing a proxy start whose answer was not recorded failed",
				slog.String("run_id", runID.String()), slog.Any("err", err))
		}
		p, ok = reread()
	}
	ch = store.ProxyStartChange{Start: p, Active: true}
	switch {
	case !ok:
		return store.ProxyStartChange{}, false
	case p.State == types.ProxyStartFailed:
		ch.Settled, ch.Reason = p.State, "answer_unrecorded"
	case p.State == types.ProxyStartEnforced || p.State == types.ProxyStartUnconfirmed:
		ch.Settled = p.State
	case p.State == types.ProxyStartPending && p.Deadline != nil:
	default:
		return store.ProxyStartChange{}, false
	}
	return ch, true
}

// proxyStartWatch schedules a start's deadline check. A variable so a test can run it at once.
var proxyStartWatch = func(d time.Duration, f func()) { time.AfterFunc(d, f) }

// watchProxyStart checks gen's deadline just after it passes. The reconcile tick's
// sweepOverdueProxyStarts is the backstop for a replica that stops before then.
func (s *Server) watchProxyStart(runID uuid.UUID, gen int) {
	proxyStartWatch(proxyReportGrace+time.Second, func() {
		ctx, cancel := context.WithTimeout(context.Background(), killCascadeTimeout)
		defer cancel()
		s.expireProxyStart(ctx, runID, gen)
	})
}

// expireProxyStart fails gen if it is still the run's active pending start past its deadline, and
// stops its proxy.
func (s *Server) expireProxyStart(ctx context.Context, runID uuid.UUID, gen int) {
	st, ok := s.cfg.Store.(store.RunProxyStarts)
	if !ok {
		return
	}
	ch, err := st.ExpireProxyStart(ctx, runID, gen)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: expiring a proxy start failed; the sweep retries",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return
	}
	if ch.Settled == types.ProxyStartFailed {
		s.auditReadbackFailure(ctx, ch, s.containProxyStart(ctx, runID, gen))
	}
}

// sweepOverdueProxyStarts is the reconcile tick's deadline pass, and its retry of failed starts whose
// proxy stop is not yet confirmed.
func (s *Server) sweepOverdueProxyStarts(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.RunProxyStarts)
	if !ok {
		return nil
	}
	starts, err := st.OverdueProxyStarts(ctx)
	if err != nil {
		return err
	}
	for _, p := range starts {
		if p.State == types.ProxyStartFailed {
			s.containProxyStart(ctx, p.RunID, p.Generation)
			continue
		}
		s.expireProxyStart(ctx, p.RunID, p.Generation)
	}
	return nil
}

// proxyContainment is what containProxyStart found for one failed start.
type proxyContainment struct {
	// Contained: the start's proxy is confirmed stopped, and that is recorded on the start.
	Contained bool
	// Superseded: gen is no longer the run's active start, so nothing was stopped.
	Superseded bool
	// Err is why containment is not confirmed (unresolved); the reconcile sweep retries it.
	Err error
}

// errStartNotFailed is a containment asked for a start that is not failed (one whose answer could not
// be recorded, refused twice, stays pending): nothing stops it, and the sweep does not list it.
var errStartNotFailed = errors.New("not failed")

// errRunOpBusy is a containment skipped because another operation holds the run's lock.
var errRunOpBusy = errors.New("another operation holds the run's lock; the sweep retries")

// containProxyStart is containProxyStartLocked under the run's operation lock (locks.go), for a
// caller that does not hold it: the report handler, the deadline watchdog, the reconcile sweep and
// the egress edit. It never waits: a lock another operation holds (a revive among them) leaves the
// start unresolved (errRunOpBusy) for the sweep's next pass. Its runner calls are bounded as the
// rebooted-run sweep's are, so it cannot hold the lock past a wedged daemon.
func (s *Server) containProxyStart(ctx context.Context, runID uuid.UUID, gen int) proxyContainment {
	lctx, unlock, ok := s.tryLockRunOp(ctx, runID)
	if !ok {
		return proxyContainment{Err: errRunOpBusy}
	}
	defer unlock()
	if _, busy := s.reviving.Load(runID); busy {
		return proxyContainment{Err: errRunOpBusy}
	}
	lctx, cancel := context.WithTimeout(lctx, keepRebootedRunTimeout)
	defer cancel()
	return s.containProxyStartLocked(lctx, runID, gen)
}

// proxyContainPaused runs between containment's generation read and its stop. A test hook.
var proxyContainPaused = func() {}

// containProxyStartLocked stops the proxy of the run's failed start gen and records the result on the
// start (store.ProxyStart.ContainedAt, or ContainmentError while unresolved). The caller holds the
// run's operation lock, and ctx is the lock's: the generation read, the run read, the stop and the
// record all happen under it, so a revive cannot start a newer proxy between the read and the stop.
// It is fenced on gen: once a newer start is the run's active one it stops nothing. A start already
// contained answers Contained with no second stop. It may run again, and on any replica.
func (s *Server) containProxyStartLocked(ctx context.Context, runID uuid.UUID, gen int) proxyContainment {
	st, ok := s.cfg.Store.(store.RunProxyStarts)
	if !ok {
		return proxyContainment{Err: errors.New("the store keeps no proxy starts")}
	}
	p, found, err := st.ActiveProxyStart(ctx, runID)
	switch {
	case err != nil:
		return proxyContainment{Err: err}
	case !found || p.Generation != gen:
		return proxyContainment{Superseded: true}
	case p.ContainedAt != nil:
		return proxyContainment{Contained: true}
	case p.State != types.ProxyStartFailed:
		return proxyContainment{Err: fmt.Errorf("proxy start %d is %s: %w", gen, p.State, errStartNotFailed)}
	}
	proxyContainPaused()
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return proxyContainment{Err: err}
	}
	stopErr := s.stopUnconfirmedProxyAt(ctx, run, gen)
	msg := ""
	if stopErr != nil {
		msg = stopErr.Error()
	}
	if err := st.RecordProxyContainment(ctx, runID, gen, msg); err != nil && stopErr == nil {
		stopErr = err
	}
	if stopErr != nil {
		slog.WarnContext(ctx, "wardynd: a failed proxy start is not confirmed stopped; the sweep retries",
			slog.String("run_id", runID.String()), slog.Int("generation", gen), slog.Any("err", stopErr))
		return proxyContainment{Err: stopErr}
	}
	return proxyContainment{Contained: true}
}

// stopUnconfirmedProxy stops the proxy of a run whose start failed after its driver answered, and
// answers nil only once that stop is confirmed. A run still being dispatched fails, its sandbox
// stopped (the dispatch's own RUNNING write then loses); a running run is lost (outage) with its
// proxy stopped, or ended when it cannot be kept, as the lapsed-token sweep does. A run kept lost
// has its proxy stopped again, an ended run whose sandbox is kept for its computer folder's copy
// back its proxy alone, and any other run its sandbox (one torn down with its lost_at kept among
// them, whose substrate may refuse a proxy stop): what a retry does.
func (s *Server) stopUnconfirmedProxy(ctx context.Context, run types.AgentRun) error {
	return s.stopUnconfirmedProxyAt(ctx, run, 0)
}

// stopUnconfirmedProxyAt is stopUnconfirmedProxy for the failed start of policy generation gen: a running
// run's lost mark lands only while gen is still the run's newest (loseRunStopped's fence), so a
// containment whose lock was lost after its generation read does not mark lost a newer revive's run.
// A kept run's stop below carries gen where generationStopperFor answers a stopper, which then never stops a
// newer revive's proxy. Elsewhere it stops the plain way (stopLostSandboxAt): no newer proxy can exist on a
// VM, a substrate with no replace path or a connected runner that revive refuses, but one can on a runner
// read as not connected at selection, and that plain stop can stop it once the runner is back.
// 0 fences nothing.
func (s *Server) stopUnconfirmedProxyAt(ctx context.Context, run types.AgentRun, gen int) error {
	switch {
	case run.State == types.RunStarting:
		ok := s.stopSandboxOrAudit(ctx, run.ID, run.SandboxRef, "run.create")
		s.failAndRevoke(ctx, run.ID, types.RunStarting, unconfirmedHint(ok))
		if !ok {
			return errProxyStopUnconfirmed
		}
		return nil
	case runIsKept(run) && !isTerminalRunState(run.State):
		return s.stopLostSandboxAt(ctx, run, s.cfg.Now(), gen)
	case run.State == types.RunRunning:
		if handled, err := s.loseUnconfirmedRun(ctx, run, gen); handled {
			return err
		}
		// The proxy first, then the end, whose computer folder copy back decides whether the sandbox
		// is stopped (finalizeRunEffects): a sandbox kept for it is not stopped here, so the end must
		// land (runEndRecorded). A proxy stop that is not confirmed, or a run on a runner (whose
		// sandbox is never kept for a copy back, quiesceKeptSandbox), has the sandbox stopped before
		// the end, as before.
		left := run.RunnerID == nil && s.stopProxyOrAudit(ctx, run.ID, run.SandboxRef, "run.reconcile") == nil
		ok := left || s.stopSandboxOrAudit(ctx, run.ID, run.SandboxRef, "run.reconcile")
		s.reconcileFinalize(ctx, run.ID, types.RunFailed, run.SandboxRef, unconfirmedHint(ok))
		switch {
		case !ok:
			return errProxyStopUnconfirmed
		case left:
			return s.runEndRecorded(ctx, run.ID)
		}
		return nil
	}
	if isTerminalRunState(run.State) {
		// A sandbox kept for its computer folder's copy back (folderKeepVerdict) stays only on a confirmed cut,
		// so its changes not yet copied back are not lost; its ref stays for the tail owner. A runner that is not
		// here fails this stop for the sweep to retry. A substrate that cannot be cut keeps no such sandbox, and
		// neither does a hosted one whose proxy stop failed (its failure row written): its sandbox is stopped as
		// any other. A record that cannot be read keeps the sandbox only on a confirmed cut, with one failure row
		// carrying folder_record_error; a rule that cannot be read past the cap is given up by the tail owner.
		v, ferr, cutErr := s.folderKeepVerdict(ctx, run, "run.reconcile")
		switch v {
		case folderKeepCut:
			if ferr != nil {
				s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.reconcile", run.SandboxRef, "failure",
					mustJSON(map[string]any{"sandbox_ref": run.SandboxRef, "folder_record_error": ferr.Error()})))
			}
			return nil
		case folderKeepAway:
			return cutErr
		}
	}
	if !s.stopSandboxOrAudit(ctx, run.ID, run.SandboxRef, "run.reconcile") {
		return errProxyStopUnconfirmed
	}
	return nil
}

// stopProxyOrAudit stops only the proxy of the sandbox ref (runner.ProxyStopper), leaving the
// sandbox, and records a failed stop under action as stopSandboxOrAudit does. It answers nil once
// the stop is confirmed, errProxyAloneUnsupported (recording nothing) for a substrate that cannot
// stop a proxy alone, and errProxyStopUnconfirmed for a stop that failed.
func (s *Server) stopProxyOrAudit(ctx context.Context, runID uuid.UUID, ref, action string) error {
	if s.cfg.Runner == nil || ref == "" {
		return nil
	}
	stopper, ok := s.cfg.Runner.(runner.ProxyStopper)
	if !ok {
		return errProxyAloneUnsupported
	}
	switch err := stopper.StopProxy(ctx, ref); {
	case err == nil:
		return nil
	case errors.Is(err, runner.ErrEndUnsupported):
		return errProxyAloneUnsupported
	case s.teardownOrAudit(ctx, runID, ref, action, func(context.Context, string) error { return err }):
		return nil
	}
	return errProxyStopUnconfirmed
}

// folderKeep is a sweep's verdict on a terminal run's sandbox against its computer folder (folderKeepVerdict).
type folderKeep int

const (
	folderKeepNone folderKeep = iota // not kept: the sandbox is stopped
	folderKeepCut                    // kept, and the agent's cut is confirmed
	folderKeepAway                   // a runner run whose runner is not here: left in place, nothing stopped or queued
)

// folderKeepVerdict is the one rule every destructive sweep decides a kept sandbox by: a sandbox kept for its
// computer folder's pass back (keepsSandboxForFolder), or whose record cannot be read (ferr), stays only once
// the agent's cut is confirmed (copyBackCut: hosted, a fenced runner, a connected runner whose proxy stop
// answered nil). A runner that is not here leaves it in place (folderKeepAway), and cutErr is errRunnerNotHere.
// Anything else is folderKeepNone: stopped whatever its record says. That covers a sandbox that cannot be cut (a
// virtual machine, Kubernetes in server mode, a runner sandbox whose cut failed, as quiesceKeptSandbox decides)
// and a hosted cut whose proxy stop failed, whose failure row copyBackCut has written. cutErr is copyBackCut's
// answer.
func (s *Server) folderKeepVerdict(ctx context.Context, run types.AgentRun, action string) (v folderKeep, ferr, cutErr error) {
	kept, ferr := s.keepsSandboxForFolder(ctx, run)
	if ferr != nil {
		slog.WarnContext(ctx, "wardynd: a terminal run's computer folder record could not be read",
			slog.String("run_id", run.ID.String()), slog.String("action", action), slog.Any("err", ferr))
	}
	if (!kept && ferr == nil) || run.MachineKind.IsVM() {
		return folderKeepNone, ferr, nil
	}
	cutErr = s.copyBackCut(ctx, run, action)
	switch {
	case errors.Is(cutErr, errRunnerNotHere):
		return folderKeepAway, ferr, cutErr // no row: the runner is not here, and the next pass asks again
	case cutErr == nil:
		return folderKeepCut, ferr, nil
	}
	return folderKeepNone, ferr, cutErr
}

// stopTerminalSandbox stops a terminal run's sandbox through its computer folder's keep decision
// (folderKeepVerdict); left reports a sandbox left in place. A sandbox kept for its pass back stays when the
// agent's cut is confirmed, and a stop that failed is action's failure row with teardown_error. A record that
// cannot be read is logged, and the sandbox stays only once that cut is confirmed, as a failure row with
// folder_record_error that stays unresolved so a later pass reads it again. A runner run whose runner is
// not connected here is left in place with errRunnerNotHere, nothing stopped, queued or written, until its
// runner is. It fails closed: any other sandbox is stopped, a hosted one whose proxy stop failed among them (its
// failure row is the cut's, and a sandbox stop that fails too is the failed-stop retry's).
func (s *Server) stopTerminalSandbox(ctx context.Context, run types.AgentRun, action string) (left bool, err error) {
	if run.SandboxRef == "" {
		return false, nil
	}
	v, ferr, cutErr := s.folderKeepVerdict(ctx, run, action)
	switch {
	case v == folderKeepAway:
		return true, cutErr
	case v == folderKeepCut && ferr != nil:
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", action, run.SandboxRef, "failure",
			mustJSON(map[string]any{"sandbox_ref": run.SandboxRef, "folder_record_error": ferr.Error()})))
		return true, fmt.Errorf("its computer folder's record could not be read, so its sandbox is kept with its proxy stopped: %w", ferr)
	case v == folderKeepCut:
		return true, nil
	}
	if !s.stopSandboxOrAudit(ctx, run.ID, run.SandboxRef, action) {
		return false, errProxyStopUnconfirmed
	}
	return false, nil
}

// errProxyAloneUnsupported is a substrate that cannot stop a proxy without its sandbox.
var errProxyAloneUnsupported = errors.New("this substrate cannot stop a proxy alone")

// errProxyStopUnconfirmed is a sandbox stop that failed (its teardown_error is audited).
var errProxyStopUnconfirmed = errors.New("the sandbox stop failed")

// runEndRecorded answers errRunEndUnwritten while the store still holds the run live, neither ended,
// lost nor kept, or cannot be read: an end whose write failed left its sandbox, and its computer
// folder decision, undone, so the containment stays unresolved and the start's sweep retries it.
// Its read is bound apart from ctx, which the end's copy back may have used up.
func (s *Server) runEndRecorded(ctx context.Context, runID uuid.UUID) error {
	ctx, cancel := stopCtx(ctx)
	defer cancel()
	cur, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || (!isTerminalRunState(cur.State) && cur.LostAt == nil && !runIsKept(cur)) {
		return errRunEndUnwritten
	}
	return nil
}

// errRunEndUnwritten is a containment whose proxy stop succeeded but whose run end could not be
// written: still unresolved, so a retry ends the run.
var errRunEndUnwritten = errors.New("the run's proxy was stopped, but its end could not be written")

// errLostMarkUnwritten is a containment whose proxy stop succeeded but whose lost mark could not be
// written: still unresolved, so a retry writes the mark.
var errLostMarkUnwritten = errors.New("the run's proxy was stopped, but its lost mark could not be written")

// loseUnconfirmedRun marks a running run lost (outage) and stops its proxy, answering the stop's
// error. handled is false for a run that cannot be kept, which the caller tears down. A lost mark
// that could not be written still has the proxy stopped, as reloseRun does, and stays unresolved so a
// retry writes it.
// gen fences the lost mark (loseRunStopped): one that misses it is not applied, and nothing is stopped.
func (s *Server) loseUnconfirmedRun(ctx context.Context, run types.AgentRun, gen int) (handled bool, err error) {
	loser, ok := s.cfg.Store.(store.RunLoser)
	leaser, lok := s.cfg.Store.(store.RunLeaser)
	if !ok || !lok {
		return false, nil
	}
	now := s.cfg.Now()
	switch o, stopErr := s.loseRunStopped(ctx, loser, leaser, run, types.LostOutage, types.RunFailed, 0, now, gen); o {
	case loseNotKeepable:
		return false, nil
	case loseApplied:
		if errors.Is(stopErr, runner.ErrEndUnsupported) {
			// loseRunStopped asked to tear the run down: contained once that end and its sandbox stop landed.
			_, err := s.reloseEnded(ctx, run.ID)
			return true, err
		}
		return true, stopErr
	case loseWriteFailed:
		s.revokeRunBroker(ctx, run.ID)
		run.LostAt, run.LostReason = &now, types.LostOutage
		if err := s.stopLostSandboxAt(ctx, run, now, gen); err != nil {
			return true, err
		}
		return true, errLostMarkUnwritten
	default:
		return true, errors.New("the run's lost mark did not land")
	}
}

// unconfirmedPolicyHint is the failure hint of a run whose proxy did not confirm its policy and was
// stopped; unconfirmedStopPendingHint the same while that stop is not confirmed. Both name one possible
// cause, not the cause (the audit row's reason says which check failed): a proxy image from before the
// readback, read as current because its reference names no release (digest only, no tag, :latest), or
// pinned on a runner whose own version decides, never reports.
const (
	unconfirmedImageCause      = ". One possible cause: a proxy image from before 0.10.0-rc.18, which never confirms; check WARDYN_PROXY_IMAGE, or a runner's --proxy-image, when upgrading the server"
	unconfirmedPolicyHint      = "The run's proxy did not confirm the policy it loaded, so it was stopped" + unconfirmedImageCause
	unconfirmedStopPendingHint = "The run's proxy did not confirm the policy it loaded; stopping it is not confirmed yet and is retried" + unconfirmedImageCause
)

// unconfirmedHint is the failure hint for a stop that was (stopped) or was not confirmed.
func unconfirmedHint(stopped bool) string {
	if stopped {
		return unconfirmedPolicyHint
	}
	return unconfirmedStopPendingHint
}

func (s *Server) auditReadback(ctx context.Context, runID uuid.UUID, action, outcome string, gen int, data map[string]any) {
	data["generation"] = gen
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", action, runID.String(), outcome, mustJSON(data)))
}

// auditReadbackFailure is a failed start's run.policy.unconfirmed row, with both hashes as evidence,
// written once its stop was tried: stopped only when c confirms it, else containment_error says why not.
func (s *Server) auditReadbackFailure(ctx context.Context, ch store.ProxyStartChange, c proxyContainment) {
	data := map[string]any{
		"reason": ch.Reason, "stopped": c.Contained,
		"expected_sha256": ch.Start.ExpectedSHA256, "reported_sha256": ch.Start.ReportedSHA256,
	}
	if c.Err != nil {
		data["containment_error"] = c.Err.Error()
	}
	s.auditReadback(ctx, ch.Start.RunID, "run.policy.unconfirmed", "failure", ch.Start.Generation, data)
}

// proxyLoadedReport is the body of POST /internal/proxy/loaded.
type proxyLoadedReport struct {
	ConfigSHA256 string          `json:"config_sha256"`
	ProxyVersion string          `json:"proxy_version"`
	Switches     json.RawMessage `json:"switches,omitempty"`
}

// handleInternalProxyLoaded is POST /internal/proxy/loaded: the run's proxy reports the SHA-256 of the
// config bytes it loaded, on its run token. The answer is 202 while the start is held for the driver's
// hash, 200 once it is settled, and 200 "stale" for a report outside the active start's token lineage,
// which changes nothing: every 2xx ends the proxy's retries.
func (s *Server) handleInternalProxyLoaded(w http.ResponseWriter, r *http.Request) {
	claims, err := claimsFromContext(r)
	if err != nil {
		writeErrorReason(w, http.StatusUnauthorized, reasonMissingRunClaims, "missing run claims")
		return
	}
	var body proxyLoadedReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil || !isSHA256Hex(body.ConfigSHA256) {
		writeErrorReason(w, http.StatusBadRequest, reasonProxyReportInvalid, "the report needs config_sha256, a lowercase hex SHA-256")
		return
	}
	st, ok := s.cfg.Store.(store.RunProxyStarts)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx := r.Context()
	ch, err := st.ReportProxyStart(ctx, claims.RunID, claims.JTI, body.ConfigSHA256)
	if err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonProxyReportUnrecorded, loggedMsg(ctx, "record the proxy's report", err))
		return
	}
	data := map[string]any{"proxy_version": body.ProxyVersion}
	if len(body.Switches) > 0 {
		data["switches"] = body.Switches
	}
	if !ch.Active {
		data["jti"] = claims.JTI
		s.auditReadback(ctx, claims.RunID, "run.policy.report_stale", "denied", ch.Start.Generation, data)
		writeJSON(w, http.StatusOK, map[string]any{"state": "stale"})
		return
	}
	switch ch.Settled {
	case types.ProxyStartEnforced:
		data["config_sha256"] = body.ConfigSHA256
		s.auditReadback(ctx, claims.RunID, "run.policy.enforced", "success", ch.Start.Generation, data)
	case types.ProxyStartFailed:
		ctx := context.WithoutCancel(ctx)
		s.auditReadbackFailure(ctx, ch, s.containProxyStart(ctx, claims.RunID, ch.Start.Generation))
	}
	status := http.StatusOK
	if ch.Start.State == types.ProxyStartPending {
		status = http.StatusAccepted
	}
	writeJSON(w, status, map[string]any{"state": ch.Start.State})
}

func isSHA256Hex(v string) bool {
	b, err := hex.DecodeString(v)
	return err == nil && len(b) == 32 && strings.ToLower(v) == v
}

// runPolicyReadback is the run read's policy_readback: its active start's state, generation and
// confirmed_at, never a hash. nil when the store keeps none, the run has none, or the read failed.
func (s *Server) runPolicyReadback(ctx context.Context, runID uuid.UUID) *types.RunPolicyReadback {
	st, ok := s.cfg.Store.(store.RunProxyStarts)
	if !ok {
		return nil
	}
	p, found, err := st.ActiveProxyStart(ctx, runID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading a run's policy readback failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return nil
	}
	if !found {
		return nil
	}
	return &types.RunPolicyReadback{State: p.State, Generation: p.Generation, ConfirmedAt: p.ConfirmedAt}
}

// recordProxyAddr stores the address the run's agent is pinned to for its proxy. A failed write is
// logged: that run's connect rows then read unknown, and the run goes on.
func (s *Server) recordProxyAddr(ctx context.Context, runID uuid.UUID, addr string) {
	st, ok := s.cfg.Store.(store.RunProxyStarts)
	if !ok || addr == "" {
		return
	}
	if err := st.SetRunProxyAddr(ctx, runID, addr); err != nil {
		slog.WarnContext(ctx, "wardynd: recording a run's proxy address failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// beginDispatchProxyStart records a dispatch's pending start, generation 1, on the run token's jti. A
// start that cannot be recorded fails the run before any create: its proxy could never confirm.
func (s *Server) beginDispatchProxyStart(ctx context.Context, run types.AgentRun, token string, done func()) (proxyStartRec, bool) {
	if _, ok := s.cfg.Store.(store.RunProxyStarts); !ok {
		return proxyStartRec{}, true
	}
	claims, err := s.cfg.Identity.Verify(ctx, token, internalAudience)
	var rec proxyStartRec
	if err == nil {
		rec, err = s.beginProxyStart(ctx, run, 1, claims.JTI)
	}
	if err != nil {
		done()
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "the run's proxy start could not be recorded")
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": err.Error(), "reason": "policy_unconfirmed"})))
		return rec, false
	}
	return rec, true
}

// recordConfirmedSandbox is recordStartedSandbox, then the driver's answer for start: false is a run
// already failed with its sandbox stopped, for an unrecorded ref or a start that cannot run.
func (s *Server) recordConfirmedSandbox(ctx context.Context, run types.AgentRun, sb runner.Sandbox, start proxyStartRec) bool {
	if err := s.recordStartedSandbox(ctx, run, sb); err != nil {
		s.abortUnrecordedSandbox(ctx, run.ID, sb.Ref, err)
		return false
	}
	if ok, why, failed := s.proxyStartAnswered(ctx, run.ID, start, sb.ProxyConfigSHA256); !ok {
		s.abortUnconfirmedSandbox(ctx, run.ID, sb.Ref, start, why, failed)
		return false
	}
	return true
}

// abortUnconfirmedSandbox stops a created sandbox whose start did not confirm (why), records that stop
// on a failed start (and in its run.policy.unconfirmed row), and fails the run. An unconfirmed stop
// is left to the sweep's retry.
func (s *Server) abortUnconfirmedSandbox(ctx context.Context, runID uuid.UUID, ref string, start proxyStartRec, why string, failed *store.ProxyStartChange) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.create",
		runID.String(), "failure", mustJSON(map[string]any{"sandbox_ref": ref, "reason": "policy_unconfirmed", "readback": why})))
	stopErr, stopped := "", s.stopSandboxOrAudit(ctx, runID, ref, "run.create")
	if !stopped {
		stopErr = errProxyStopUnconfirmed.Error()
	}
	s.failAndRevoke(ctx, runID, types.RunStarting, unconfirmedHint(stopped))
	if failed != nil {
		c := proxyContainment{Contained: stopped}
		if !stopped {
			c.Err = errProxyStopUnconfirmed
		}
		s.auditReadbackFailure(ctx, *failed, c)
	}
	if st, ok := s.cfg.Store.(store.RunProxyStarts); ok && start.active {
		if err := st.RecordProxyContainment(ctx, runID, start.gen, stopErr); err != nil {
			slog.WarnContext(ctx, "wardynd: recording a failed proxy start's stop failed; the sweep retries",
				slog.String("run_id", runID.String()), slog.Any("err", err))
		}
	}
}
