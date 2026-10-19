// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// Revive and restart with current limits (long-holds design rev 4, §4.1,
// RL-10 and RL-11). A run lost to a control-plane outage still has its agent
// running and its proxy stopped and removed (run_lost.go). Revive gives it a
// new proxy built from the run's stored config (run_proxy_config.go), with
// only two things changed: a fresh
// run token, and the owner's CURRENT profile denies unioned over the frozen
// policy. The same path restarts a live run's proxy, which is how an admin
// brings a standing run onto current limits and a current proxy release.
//
// A run lost to a reboot has its agent stopped too, its files kept in the
// container's writable layer. Its revive is the same new proxy, and then the
// agent started again behind it (runner.SandboxStarter): never before, so the
// agent's first byte out goes through the rewritten config. The image's
// agent-run sees it has booted before and revives rather than re-seeding
// (`agent-run --revive`; Claude Code continues its conversation). Only the
// run's own page does this; the admin bulk restart is proxy-only.
//
// An outage run's agent is stopped the same way once its end passes while it
// is still lost, but its lost_reason stays outage (run_lost.go); this is
// detected by probing the agent's own status rather than trusted from the
// label, and takes the same start-again path a reboot does (F1.2, Fable
// review). The proxy image is pulled before the claim below, never after, so
// the window a watcher sweep must leave the run alone in covers only a fast
// create+start, not a pull (F2); keepRebootedRun (run_lost.go) is the guard
// that actually leaves it alone.
//
// Authority is the OWNER's, never the caller's: an admin's own ceiling is
// empty (effectiveCeiling's operator short-circuit), so resolving the caller
// would let an admin's click strip a member's limits. The ceiling comes from
// the profile captured on the run at create, and a run whose profile no longer
// exists is refused. The policy is never re-resolved; the rewrite only adds
// denies and removes credential lanes, and the rest of the owner's authority
// is re-checked as run_owner_authority.go describes. The old config comes
// from the run's stored row (run_proxy_config.go), never from the proxy
// container, and the per-run MITM CA inside it is carried over, so an agent
// still running keeps trusting its proxy.

// reviveBulkMax bounds one admin restart request.
const reviveBulkMax = 100

// reviveLiveEvery bounds how often a live run's proxy is replaced: each revive
// of a running run removes and recreates its sidecar, and a run that is lost
// is never bounded (#1005).
// ponytail: per process; a stamp on the run row if replicas ever multiply it.
const reviveLiveEvery = time.Minute

// jtiRevoker is an OPTIONAL identity.Provider capability: revoke a single
// token by its own jti, without revoking the whole run (O2, least-privilege
// credentials, owner law 2026-09-24). A revive mints a fresh run token; this
// is how it retires the OLD one on the spot rather than leaving it to lapse
// on its own TTL. The embedded provider's RevocationStore already carries
// RevokeJTI (cmd/wardynd/adapters.go); this is the narrow seam that reaches
// it without widening identity.Provider for every implementation.
type jtiRevoker interface {
	RevokeJTI(ctx context.Context, jti string, runID uuid.UUID) error
}

// reviveError is a revive refusal: status is the HTTP answer, reason the
// machine-readable wire reason (#656 slice 3 — the same one recordAudit
// already wrote for every refusal below), and lost says the proxy is gone
// and the run was put back to lost (outage).
type reviveError struct {
	status int
	msg    string
	reason string // errorBody.Reason, and adminRestartResult.Reason in a bulk restart
	lost   bool
}

func (e *reviveError) Error() string { return e.msg }

func reviveRefused(status int, reason, msg string) *reviveError {
	return &reviveError{status: status, reason: reason, msg: msg}
}

// reviveUnsupported is the refusal for a run whose substrate cannot replace
// its proxy (runner.ErrReviveUnsupported): a 409 with reasonReviveUnsupported,
// the same on the single-run revive and in each bulk-restart result.
func reviveUnsupported() *reviveError {
	return &reviveError{status: http.StatusConflict, msg: runner.ErrReviveUnsupported.Error(), reason: reasonReviveUnsupported}
}

// reviveResult is what a revive or restart reports.
type reviveResult struct {
	RunID        uuid.UUID `json:"run_id"`
	DeniedAdded  []string  `json:"denied_added"`
	ProxyRelease string    `json:"proxy_release"`
	AgentStarted bool      `json:"agent_started,omitempty"`
}

// handleReviveRun is POST /api/v1/runs/{id}/revive: the owner (or a super
// admin, acting with the owner's authority) gives a lost or live run a new
// proxy, and starts a rebooted run's agent again behind it.
func (s *Server) handleReviveRun(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin)
	if !ok {
		return
	}
	res, err := s.reviveFromRequest(r, run, true)
	if err != nil {
		writeErrorReason(w, err.status, err.reason, err.msg)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// reviveFromRequest applies the local-mode rule, then revives run. startAgent
// allows a run lost to a reboot, whose agent must be started again.
func (s *Server) reviveFromRequest(r *http.Request, run types.AgentRun, startAgent bool) (reviveResult, *reviveError) {
	actorType, actor := actorFromRequest(r)
	// Local mode mints for the host operator (runIdentitySubject), not for the
	// run's owner, so only the principal that created the run may revive it.
	if localPrincipalFromContext(r.Context()) != "" && actor != run.CreatedBy {
		// O4: audited like every other revive refusal, never silent.
		s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(),
			"denied", mustJSON(map[string]any{"subject": run.CreatedBy, "reason": reasonReviveLocalModeNotOwner})))
		return reviveResult{}, reviveRefused(http.StatusForbidden, reasonReviveLocalModeNotOwner, "in local mode only the run's owner can revive it")
	}
	return s.reviveRunProxy(r.Context(), run, actorType, actor, startAgent)
}

// reviveSupported is what reviveRunProxy needs from this deployment, and whether run's agent must be
// started (rebooted), or the refusal that leaves the run as it was.
func (s *Server) reviveSupported(ctx context.Context, run types.AgentRun, startAgent bool) (store.RunReviver, runner.ProxyReviver, runner.SandboxStarter, bool, *reviveError) {
	reviver, ok := s.cfg.Store.(store.RunReviver)
	if !ok || s.cfg.Runner == nil {
		return nil, nil, nil, false, reviveRefused(http.StatusNotImplemented, reasonReviveUnsupportedDeployment, "this deployment cannot revive a run")
	}
	rv, ok := s.cfg.Runner.(runner.ProxyReviver)
	if !ok {
		return nil, nil, nil, false, reviveUnsupported()
	}
	if rerr := s.reviveEligible(run, startAgent); rerr != nil {
		return nil, nil, nil, false, rerr
	}
	rebooted, rerr := s.reviveNeedsAgentStart(ctx, run)
	if rerr != nil {
		return nil, nil, nil, false, rerr
	}
	starter, canStart := s.cfg.Runner.(runner.SandboxStarter)
	if rebooted && !canStart {
		return nil, nil, nil, false, reviveUnsupported()
	}
	if rebooted && !startAgent {
		return nil, nil, nil, false, reviveRefused(http.StatusConflict, reasonReviveBulkCannotStartAgent,
			"run's agent is stopped and a bulk restart cannot start it; revive it from the run's page")
	}
	return reviver, rv, starter, rebooted, nil
}

// reviveRunProxy replaces run's proxy with its own config under a fresh token
// and the owner's current denies, then, for a run lost to a reboot, starts its
// agent. Every refusal before the claim leaves the run as it was, and so does
// a failure before a live run's old proxy is touched. Otherwise a proxy that
// cannot be replaced, or an agent that cannot be started, puts the run back to
// lost with its proxy and agent stopped, so it never runs without the proxy it
// was promised.
func (s *Server) reviveRunProxy(ctx context.Context, run types.AgentRun, actorType types.ActorType, actor string, startAgent bool) (reviveResult, *reviveError) {
	reviver, rv, starter, rebooted, rerr := s.reviveSupported(ctx, run, startAgent)
	if rerr != nil {
		return reviveResult{}, rerr
	}
	if rerr := s.markReviving(run); rerr != nil {
		return reviveResult{}, rerr
	}
	defer s.reviving.Delete(run.ID)
	// The run's lock (locks.go) is held from here to the settle, so a lease
	// pass that read the run's lost mark before the claim cannot stop the proxy
	// this revive starts. It is released after the settle, and after the
	// compensation of a failed one.
	ctx, unlockRun, rerr := s.lockRevive(ctx, run)
	if rerr != nil {
		return reviveResult{}, rerr
	}
	defer unlockRun()
	// One deadline bounds every call the revive makes to a runner while it holds the run's lock, so a
	// runner that stops answering cannot hold the lock, or a lock connection, past it.
	runnerDeadline := time.Now().Add(reviveRunnerTimeout)
	ctx, endRunnerCalls := withRunnerDeadline(ctx, run, runnerDeadline)
	defer endRunnerCalls()

	c, rerr := s.reviveCeilingAdmitting(ctx, run, actorType, actor)
	if rerr != nil {
		return reviveResult{}, rerr
	}
	cfg, rerr := s.reviveSourceConfig(ctx, rv, run, actorType, actor)
	if rerr != nil {
		return reviveResult{}, rerr
	}
	corpus := inspectionCorpusOf(cfg.Policy) // the organisation's, before a laptop copy drops it
	re, rerr := reassertProxyCeiling(run, cfg, c)
	if rerr != nil {
		if rerr.reason == reasonReviveCeilingDeniesGitBroker || rerr.reason == reasonReviveCeilingDeniesAzureCLI {
			s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
				mustJSON(map[string]any{"subject": run.CreatedBy, "reason": rerr.reason})))
		}
		return reviveResult{}, rerr
	}
	if rerr := s.reviveOwnerRecheck(ctx, run, cfg, actorType, actor); rerr != nil {
		return reviveResult{}, rerr
	}
	if rerr := s.reviveCapPrecheck(ctx, run, rebooted); rerr != nil {
		return reviveResult{}, rerr
	}
	if rerr := s.reviveRecoverContainment(ctx, run, actorType, actor); rerr != nil {
		return reviveResult{}, rerr
	}
	retiring := cfg.RunToken
	id, rerr := s.reviveMintIdentity(ctx, run)
	if rerr != nil {
		return reviveResult{}, rerr
	}
	cfg.RunToken = id.Token
	freshJTI := id.JTI
	cfgJSON, rerr := encodeRevivedConfig(ctx, cfg) // loaded as the new proxy will load it, before the claim
	if rerr != nil {
		return reviveResult{}, rerr
	}
	// The stored row stays the organisation's: a run on a runner keeps the inspection corpus its
	// laptop copy drops, as dispatch stores it (classifyLocalDispatch).
	storedJSON := cfgJSON
	if !remotePlacement(run.Placement) {
		org := *cfg
		org.Policy = corpus.restoredOn(cfg.Policy)
		if storedJSON, rerr = encodeRevivedConfig(ctx, &org); rerr != nil {
			return reviveResult{}, rerr
		}
	}

	// F2 (Fable review): a slow first pull of the proxy image belongs BEFORE
	// the claim below, never after — the claim is what makes a watcher sweep
	// leave this run alone (keepRebootedRun's busy check only helps once the
	// run IS claimed), so the window it opens must cover only a fast
	// docker create+start, not an image pull.
	if err := rv.EnsureProxyImage(ctx); err != nil {
		return reviveResult{}, reviveRefused(http.StatusBadGateway, reasonRevivePullImageFailed, loggedMsg(ctx, "pull the proxy image", err))
	}

	// The claim comes BEFORE the new proxy starts: once the run is no longer
	// lost, the lease sweep stops re-asserting the old proxy's stop, and the
	// fresh token stamp keeps the lapsed-token sweep off it. It lands only on
	// the row as read here, so a run read live still had its old proxy running.
	// It also refreshes the watcher lease, so the watcher sweep does not find
	// a rebooted agent not yet started and lose the run again.
	if rerr := s.claimRevive(ctx, reviver, run, rebooted, actorType, actor); rerr != nil {
		return reviveResult{}, rerr
	}
	// From the claim on, the revive finishes or compensates whatever becomes of
	// the request: a person closing the tab stops waiting for the answer, not
	// the consistency work. The context keeps the request's values (the audit
	// delegation) and loses its cancellation, bounded as the kill cascade is.
	// This is also what bounds how long the run's lock is held.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	// The runner calls below stay under the revive's one runner deadline; the compensation does not.
	runnerCtx, endClaimedRunnerCalls := withRunnerDeadline(ctx, run, runnerDeadline)
	defer endClaimedRunnerCalls()
	data := map[string]any{
		"subject":                 run.CreatedBy,
		"profile_id":              run.GovernanceProfileID,
		"profile":                 c.profile,
		"from":                    reviveFrom(run),
		"denied_added":            re.added,
		"denies_added":            len(re.added),
		"dropped_injection_hosts": re.droppedInjection,
		"dropped_broker_lanes":    re.droppedLane,
		"proxy_release":           version.Version,
	}
	s.stampLiveRevive(run)
	// The old proxy's renewed token ids are read before the new proxy starts: the new one renews at once,
	// stamping its own pair, which a read after the swap would take for the old proxy's.
	renewed := s.runTokenJTIs(ctx, run.ID)
	if err := s.replaceRevivedProxy(runnerCtx, rv, run, cfgJSON); err != nil {
		data["error"] = err.Error()
		if run.LostAt == nil && !errors.Is(err, runner.ErrProxyReplaceFailed) {
			// The old proxy was never touched (an image that cannot be pulled, a
			// failed inspect): a live run keeps it, renewing its own token. Only
			// the claim's token stamp moved. Its retiring token must stay live
			// too (F4, Fable review) — revoking it here would strand that kept,
			// untouched proxy on a token that can neither renew nor decide,
			// while its allowlisted egress keeps flowing audit-dark for up to
			// the lapsed-token sweep's ~1h05m window.
			s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
			s.liveRevived.Delete(run.ID) // nothing was replaced, so nothing to bound
			return reviveResult{}, reviveRefused(http.StatusBadGateway, reasonReviveProxyKeptCurrent,
				loggedMsg(ctx, "the run's proxy was not replaced, and the run keeps its current proxy", err))
		}
		// The old proxy is, or may be, gone: nothing is left to serve on the
		// retiring token, so revoking it here is safe (F4).
		s.revokeRetiringToken(ctx, retiring, run.ID)
		s.revokeRenewedTokens(ctx, run.ID, renewed, freshJTI)
		return reviveResult{}, s.reviveFailedAfterClaim(ctx, run, data, actorType, actor, reasonReviveProxyReplaceFailedLost,
			"the run's proxy could not be replaced, so the run has no egress and is lost until revived", err)
	}
	// The new proxy is up behind the fresh token: the retiring one is no
	// longer needed (O2), and only NOW is it safe to revoke (F4) — every
	// return above this point leaves the old proxy running unrevoked.
	s.revokeRetiringToken(ctx, retiring, run.ID)
	s.revokeRenewedTokens(ctx, run.ID, renewed, freshJTI)
	// The stored config becomes the one now running. A failed write keeps the
	// previous one, from which the next revive re-derives the same result.
	if err := s.storeRunProxyConfig(ctx, run.ID, storedJSON); err != nil {
		slog.WarnContext(ctx, "wardynd: storing a revived run's proxy config failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
	if rebooted {
		// F2: refresh the watcher lease right before starting the agent — on a
		// multi-replica deployment the claim's own stamp can go stale while
		// ReplaceProxy ran, and a sweep that then adopted this run would probe
		// an agent not yet started and lose it again mid-revive. Best-effort:
		// keepRebootedRun's busy check is the load-bearing guard; this only
		// narrows the window it has to cover.
		if wl, ok := s.cfg.Store.(store.RunWatcherLeaser); ok {
			if err := wl.HeartbeatRunWatcher(ctx, run.ID, watcherOwner); err != nil {
				slog.WarnContext(ctx, "wardynd: heartbeating a revived run's watcher lease before starting its agent failed",
					slog.String("run_id", run.ID.String()), slog.Any("err", err))
			}
		}
		// Only now, behind the new proxy. A failed start is lost (reboot) again,
		// which stops the new proxy with the agent.
		if err := s.startRevivedAgent(runnerCtx, starter, run); err != nil {
			data["error"] = "start agent: " + err.Error()
			return reviveResult{}, s.reviveFailedAfterClaim(ctx, run, data, actorType, actor, reasonReviveAgentStartFailedLost,
				"the run's agent could not be started, so the run is stopped and lost until revived", err)
		}
		data["agent_started"] = true
	}
	s.leaseEnded.Delete(run.ID)
	if err := reviver.SetRunProxyRelease(ctx, run.ID, version.Version); err != nil {
		// The row keeps the old release, so the proxy-window listing may name
		// this run again: a second restart, never a missed one.
		slog.WarnContext(ctx, "wardynd: recording a revived run's proxy release failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "success", mustJSON(data)))
	return reviveResult{RunID: run.ID, DeniedAdded: nonNilStrings(re.added), ProxyRelease: version.Version, AgentStarted: rebooted}, nil
}

// claimRevive is reviveRunProxy's claim (store.RunReviver.MarkRunRevived). A
// revive that starts the run's stopped agent (startsAgent, reviveNeedsAgentStart)
// takes a slot under its pool's cap, WARDYN_MAX_CONCURRENT_RUNS and its runner's
// max_runs, pool lock first, then the cap's lock, then the runner's, and at any of
// them is refused as a create is, the run left kept.
func (s *Server) claimRevive(ctx context.Context, reviver store.RunReviver, run types.AgentRun, startsAgent bool, actorType types.ActorType, actor string) *reviveError {
	claimed, err := reviver.MarkRunRevived(ctx, run.ID, run.LostReason, s.endedKept(run, s.cfg.Now()), s.cfg.MaxConcurrentRuns, startsAgent)
	if errors.Is(err, store.ErrRunCapReached) {
		return reviveRefused(http.StatusUnprocessableEntity, string(authz.ReasonRunQuota), runCapMsg)
	}
	// A revive takes a pool slot only when nobody waits (Q10-A): a waiting run is first in line.
	var full *store.RunnerPoolFullError
	if errors.As(err, &full) {
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
			mustJSON(map[string]any{"subject": run.CreatedBy, "reason": string(runnerpool.ReasonFull), "pool_id": run.RunnerPoolID})))
		return reviveRefused(runnerpool.ReasonFull.Status(), string(runnerpool.ReasonFull), runnerpool.FullMsg(full.Pool, full.Max))
	}
	var atMax *store.RunnerAtCapacityError
	if errors.As(err, &atMax) {
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
			mustJSON(map[string]any{"subject": run.CreatedBy, "reason": string(placement.ReasonRunnerAtCapacity), "runner_id": run.RunnerID})))
		return reviveRefused(placement.ReasonRunnerAtCapacity.Status(), string(placement.ReasonRunnerAtCapacity),
			fmt.Sprintf("%s is at its limit, so this run cannot start there now. Try again when a run there finishes, or raise the runner's limits.", atMax.Runner))
	}
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveClaimFailed, loggedMsg(ctx, "claim the run for revive", err))
	}
	if !claimed {
		return reviveRefused(http.StatusConflict, reasonReviveRunChanged, "the run changed while it was being revived (it ended, was lost or was revived); try again")
	}
	return nil
}

// reviveCapPrecheck refuses, before an identity is minted or the proxy image pulled, a revive that
// would start an agent while the deployment is at its cap. Lock-free and advisory: claimRevive's
// count under the cap lock stays the authority on a race. run is never counted, since an
// outage-kept run whose agent stopped can still read as holding a slot itself.
func (s *Server) reviveCapPrecheck(ctx context.Context, run types.AgentRun, startsAgent bool) *reviveError {
	c, ok := s.cfg.Store.(runCounter)
	if !startsAgent || s.cfg.MaxConcurrentRuns <= 0 || !ok {
		return nil
	}
	n, err := c.CountNonTerminalRuns(ctx, run.ID)
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveClaimFailed, loggedMsg(ctx, "count active runs", err))
	}
	if n >= s.cfg.MaxConcurrentRuns {
		return reviveRefused(http.StatusUnprocessableEntity, string(authz.ReasonRunQuota), runCapMsg)
	}
	return nil
}

// reviveFailedAfterClaim is the end of a revive whose proxy replace or agent
// start failed after the claim: the run is put back to lost, and the answer says
// "lost" only when that was persisted. When the lost mark could not be written
// the run is not lost on record, its proxy is stopped and its broker revoked
// anyway (reloseRun), and the answer says the recovery is unresolved, for a
// later sweep to settle. data carries the failure; err is the cause, logged and in the audit row, never in the answer.
func (s *Server) reviveFailedAfterClaim(ctx context.Context, run types.AgentRun, data map[string]any, actorType types.ActorType, actor, reason, msg string, err error) *reviveError {
	unresolved := s.reloseRun(ctx, run)
	data["lost_again"] = !unresolved
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
	if unresolved {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveRecoveryUnresolved,
			loggedMsg(ctx, "the revive failed and the run could not be recorded as lost; its proxy was stopped and its credentials revoked, and a later sweep recovers it", err))
	}
	return &reviveError{status: http.StatusBadGateway, reason: reason, lost: true, msg: loggedMsg(ctx, msg, err)}
}

// reviveRecoverContainment is a revive's answer for a run its runner holds contained after a settled end
// or stop_proxy (an outage loss contained on reconnect): with the owner and policy re-checked above, it
// queues the runner's recover_containment, ends the runner's session on this replica so its next fresh
// session applies it, and refuses this revive; the next revive, once the runner applied it, leases the run.
// A run held any other way (a kill, a quarantine, a hold pending a node record) is not recovered, and the
// revive goes on to the lease refusal it met before.
func (s *Server) reviveRecoverContainment(ctx context.Context, run types.AgentRun, actorType types.ActorType, actor string) *reviveError {
	if run.RunnerID == nil {
		return nil
	}
	st, err := s.runnerExecutionStore()
	if err != nil {
		return nil
	}
	if held, err := st.RunHeldContainedOnRunner(ctx, run.ID); err != nil || !held {
		return nil
	}
	a, queued, err := st.QueueRunnerContainmentRecovery(ctx, *run.RunnerID, run.ID)
	if err != nil {
		if !errors.Is(err, store.ErrRunnerActionRefused) && !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "wardynd: queueing a contained run's recovery failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
		return nil
	}
	if queued {
		// Its receipt is runner.action.apply or runner.action.refuse.
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, placement.ActionRunnerRecoveryRequest, a.ID.String(), "success",
			mustJSON(map[string]any{"subject": run.CreatedBy, "runner_id": a.RunnerID, "operation": a.Recovery.Operation, "marker_id": a.Recovery.ID})))
	}
	// Also for a recovery already queued: the session an earlier revive ended may have been on another
	// replica, or its end may have failed. A runner on another replica, or offline, reads it on its next session.
	if err := s.runnerHub(st).Goaway(ctx, *run.RunnerID); err != nil {
		slog.WarnContext(ctx, "wardynd: a contained run's recovery waits for its runner's next session", slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
	return reviveRefused(http.StatusConflict, reasonReviveContainmentRecovering,
		"the run's runner holds it contained since it was offline; a recovery is queued and the runner applies it when it next connects; revive the run again once it has")
}

// reviveSourceConfig is the config a revive starts from: the run's stored
// one (run_proxy_config.go), never the proxy container's (#1176), once the
// run's substrate says it can replace a proxy at all.
func (s *Server) reviveSourceConfig(ctx context.Context, rv runner.ProxyReviver, run types.AgentRun, actorType types.ActorType, actor string) (*proxy.Config, *reviveError) {
	if err := rv.CanReplaceProxy(ctx, run.SandboxRef); err != nil {
		if errors.Is(err, runner.ErrReviveUnsupported) {
			return nil, reviveUnsupported()
		}
		return nil, reviveRefused(http.StatusBadGateway, reasonReviveSubstrateUnreadable, loggedMsg(ctx, "resolve the run's substrate", err))
	}
	old, err := s.loadRunProxyConfig(ctx, run.ID)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, errRunProxyConfigNotKept):
		return nil, reviveRefused(http.StatusConflict, reasonReviveConfigNotStored,
			"the run's proxy config is not stored (it started before this release, or this deployment keeps none); start a new run")
	case err != nil:
		return nil, reviveRefused(http.StatusServiceUnavailable, reasonReviveConfigUnreadable, loggedMsg(ctx, "read the run's proxy config", err))
	}
	cfg, err := s.loadRenderedProxyConfig(old)
	if err != nil {
		return nil, reviveRefused(http.StatusConflict, reasonReviveConfigDoesNotLoad, "the run's proxy config does not load: "+err.Error())
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		return nil, reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable,
			loggedMsg(ctx, "read the run's git_pat grants", err))
	}
	if rerr := s.revivePATNarrowing(ctx, run, cfg, grants, actorType, actor); rerr != nil {
		return nil, rerr
	}
	// A config rendered under 0.8.5 has no brokered_pat_grant_ids, so the set is
	// recomputed rather than trusted, whatever the stored value says.
	cfg.BrokeredPATGrantIDs = brokeredPATGrantIDs(grants, !s.cfg.DisableGitPATBroker)
	// Likewise the narrowing: a run dispatched under 0.8.5 stored its allowlist without repos, access,
	// forge or api, and its grant row may carry them (0.8.5 ignored them). The revived proxy gets what
	// dispatch would give it. A grant whose row is gone has no scope to apply and revives as stored.
	if err := rescopeRevivedPATGrants(cfg.PATGrants, grants); err != nil {
		return nil, reviveRefused(http.StatusConflict, reasonReviveConfigDoesNotLoad,
			"a git_pat grant's scope could not be read, so what it is narrowed to is unknown: "+err.Error())
	}
	if rerr := s.reviveMCPBindings(ctx, run, cfg); rerr != nil {
		return nil, rerr
	}
	return cfg, nil
}

// rescopeRevivedPATGrants is scopePATGrants over the allowlist entries whose grant row still exists.
func rescopeRevivedPATGrants(allow map[string]proxy.PATGrant, rows []types.CredentialGrant) error {
	withRow := map[string]proxy.PATGrant{}
	for host, g := range allow {
		if slices.ContainsFunc(rows, func(r types.CredentialGrant) bool { return r.ID == g.GrantID && r.Spec.Kind == types.GrantGitPAT }) {
			withRow[host] = g
		}
	}
	if err := scopePATGrants(withRow, rows); err != nil {
		return err
	}
	maps.Copy(allow, withRow)
	return nil
}

// revivePATNarrowing asks dispatch's narrowing refusals (patNarrowingRefusal)
// again under the deployment as it is now. A narrowed git_pat grant revived
// after the PAT broker was turned off would keep its narrowed lane but lose the
// raw-mint refusal, so the PAT would reach the sandbox unnarrowed; the revive is
// refused and audited instead. A run with no git_pat grant reads nothing more.
func (s *Server) revivePATNarrowing(ctx context.Context, run types.AgentRun, cfg *proxy.Config,
	grants []types.CredentialGrant, actorType types.ActorType, actor string,
) *reviveError {
	if len(patGrantIDsOf(grants)) == 0 {
		return nil
	}
	site, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable,
			loggedMsg(ctx, "re-check the run's git_pat narrowing: read site config", err))
	}
	// The lane's hosts are the ones dispatch stored on the grant, which the
	// revived proxy carries: no repository is re-resolved, so revive cannot
	// disagree with dispatch about the lane.
	// The operator bit is not kept on the run: the owner's own revive asks their role now, as dispatch does
	// (runUngoverned); anyone else's reads OperatorOwned (the admin token, local mode) alone.
	operator := run.OperatorOwned || (actor != "" && actor == run.CreatedBy && s.runUngoverned(ctx))
	env := s.patNarrowingEnvOf(!s.cfg.DisableGitPATBroker, operator, cfg.GitGrants, site, adoEntraRun{}, false)
	if cfg.ADOGrant != nil {
		env.adoHosts = slices.Clone(cfg.ADOGrant.Hosts)
	}
	reason, detail := brokerOffPATRefusal(brokeredLaneSpecs(grants), env)
	if reason == "" {
		reason, detail = patNarrowingRefusal(patGrantSpecs(grants), env)
	}
	if reason == "" {
		return nil
	}
	detail = "the run cannot be revived as it was dispatched: " + detail
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure",
		mustJSON(map[string]any{"subject": run.CreatedBy, "reason": reason, "error": detail})))
	return reviveRefused(http.StatusConflict, reason, detail)
}

// stripRevivedModelInjections is dispatch's strip (dropLegacyModelInjections)
// over a stored proxy config (#548). A revive or admin restart replays the
// config the run was dispatched with, so a run dispatched before 0.8.2 can
// still carry a model credential no provider authored: a policy's api_key
// grant reading the operator's key, or a managed or subscription sentinel.
// Every injection the strip would drop goes, audited as run.injection.drop,
// unless its grant's snapshot names the UID, as it stands now, of a provider
// the run was launched with for any harness of its set (runHarnessPairs) —
// the grants the provider arms author — or an AWS CLI row the run carries,
// whose portal grant names the AWS SSO sentinel without being a model
// credential. A run left with no model
// credential revives without one, as dispatch leaves a run no provider serves:
// never a fallback to the operator's. A site config or grant list that cannot
// be read refuses the revive.
func (s *Server) stripRevivedModelInjections(ctx context.Context, run types.AgentRun, cfg *proxy.Config) *reviveError {
	if len(cfg.Injection) == 0 {
		return nil
	}
	unreadable := func(what string, err error) *reviveError {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable,
			loggedMsg(ctx, "re-check the run's model credentials: read "+what, err))
	}
	siteCfg, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return unreadable("site config", err)
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		return unreadable("the run's grants", err)
	}
	pairs, err := s.runHarnessPairs(ctx, run)
	if err != nil {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable, loggedMsg(ctx, "re-check the run's model credentials", err))
	}
	var uids []string
	for _, h := range pairs {
		if p, ok := modelProviderByID(siteCfg.ModelProviders, h.ModelProviderID); ok && h.ModelProviderID != "" && p.UID != "" {
			uids = append(uids, p.UID)
		}
	}
	cliRows, err := s.runAWSCLIRows(ctx, run.ID)
	if err != nil {
		return unreadable("the run's components", err)
	}
	authored, cliGrants := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	for _, g := range grants {
		var sc struct {
			Snapshot struct {
				ProviderUID string `json:"provider_uid"`
				ComponentID string `json:"component_id"`
				Mechanism   string `json:"mechanism"`
			} `json:"snapshot"`
		}
		if json.Unmarshal(g.Spec.Scope, &sc) != nil {
			continue
		}
		switch {
		case sc.Snapshot.Mechanism == cliAWSMechanism && cliRows[sc.Snapshot.ComponentID]:
			cliGrants[g.ID] = true
		case sc.Snapshot.ProviderUID != "" && slices.Contains(uids, sc.Snapshot.ProviderUID):
			authored[g.ID] = true
		}
	}
	model := s.modelCredentialInjection(nil, s.modelServingHosts(siteCfg))
	cfg.Injection = slices.DeleteFunc(cfg.Injection, func(in proxy.InjectionConfig) bool {
		// An AWS CLI row's portal grant is that lane's own, not a model
		// credential: the sentinel on a grant whose snapshot names an AWS CLI row
		// the run carries. Every resolve re-checks the row, the run and the owner
		// (cliSSOScopeAt).
		cli := cliGrants[in.GrantID] && in.SecretName == types.AWSSSOAccessTokenSecret
		if authored[in.GrantID] || cli || !model(in.InjectionRule) {
			return false
		}
		s.auditDroppedInjection(ctx, run, runner.InjectionGrant{GrantID: in.GrantID, Rule: in.InjectionRule}, "model_credential_not_provider_authored")
		return true
	})
	return nil
}

// reviveEligible: a RUNNING run with a sandbox, inside its lease, that is live,
// lost to an outage, or (startAgent) lost to a reboot, or an interactive run
// its own end stopped, extended since and still inside its files grace (#1061).
// The bulk restart never starts a stopped agent. The claim re-checks the grace
// and the end (store.EndedKept); this is the answer a person can act on.
func (s *Server) reviveEligible(run types.AgentRun, startAgent bool) *reviveError {
	now := s.cfg.Now()
	ended := run.LostReason == types.LostEnded
	switch {
	case run.State != types.RunRunning || run.SandboxRef == "":
		return reviveRefused(http.StatusConflict, reasonReviveNotRunning, "run is not running (state="+string(run.State)+")")
	case ended && !s.endedFilesKept(run, now):
		return reviveRefused(http.StatusConflict, reasonReviveEndedFilesGone, "run has ended and its files are no longer kept; start a new run")
	case run.EndsAt != nil && !now.Before(*run.EndsAt):
		return reviveRefused(http.StatusConflict, reasonRevivePastEnd, "run has passed its end; extend it first")
	case ended && !run.Interactive:
		// A task run's agent is the task, started once by dispatch: starting
		// its container again brings back the files and no agent.
		return reviveRefused(http.StatusConflict, reasonReviveEndedTaskRun, "run has ended and a task run's agent cannot be started again; start a new run")
	case run.LostAt == nil, run.LostReason == types.LostOutage:
	case (run.LostReason == types.LostReboot || ended) && startAgent:
	case run.LostReason == types.LostReboot:
		return reviveRefused(http.StatusConflict, reasonReviveRebootAgentStopped, "run was lost to a reboot and its agent is stopped; revive it from the run's page")
	case ended:
		return reviveRefused(http.StatusConflict, reasonReviveEndedAgentStopped, "run has ended and its agent is stopped; revive it from the run's page")
	default:
		return reviveRefused(http.StatusConflict, reasonReviveUnknownLostReason, "run was lost ("+string(run.LostReason)+") and cannot be revived")
	}
	return nil
}

// markReviving takes run's reviving mark. It refuses a second revive of the
// run in flight, and a revive of a live run whose proxy this process started
// replacing less than reviveLiveEvery ago (#1005). Only an attempt that
// reaches the proxy is stamped (stampLiveRevive), so a refusal before it
// changes nothing; the mark keeps the check and the stamp from interleaving
// with another revive of the same run.
func (s *Server) markReviving(run types.AgentRun) *reviveError {
	if _, busy := s.reviving.LoadOrStore(run.ID, struct{}{}); busy {
		return reviveRefused(http.StatusConflict, reasonReviveAlreadyInProgress, "a revive of this run is already in progress")
	}
	if last, ok := s.liveRevived.Load(run.ID); ok && run.LostAt == nil && s.cfg.Now().Before(last.(time.Time).Add(reviveLiveEvery)) {
		s.reviving.Delete(run.ID)
		return reviveRefused(http.StatusTooManyRequests, reasonReviveLiveTooSoon,
			"this run's proxy was replaced less than a minute ago; try again in a minute")
	}
	return nil
}

// stampLiveRevive records that a live run's proxy is being replaced now; the
// entry drops itself once it has lapsed.
func (s *Server) stampLiveRevive(run types.AgentRun) {
	if run.LostAt != nil {
		return
	}
	now := s.cfg.Now()
	s.liveRevived.Store(run.ID, now)
	time.AfterFunc(reviveLiveEvery, func() { s.liveRevived.CompareAndDelete(run.ID, now) })
}

// reviveNeedsAgentStart reports whether a revive must start run's agent
// again, behind its new proxy, before it can be called live: always true for
// a reboot or an end (both stop the agent), and true for an outage run only
// when the agent itself is not running (F1.2, long-holds design rev 4 §4.1;
// Fable review).
//
// stopLostSandbox (run_lost.go) stops an outage run's agent too, once its end
// passes while it is still lost, but leaves lost_reason=outage —
// relabeling it there would add a second writer racing the very sweeps that
// already set it. Trusting the agent's OWN status instead of the label is
// smaller and safer: a probe is read-only and can never drift, where a
// relabel CAS could land wrong and stay wrong.
//
// A probe error refuses (503) rather than silently answering false (Minor,
// Fable review): treating "could not observe" the same as "confirmed
// running" would let a proxy-only revive of an outage run whose agent is
// actually stopped report success while the agent stays dead behind a proxy
// that now claims it is live.
func (s *Server) reviveNeedsAgentStart(ctx context.Context, run types.AgentRun) (bool, *reviveError) {
	if run.LostReason == types.LostReboot || run.LostReason == types.LostEnded {
		return true, nil
	}
	if run.LostReason != types.LostOutage {
		return false, nil
	}
	st, err := s.cfg.Runner.Status(ctx, run.SandboxRef)
	if err != nil {
		return false, reviveRefused(http.StatusServiceUnavailable, reasonReviveAgentStatusUnreadable, loggedMsg(ctx, "check the run's agent status", err))
	}
	return st.State != types.RunRunning, nil
}

// revokeRetiringToken retires the OLD run token's own jti once its proxy is
// confirmed gone or replaced (O2, least-privilege credentials, owner law
// 2026-09-24): a live old token must not go on answering /internal/* calls
// until its TTL lapses on its own. Best-effort and silent on a miss: an
// already-expired retiring token fails Verify on expiry before revocation is
// even reached, so there is nothing to revoke, and a provider without
// single-jti revocation is not held back.
//
// CALLER MUST wait until ReplaceProxy has returned before calling this (F4,
// Fable review): calling it any earlier — before the claim, before
// EnsureProxyImage, before ReplaceProxy — can revoke a LIVE run's still-
// serving old proxy's only token when the replace never even starts (a
// refused claim, an image pull failure). That proxy would then be unable to
// renew or answer a decision while its allowlisted egress keeps flowing,
// audit-dark, until the lapsed-token sweep's ~1h05m window loses it. The one
// call site that leaves an untouched live proxy running (reviveRunProxy's
// "old proxy was never touched" arm) MUST NOT call this either, for the same
// reason.
func (s *Server) revokeRetiringToken(ctx context.Context, retiring string, runID uuid.UUID) {
	claims, verr := s.cfg.Identity.Verify(ctx, retiring, internalAudience)
	if verr != nil {
		return
	}
	jr, ok := s.cfg.Identity.(jtiRevoker)
	if !ok {
		return
	}
	if err := jr.RevokeJTI(ctx, claims.JTI, runID); err != nil {
		slog.WarnContext(ctx, "wardynd: revoking a revived run's retiring token failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// runTokenJTIs reads the ids the run's last token renewal recorded (the minted token, and the one it presented,
// which the proxy keeps when the lease send behind a renewal fails). A failed read revokes nothing later.
func (s *Server) runTokenJTIs(ctx context.Context, runID uuid.UUID) []string {
	loser, ok := s.cfg.Store.(store.RunLoser)
	if !ok {
		return nil
	}
	jtis, err := loser.RunTokenJTIs(ctx, runID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading a revived run's renewed token ids failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return nil
	}
	return jtis
}

// revokeRenewedTokens revokes the tokens the replaced proxy renewed since it started: the config the revive
// reads carries the token the proxy began with, but each renewal minted a newer one that would otherwise
// stay valid until its own expiry. jtis are the ids read before the swap (runTokenJTIs), except keep, the
// fresh token's. Same caller rule as revokeRetiringToken: only once the old proxy is, or may be, gone.
func (s *Server) revokeRenewedTokens(ctx context.Context, runID uuid.UUID, jtis []string, keep string) {
	jr, ok := s.cfg.Identity.(jtiRevoker)
	if !ok {
		return
	}
	for _, jti := range jtis {
		if jti == "" || jti == keep {
			continue
		}
		if err := jr.RevokeJTI(ctx, jti, runID); err != nil {
			slog.WarnContext(ctx, "wardynd: revoking a revived run's renewed token failed",
				slog.String("run_id", runID.String()), slog.Any("err", err))
		}
	}
}

func reviveFrom(run types.AgentRun) string {
	if run.LostAt == nil {
		return "live"
	}
	return string(run.LostReason)
}

// reloseRun is the fail-closed arm after a claim: the run's proxy is gone (or
// was never replaced), or its agent did not start, so it is marked lost again
// and its proxy stopped (a rebooted run's agent too), or torn down when it
// cannot be kept. A rebooted run stays lost (reboot), so the next revive
// starts its agent again. An ended run goes back to ended under its FIRST mark,
// so a failed revive never restarts its files grace.
//
// It reports whether the recovery is unresolved: the lost mark could not be
// written, so nothing says the run is lost. The run is contained anyway, its
// proxy stopped and its broker credentials revoked, and a sweep settles the
// rest; the caller must not tell anyone the run is lost.
func (s *Server) reloseRun(ctx context.Context, run types.AgentRun) (unresolved bool) {
	reason, at := types.LostOutage, s.cfg.Now()
	switch run.LostReason {
	case types.LostReboot:
		reason = types.LostReboot
	case types.LostEnded:
		reason, at = types.LostEnded, *run.LostAt
	}
	run.LostAt, run.LostReason = nil, ""
	loser, ok := s.cfg.Store.(store.RunLoser)
	leaser, lok := s.cfg.Store.(store.RunLeaser)
	if ok && lok {
		switch s.loseRun(ctx, loser, leaser, run, reason, types.RunFailed, 0, at) {
		case loseWriteFailed:
			s.revokeRunBroker(ctx, run.ID)
			run.LostAt, run.LostReason = &at, reason
			if err := s.stopLostSandbox(ctx, run, s.cfg.Now()); err != nil {
				slog.WarnContext(ctx, "wardynd: stopping a run's proxy after its lost mark could not be written failed",
					slog.String("run_id", run.ID.String()), slog.Any("err", err))
			}
			return true
		case loseApplied, loseNotApplied:
			return false
		}
	}
	slog.WarnContext(ctx, "wardynd: a run whose proxy could not be replaced cannot be kept; tearing it down",
		slog.String("run_id", run.ID.String()))
	s.reconcileFinalize(ctx, run.ID, types.RunFailed, run.SandboxRef, "its proxy could not be replaced at revive")
	return false
}

// adminRestartRequest is POST /api/v1/admin/runs/restart's body.
type adminRestartRequest struct {
	RunIDs []uuid.UUID `json:"run_ids"`
}

// adminRestartResult is one run's outcome in a bulk restart.
type adminRestartResult struct {
	RunID       uuid.UUID `json:"run_id"`
	OK          bool      `json:"ok"`
	Error       string    `json:"error,omitempty"`
	Reason      string    `json:"reason,omitempty"` // the single-run revive's errorBody.Reason for the same refusal
	LostAgain   bool      `json:"lost_again,omitempty"`
	DeniedAdded []string  `json:"denied_added,omitempty"`
}

// handleAdminRestartRuns is POST /api/v1/admin/runs/restart, the bulk
// "Restart with current limits": each named run gets a new proxy on the
// current release under its OWNER's current denies, one at a time, each
// audited as run.revive.
func (s *Server) handleAdminRestartRuns(w http.ResponseWriter, r *http.Request) {
	var req adminRestartRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if len(req.RunIDs) == 0 || len(req.RunIDs) > reviveBulkMax {
		writeErrorReason(w, http.StatusBadRequest, reasonReviveAdminRestartCountInvalid, "run_ids must name 1 to "+strconv.Itoa(reviveBulkMax)+" runs")
		return
	}
	results := make([]adminRestartResult, 0, len(req.RunIDs))
	for _, id := range req.RunIDs {
		res := adminRestartResult{RunID: id}
		if r.Context().Err() != nil {
			// A cancelled request settles the run in flight (reviveRunProxy) and
			// refuses the rest before their claim.
			res.Error = "the request was cancelled before this run was restarted"
			results = append(results, res)
			continue
		}
		run, err := s.cfg.Store.GetRun(r.Context(), id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			res.Error = "run not found"
		case err != nil:
			res.Error = "read run: " + err.Error()
		default:
			out, rerr := s.reviveFromRequest(r, run, false)
			if rerr != nil {
				res.Error, res.Reason, res.LostAgain = rerr.msg, rerr.reason, rerr.lost
			} else {
				res.OK, res.DeniedAdded = true, out.DeniedAdded
			}
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleAdminProxyWindow is GET /api/v1/admin/runs/proxy-window: every live
// run whose proxy was started by a release outside the supported window
// (wardynd N and N-1, the window the internal-API contract test pins), or by
// a release it cannot place. These are the runs to restart.
func (s *Server) handleAdminProxyWindow(w http.ResponseWriter, r *http.Request) {
	reviver, ok := s.cfg.Store.(store.RunReviver)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonReviveProxyWindowStoreUnavailable, "this store cannot list proxy releases")
		return
	}
	runs, err := reviver.ListRunProxyReleases(r.Context())
	if err != nil {
		writeServerError(w, r, "list run proxy releases", err)
		return
	}
	outside := make([]store.RunProxyRelease, 0)
	for _, run := range runs {
		if !inProxyWindow(version.Version, run.Release) {
			outside = append(outside, run)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"release": version.Version,
		"window":  proxyWindow(version.Version),
		"outside": outside,
	})
}

// inProxyWindow: release is the current minor or the one before it. An
// unparseable release ("" before migration 0084) is outside.
func inProxyWindow(current, release string) bool {
	cm, cn, ok := majorMinor(current)
	rm, rn, rok := majorMinor(release)
	return ok && rok && cm == rm && (rn == cn || rn == cn-1)
}

// proxyWindow names the supported minors, newest first.
func proxyWindow(current string) []string {
	m, n, ok := majorMinor(current)
	if !ok {
		return nil
	}
	w := []string{fmt.Sprintf("%d.%d", m, n)}
	if n > 0 {
		w = append(w, fmt.Sprintf("%d.%d", m, n-1))
	}
	return w
}

func majorMinor(v string) (int, int, bool) {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	m, err1 := strconv.Atoi(parts[0])
	n, err2 := strconv.Atoi(parts[1])
	return m, n, err1 == nil && err2 == nil
}

// reviveMintIdentity mints the revived run's new identity. A service run's sponsor is read from the identity's
// row, so a sponsor change reaches the new token.
func (s *Server) reviveMintIdentity(ctx context.Context, run types.AgentRun) (identity.RunIdentity, *reviveError) {
	sponsor, err := s.runSponsor(ctx, run.CreatedBy)
	if errors.Is(err, errServiceRetired) {
		return identity.RunIdentity{}, reviveRefused(http.StatusConflict, reasonServiceIdentityInactive, "the service identity that started this run is retired")
	}
	if err != nil {
		return identity.RunIdentity{}, reviveRefused(http.StatusInternalServerError, reasonReviveMintIdentityFailed, loggedMsg(ctx, "read run sponsor", err))
	}
	id, err := s.cfg.Identity.MintRunIdentity(ctx, run.ID, runIdentitySubject(ctx, run.CreatedBy), sponsor, internalAudience, run.OperatorOwned)
	if err != nil {
		return identity.RunIdentity{}, reviveRefused(http.StatusInternalServerError, reasonReviveMintIdentityFailed, loggedMsg(ctx, "mint run identity", err))
	}
	return id, nil
}
