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

// releaseBefore reports whether v parses as a release older than readbackRelease. Anything that does
// not parse (empty, a development build, a digest) is not evidence, and reads as not older.
func releaseBefore(v string) bool {
	a, ok := releaseRank(strings.TrimPrefix(strings.TrimSpace(v), "v"))
	if !ok {
		return false
	}
	b, _ := releaseRank(readbackRelease)
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

// proxyStartMode is legacy only on positive evidence: a remote runner whose registered Version is a
// release older than readbackRelease, or an in-process driver whose proxy image tag is. Anything else,
// an unreadable runner row included, is expected.
func (s *Server) proxyStartMode(ctx context.Context, run types.AgentRun) string {
	if run.RunnerID != nil {
		rs, ok := s.cfg.Store.(store.RunnerStore)
		if !ok {
			return store.ProxyStartExpected
		}
		row, err := rs.GetRunner(ctx, *run.RunnerID)
		if err == nil && releaseBefore(row.Version) {
			return store.ProxyStartLegacy
		}
		return store.ProxyStartExpected
	}
	if tag := proxyImageRelease(s.cfg.ProxyImage); tag != "" && releaseBefore(tag) {
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
func (s *Server) proxyStartAnswered(ctx context.Context, runID uuid.UUID, rec proxyStartRec, sha string) (bool, string) {
	st, ok := s.cfg.Store.(store.RunProxyStarts)
	if !ok || !rec.active {
		return true, ""
	}
	ch, err := st.AnswerProxyStart(ctx, runID, rec.gen, sha, proxyReportGrace)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: recording a proxy start's answer failed; stopping it",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		s.auditReadback(ctx, runID, "run.policy.unconfirmed", "failure", rec.gen, map[string]any{"reason": "answer_unrecorded"})
		return false, "answer_unrecorded"
	}
	if !ch.Active {
		s.auditReadback(ctx, runID, "run.policy.unconfirmed", "failure", rec.gen, map[string]any{"reason": "start_superseded"})
		return false, "start_superseded"
	}
	switch ch.Settled {
	case types.ProxyStartUnconfirmed:
		s.auditReadback(ctx, runID, "run.policy.unconfirmed", "success", rec.gen, map[string]any{"reason": store.ProxyStartLegacy})
	case types.ProxyStartEnforced:
		s.auditReadback(ctx, runID, "run.policy.enforced", "success", rec.gen, map[string]any{"config_sha256": ch.Start.ExpectedSHA256})
	case types.ProxyStartFailed:
		s.auditReadbackFailure(ctx, ch)
		return false, ch.Reason
	default:
		s.watchProxyStart(runID, rec.gen)
	}
	return true, ""
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
		s.auditReadbackFailure(ctx, ch)
		s.containProxyStart(ctx, runID, gen)
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

// containProxyStart stops the proxy of the run's failed start gen and records the result on the start
// (store.ProxyStart.ContainedAt, or ContainmentError while unresolved). It is fenced on gen: once a
// newer start is the run's active one it stops nothing. A start already contained answers Contained
// with no second stop. It may run again, and on any replica.
func (s *Server) containProxyStart(ctx context.Context, runID uuid.UUID, gen int) proxyContainment {
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
		return proxyContainment{Err: fmt.Errorf("proxy start %d is %s, not failed", gen, p.State)}
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return proxyContainment{Err: err}
	}
	stopErr := s.stopUnconfirmedProxy(ctx, run)
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
// proxy stopped, or torn down when it cannot be kept, as the lapsed-token sweep does. A run already
// lost has its proxy stopped again, and any other run its sandbox: what a retry does.
func (s *Server) stopUnconfirmedProxy(ctx context.Context, run types.AgentRun) error {
	switch {
	case run.State == types.RunStarting:
		ok := s.stopSandboxOrAudit(ctx, run.ID, run.SandboxRef, "run.create")
		s.failAndRevoke(ctx, run.ID, types.RunStarting, unconfirmedPolicyHint)
		if !ok {
			return errProxyStopUnconfirmed
		}
		return nil
	case runIsKept(run):
		return s.stopLostSandbox(ctx, run, s.cfg.Now())
	case run.State == types.RunRunning:
		if handled, err := s.loseUnconfirmedRun(ctx, run); handled {
			return err
		}
		s.reconcileFinalize(ctx, run.ID, types.RunFailed, run.SandboxRef, unconfirmedPolicyHint)
	}
	if !s.stopSandboxOrAudit(ctx, run.ID, run.SandboxRef, "run.reconcile") {
		return errProxyStopUnconfirmed
	}
	return nil
}

// errProxyStopUnconfirmed is a sandbox stop that failed (its teardown_error is audited).
var errProxyStopUnconfirmed = errors.New("the sandbox stop failed")

// loseUnconfirmedRun marks a running run lost (outage) and stops its proxy, answering the stop's
// error. handled is false for a run that cannot be kept, which the caller tears down. A lost mark
// that could not be written still has the proxy stopped, as reloseRun does, and stays unresolved so a
// retry writes it.
func (s *Server) loseUnconfirmedRun(ctx context.Context, run types.AgentRun) (handled bool, err error) {
	loser, ok := s.cfg.Store.(store.RunLoser)
	leaser, lok := s.cfg.Store.(store.RunLeaser)
	if !ok || !lok {
		return false, nil
	}
	now := s.cfg.Now()
	switch o, stopErr := s.loseRunStopped(ctx, loser, leaser, run, types.LostOutage, types.RunFailed, 0, now); o {
	case loseNotKeepable:
		return false, nil
	case loseApplied:
		return true, stopErr
	case loseWriteFailed:
		s.revokeRunBroker(ctx, run.ID)
		run.LostAt, run.LostReason = &now, types.LostOutage
		if err := s.stopLostSandbox(ctx, run, now); err != nil {
			return true, err
		}
		return true, errors.New("the run's lost mark could not be written")
	default:
		return true, errors.New("the run's lost mark did not land")
	}
}

// unconfirmedPolicyHint is the failure hint of a run whose proxy did not confirm its policy.
const unconfirmedPolicyHint = "The run's proxy did not confirm the policy it loaded, so it was stopped"

func (s *Server) auditReadback(ctx context.Context, runID uuid.UUID, action, outcome string, gen int, data map[string]any) {
	data["generation"] = gen
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", action, runID.String(), outcome, mustJSON(data)))
}

// auditReadbackFailure is a failed start's run.policy.unconfirmed row, with both hashes as evidence.
func (s *Server) auditReadbackFailure(ctx context.Context, ch store.ProxyStartChange) {
	s.auditReadback(ctx, ch.Start.RunID, "run.policy.unconfirmed", "failure", ch.Start.Generation, map[string]any{
		"reason": ch.Reason, "stopped": true,
		"expected_sha256": ch.Start.ExpectedSHA256, "reported_sha256": ch.Start.ReportedSHA256,
	})
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
		writeError(w, http.StatusServiceUnavailable, loggedMsg(ctx, "record the proxy's report", err))
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
		s.auditReadbackFailure(ctx, ch)
		s.containProxyStart(context.WithoutCancel(ctx), claims.RunID, ch.Start.Generation)
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
	if ok, why := s.proxyStartAnswered(ctx, run.ID, start, sb.ProxyConfigSHA256); !ok {
		s.abortUnconfirmedSandbox(ctx, run.ID, sb.Ref, start, why)
		return false
	}
	return true
}

// abortUnconfirmedSandbox stops a created sandbox whose start did not confirm (why), records that stop
// on a failed start, and fails the run. An unconfirmed stop is left to the sweep's retry.
func (s *Server) abortUnconfirmedSandbox(ctx context.Context, runID uuid.UUID, ref string, start proxyStartRec, why string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.create",
		runID.String(), "failure", mustJSON(map[string]any{"sandbox_ref": ref, "reason": "policy_unconfirmed", "readback": why})))
	stopErr := ""
	if !s.stopSandboxOrAudit(ctx, runID, ref, "run.create") {
		stopErr = errProxyStopUnconfirmed.Error()
	}
	s.failAndRevoke(ctx, runID, types.RunStarting, unconfirmedPolicyHint)
	if st, ok := s.cfg.Store.(store.RunProxyStarts); ok && start.active {
		if err := st.RecordProxyContainment(ctx, runID, start.gen, stopErr); err != nil {
			slog.WarnContext(ctx, "wardynd: recording a failed proxy start's stop failed; the sweep retries",
				slog.String("run_id", runID.String()), slog.Any("err", err))
		}
	}
}
