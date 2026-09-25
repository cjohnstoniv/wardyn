// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// Revive and restart with current limits (long-holds design rev 4, §4.1,
// RL-10 and RL-11). A run lost to a control-plane outage still has its agent
// running and its proxy stopped (run_lost.go). Revive gives it a new proxy
// built from the old one's own config, with only two things changed: a fresh
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
// is re-checked as run_owner_authority.go describes. The per-run MITM CA inside the config
// is carried over, and no copy of it is made anywhere else.

// reviveBulkMax bounds one admin restart request.
const reviveBulkMax = 100

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

// reviveError is a revive refusal: status is the HTTP answer, and lost says
// the proxy is gone and the run was put back to lost (outage).
type reviveError struct {
	status int
	msg    string
	lost   bool
}

func (e *reviveError) Error() string { return e.msg }

func reviveRefused(status int, msg string) *reviveError {
	return &reviveError{status: status, msg: msg}
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
		writeError(w, err.status, err.msg)
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
			"denied", mustJSON(map[string]any{"subject": run.CreatedBy, "reason": "local_mode_not_owner"})))
		return reviveResult{}, reviveRefused(http.StatusForbidden, "in local mode only the run's owner can revive it")
	}
	return s.reviveRunProxy(r.Context(), run, actorType, actor, startAgent)
}

// reviveRunProxy replaces run's proxy with its own config under a fresh token
// and the owner's current denies, then, for a run lost to a reboot, starts its
// agent. Every refusal before the claim leaves the run as it was, and so does
// a failure before a live run's old proxy is touched. Otherwise a proxy that
// cannot be replaced, or an agent that cannot be started, puts the run back to
// lost with its proxy and agent stopped, so it never runs without the proxy it
// was promised.
func (s *Server) reviveRunProxy(ctx context.Context, run types.AgentRun, actorType types.ActorType, actor string, startAgent bool) (reviveResult, *reviveError) {
	reviver, ok := s.cfg.Store.(store.RunReviver)
	if !ok || s.cfg.Runner == nil {
		return reviveResult{}, reviveRefused(http.StatusNotImplemented, "this deployment cannot revive a run")
	}
	rv, ok := s.cfg.Runner.(runner.ProxyReviver)
	if !ok {
		return reviveResult{}, reviveRefused(http.StatusConflict, runner.ErrReviveUnsupported.Error())
	}
	if rerr := s.reviveEligible(run, startAgent); rerr != nil {
		return reviveResult{}, rerr
	}
	rebooted, rerr := s.reviveNeedsAgentStart(ctx, run)
	if rerr != nil {
		return reviveResult{}, rerr
	}
	starter, canStart := s.cfg.Runner.(runner.SandboxStarter)
	if rebooted && !canStart {
		return reviveResult{}, reviveRefused(http.StatusConflict, runner.ErrReviveUnsupported.Error())
	}
	if rebooted && !startAgent {
		return reviveResult{}, reviveRefused(http.StatusConflict,
			"run's agent is stopped and a bulk restart cannot start it; revive it from the run's page")
	}
	if _, busy := s.reviving.LoadOrStore(run.ID, struct{}{}); busy {
		return reviveResult{}, reviveRefused(http.StatusConflict, "a revive of this run is already in progress")
	}
	defer s.reviving.Delete(run.ID)

	c, rerr := s.reviveCeiling(ctx, run)
	if rerr != nil {
		return reviveResult{}, rerr
	}
	old, err := rv.ProxyConfig(ctx, run.SandboxRef)
	if err != nil {
		if errors.Is(err, runner.ErrReviveUnsupported) {
			return reviveResult{}, reviveRefused(http.StatusConflict, err.Error())
		}
		return reviveResult{}, reviveRefused(http.StatusBadGateway, "read the run's proxy config: "+err.Error())
	}
	cfg, re, rerr := reassertProxyCeiling(run, old, c)
	if rerr != nil {
		return reviveResult{}, rerr
	}
	if rerr := s.reviveOwnerRecheck(ctx, run, cfg, actorType, actor); rerr != nil {
		return reviveResult{}, rerr
	}
	retiring := cfg.RunToken
	id, err := s.cfg.Identity.MintRunIdentity(ctx, run.ID, runIdentitySubject(ctx, run.CreatedBy), run.CreatedBy, internalAudience)
	if err != nil {
		return reviveResult{}, reviveRefused(http.StatusInternalServerError, "mint run identity: "+err.Error())
	}
	cfg.RunToken, cfg.ControlPlaneURL, cfg.ControlPlaneCAPEM = id.Token, s.cfg.ControlPlaneURL, s.cfg.ControlPlaneCAPEM
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return reviveResult{}, reviveRefused(http.StatusInternalServerError, "encode proxy config: "+err.Error())
	}

	// F2 (Fable review): a slow first pull of the proxy image belongs BEFORE
	// the claim below, never after — the claim is what makes a watcher sweep
	// leave this run alone (keepRebootedRun's busy check only helps once the
	// run IS claimed), so the window it opens must cover only a fast
	// docker create+start, not an image pull.
	if err := rv.EnsureProxyImage(ctx); err != nil {
		return reviveResult{}, reviveRefused(http.StatusBadGateway, "pull the proxy image: "+err.Error())
	}

	// The claim comes BEFORE the new proxy starts: once the run is no longer
	// lost, the lease sweep stops re-asserting the old proxy's stop, and the
	// fresh token stamp keeps the lapsed-token sweep off it. It lands only on
	// the row as read here, so a run read live still had its old proxy running.
	// It also refreshes the watcher lease, so the watcher sweep does not find
	// a rebooted agent not yet started and lose the run again.
	claimed, err := reviver.MarkRunRevived(ctx, run.ID, run.LostReason)
	if err != nil {
		return reviveResult{}, reviveRefused(http.StatusServiceUnavailable, "claim the run for revive: "+err.Error())
	}
	if !claimed {
		return reviveResult{}, reviveRefused(http.StatusConflict, "the run changed while it was being revived (it ended, was lost or was revived); try again")
	}
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
	if err := rv.ReplaceProxy(ctx, run.SandboxRef, cfgJSON); err != nil {
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
			return reviveResult{}, reviveRefused(http.StatusBadGateway,
				"the run's proxy was not replaced, and the run keeps its current proxy: "+err.Error())
		}
		// The old proxy is, or may be, gone: nothing is left to serve on the
		// retiring token, so revoking it here is safe (F4).
		data["lost_again"] = true
		s.revokeRetiringToken(ctx, retiring, run.ID)
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
		s.reloseRun(ctx, run)
		return reviveResult{}, &reviveError{status: http.StatusBadGateway, lost: true,
			msg: "the run's proxy could not be replaced, so the run has no egress and is lost until revived: " + err.Error()}
	}
	// The new proxy is up behind the fresh token: the retiring one is no
	// longer needed (O2), and only NOW is it safe to revoke (F4) — every
	// return above this point leaves the old proxy running unrevoked.
	s.revokeRetiringToken(ctx, retiring, run.ID)
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
		if err := starter.StartSandbox(ctx, run.SandboxRef); err != nil {
			data["error"], data["lost_again"] = "start agent: "+err.Error(), true
			s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
			s.reloseRun(ctx, run)
			return reviveResult{}, &reviveError{status: http.StatusBadGateway, lost: true,
				msg: "the run's agent could not be started, so the run is stopped and lost until revived: " + err.Error()}
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
	return reviveResult{RunID: run.ID, DeniedAdded: re.added, ProxyRelease: version.Version, AgentStarted: rebooted}, nil
}

// reviveEligible: a RUNNING run with a sandbox, inside its lease, that is live,
// lost to an outage, or (startAgent) lost to a reboot. An ended run has passed
// its end, and the bulk restart never starts a stopped agent.
func (s *Server) reviveEligible(run types.AgentRun, startAgent bool) *reviveError {
	switch {
	case run.State != types.RunRunning || run.SandboxRef == "":
		return reviveRefused(http.StatusConflict, "run is not running (state="+string(run.State)+")")
	case run.EndsAt != nil && !s.cfg.Now().Before(*run.EndsAt):
		return reviveRefused(http.StatusConflict, "run has passed its end; extend it first")
	case run.LostAt == nil, run.LostReason == types.LostOutage:
	case run.LostReason == types.LostReboot && startAgent:
	case run.LostReason == types.LostReboot:
		return reviveRefused(http.StatusConflict, "run was lost to a reboot and its agent is stopped; revive it from the run's page")
	default:
		return reviveRefused(http.StatusConflict, "run was lost ("+string(run.LostReason)+") and cannot be revived")
	}
	return nil
}

// reviveNeedsAgentStart reports whether a revive must start run's agent
// again, behind its new proxy, before it can be called live: always true for
// a reboot, and true for an outage run only when the agent itself is not
// running (F1.2, long-holds design rev 4 §4.1; Fable review).
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
	if run.LostReason == types.LostReboot {
		return true, nil
	}
	if run.LostReason != types.LostOutage {
		return false, nil
	}
	st, err := s.cfg.Runner.Status(ctx, run.SandboxRef)
	if err != nil {
		return false, reviveRefused(http.StatusServiceUnavailable, "check the run's agent status: "+err.Error())
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

func reviveFrom(run types.AgentRun) string {
	if run.LostAt == nil {
		return "live"
	}
	return string(run.LostReason)
}

// ownerCeiling is the part of the owner's profile a revive re-asserts: its
// denies, and its name for the audit row.
type ownerCeiling struct {
	deny    []string
	profile string
}

// reviveCeiling is the OWNER's current ceiling: the profile captured on the
// run at create, as it stands now. It is not a dispatch lane's ceiling (no
// principal is resolved; the run row names the profile). No captured profile
// (an unassigned or super-admin owner) has no profile denies, as at dispatch.
// A captured profile that no longer exists is refused: its walls cannot be
// known.
func (s *Server) reviveCeiling(ctx context.Context, run types.AgentRun) (ownerCeiling, *reviveError) {
	p, ref := s.ownerProfile(ctx, run)
	switch {
	case ref != nil:
		return ownerCeiling{}, reviveRefused(ref.status, ref.msg)
	case p == nil:
		return ownerCeiling{}, nil
	}
	return ownerCeiling{deny: p.Ceiling.DeniedDomains, profile: p.Name}, nil
}

// reasserted is what reassertProxyCeiling changed.
type reasserted struct {
	added, droppedInjection, droppedLane []string
}

// reassertProxyCeiling is reassertCeilingDenies over a rendered proxy config:
// it unions the ceiling's denies into the frozen policy and drops every
// injection, brokered PAT host and MITM host the ceiling denies. It never
// re-resolves the policy and never widens it.
//
// One lane cannot be dropped here. Dispatch withholds the git broker by
// emptying its grants AND unsetting WARDYN_GITHUB_GRANT_ID in the sandbox, and
// the proxy refuses the mint route for a brokered grant only while its grant
// map is non-empty (isBrokeredGitGrant). A running agent's env cannot change,
// so emptying the map would hand the sandbox a GitHub installation token on
// request. A revive whose ceiling denies a broker-managed host is refused.
func reassertProxyCeiling(run types.AgentRun, old []byte, c ownerCeiling) (*proxy.Config, reasserted, *reviveError) {
	cfg, err := proxy.LoadConfigBytes(old)
	if err != nil {
		return nil, reasserted{}, reviveRefused(http.StatusConflict, "the run's proxy config does not load: "+err.Error())
	}
	if cfg.RunID != run.ID {
		return nil, reasserted{}, reviveRefused(http.StatusConflict, "the run's proxy config names another run")
	}
	var re reasserted
	if len(c.deny) == 0 {
		return cfg, re, nil
	}
	if len(cfg.GitGrants) > 0 && ceilingDeniesAny(c.deny, gitBrokerManagedHosts) {
		return nil, re, reviveRefused(http.StatusConflict,
			"the owner's governance profile now denies GitHub, which this run's git broker needs; start a new run")
	}
	re.added = unionCeilingDenies(&cfg.Policy, c.deny)
	cfg.Injection = slices.DeleteFunc(cfg.Injection, func(in proxy.InjectionConfig) bool {
		if ceilingDenies(c.deny, in.Host) {
			re.droppedInjection = append(re.droppedInjection, in.Host)
			return true
		}
		return false
	})
	for host := range cfg.PATGrants {
		if ceilingDenies(c.deny, host) {
			re.droppedLane = append(re.droppedLane, host)
			delete(cfg.PATGrants, host)
		}
	}
	slices.Sort(re.droppedLane)
	cfg.MITMHosts = slices.DeleteFunc(cfg.MITMHosts, func(h string) bool { return ceilingDenies(c.deny, h) })
	return cfg, re, nil
}

// reloseRun is the fail-closed arm after a claim: the run's proxy is gone (or
// was never replaced), or its agent did not start, so it is marked lost again
// and its proxy stopped (a rebooted run's agent too), or torn down when it
// cannot be kept. A rebooted run stays lost (reboot), so the next revive
// starts its agent again.
func (s *Server) reloseRun(ctx context.Context, run types.AgentRun) {
	reason := types.LostOutage
	if run.LostReason == types.LostReboot {
		reason = types.LostReboot
	}
	run.LostAt, run.LostReason = nil, ""
	loser, ok := s.cfg.Store.(store.RunLoser)
	if ok && s.loseRun(ctx, loser, run, reason, types.RunFailed, 0) {
		return
	}
	slog.WarnContext(ctx, "wardynd: a run whose proxy could not be replaced cannot be kept; tearing it down",
		slog.String("run_id", run.ID.String()))
	s.reconcileFinalize(ctx, run.ID, types.RunFailed, run.SandboxRef, "its proxy could not be replaced at revive")
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
		writeError(w, http.StatusBadRequest, "run_ids must name 1 to "+strconv.Itoa(reviveBulkMax)+" runs")
		return
	}
	results := make([]adminRestartResult, 0, len(req.RunIDs))
	for _, id := range req.RunIDs {
		res := adminRestartResult{RunID: id}
		run, err := s.cfg.Store.GetRun(r.Context(), id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			res.Error = "run not found"
		case err != nil:
			res.Error = "read run: " + err.Error()
		default:
			out, rerr := s.reviveFromRequest(r, run, false)
			if rerr != nil {
				res.Error, res.LostAgain = rerr.msg, rerr.lost
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
		writeError(w, http.StatusNotImplemented, "this store cannot list proxy releases")
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
// unparseable release ("" before migration 0077) is outside.
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
