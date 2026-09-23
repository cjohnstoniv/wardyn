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

// Proxy-only revive and restart with current limits (long-holds design rev 4,
// §4.1, RL-10). A run lost to a control-plane outage still has its agent
// running and its proxy stopped (run_lost.go). Revive gives it a new proxy
// built from the old one's own config, with only two things changed: a fresh
// run token, and the owner's CURRENT profile denies unioned over the frozen
// policy. The same path restarts a live run's proxy, which is how an admin
// brings a standing run onto current limits and a current proxy release.
//
// Authority is the OWNER's, never the caller's: an admin's own ceiling is
// empty (effectiveCeiling's operator short-circuit), so resolving the caller
// would let an admin's click strip a member's limits. The ceiling comes from
// the profile captured on the run at create, and a run whose profile no longer
// exists is refused. The policy is never re-resolved; the rewrite only adds
// denies and removes credential lanes. The per-run MITM CA inside the config
// is carried over, and no copy of it is made anywhere else.

// reviveBulkMax bounds one admin restart request.
const reviveBulkMax = 100

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
}

// handleReviveRun is POST /api/v1/runs/{id}/revive: the owner (or a super
// admin, acting with the owner's authority) gives a lost or live run a new
// proxy.
func (s *Server) handleReviveRun(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin)
	if !ok {
		return
	}
	res, err := s.reviveFromRequest(r, run)
	if err != nil {
		writeError(w, err.status, err.msg)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// reviveFromRequest applies the local-mode rule, then revives run.
func (s *Server) reviveFromRequest(r *http.Request, run types.AgentRun) (reviveResult, *reviveError) {
	actorType, actor := actorFromRequest(r)
	// Local mode mints for the host operator (runIdentitySubject), not for the
	// run's owner, so only the principal that created the run may revive it.
	if localPrincipalFromContext(r.Context()) != "" && actor != run.CreatedBy {
		return reviveResult{}, reviveRefused(http.StatusForbidden, "in local mode only the run's owner can revive it")
	}
	return s.reviveRunProxy(r.Context(), run, actorType, actor)
}

// reviveRunProxy replaces run's proxy with its own config under a fresh token
// and the owner's current denies. Every refusal before the claim leaves the run
// as it was. After the claim, a proxy that cannot be replaced puts the run back
// to lost (outage), so it never runs without the proxy it was promised.
func (s *Server) reviveRunProxy(ctx context.Context, run types.AgentRun, actorType types.ActorType, actor string) (reviveResult, *reviveError) {
	reviver, ok := s.cfg.Store.(store.RunReviver)
	if !ok || s.cfg.Runner == nil {
		return reviveResult{}, reviveRefused(http.StatusNotImplemented, "this deployment cannot revive a run")
	}
	rv, ok := s.cfg.Runner.(runner.ProxyReviver)
	if !ok {
		return reviveResult{}, reviveRefused(http.StatusConflict, runner.ErrReviveUnsupported.Error())
	}
	if rerr := s.reviveEligible(run); rerr != nil {
		return reviveResult{}, rerr
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
	id, err := s.cfg.Identity.MintRunIdentity(ctx, run.ID, runIdentitySubject(ctx, run.CreatedBy), run.CreatedBy, internalAudience)
	if err != nil {
		return reviveResult{}, reviveRefused(http.StatusInternalServerError, "mint run identity: "+err.Error())
	}
	cfg.RunToken, cfg.ControlPlaneURL = id.Token, s.cfg.ControlPlaneURL
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return reviveResult{}, reviveRefused(http.StatusInternalServerError, "encode proxy config: "+err.Error())
	}

	// The claim comes BEFORE the new proxy starts: once the run is no longer
	// lost, the lease sweep stops re-asserting the old proxy's stop, and the
	// fresh token stamp keeps the lapsed-token sweep off it.
	claimed, err := reviver.MarkRunRevived(ctx, run.ID, version.Version)
	if err != nil {
		return reviveResult{}, reviveRefused(http.StatusServiceUnavailable, "claim the run for revive: "+err.Error())
	}
	if !claimed {
		return reviveResult{}, reviveRefused(http.StatusConflict, "run is no longer running or revivable")
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
		data["lost_again"] = true
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "failure", mustJSON(data)))
		s.reloseRun(ctx, run)
		return reviveResult{}, &reviveError{status: http.StatusBadGateway, lost: true,
			msg: "the run's proxy could not be replaced, so the run has no egress and is lost until revived: " + err.Error()}
	}
	s.leaseEnded.Delete(run.ID)
	s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "success", mustJSON(data)))
	return reviveResult{RunID: run.ID, DeniedAdded: re.added, ProxyRelease: version.Version}, nil
}

// reviveEligible: a RUNNING run with a sandbox, inside its lease, that is live
// or lost to an outage. A reboot left its agent stopped and an ended run has
// passed its end; both need more than a new proxy.
func (s *Server) reviveEligible(run types.AgentRun) *reviveError {
	switch {
	case run.State != types.RunRunning || run.SandboxRef == "":
		return reviveRefused(http.StatusConflict, "run is not running (state="+string(run.State)+")")
	case run.EndsAt != nil && !s.cfg.Now().Before(*run.EndsAt):
		return reviveRefused(http.StatusConflict, "run has passed its end; extend it first")
	case run.LostAt != nil && run.LostReason != types.LostOutage:
		return reviveRefused(http.StatusConflict, "run was lost to a "+string(run.LostReason)+
			"; its agent is stopped, and a new proxy alone cannot bring it back")
	}
	return nil
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
	if run.GovernanceProfileID == nil {
		return ownerCeiling{}, nil
	}
	profiles, err := s.cfg.Store.ListGovernanceProfiles(ctx)
	if err != nil {
		return ownerCeiling{}, reviveRefused(http.StatusServiceUnavailable, "resolve the owner's governance profile: "+err.Error())
	}
	i := slices.IndexFunc(profiles, func(p types.GovernanceProfile) bool { return p.ID == *run.GovernanceProfileID })
	if i < 0 {
		return ownerCeiling{}, reviveRefused(http.StatusConflict,
			"the governance profile this run was created under no longer exists; start a new run")
	}
	return ownerCeiling{deny: profiles[i].Ceiling.DeniedDomains, profile: profiles[i].Name}, nil
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
// was never replaced), so it is marked lost (outage) again and its proxy
// stopped, or torn down when it cannot be kept.
func (s *Server) reloseRun(ctx context.Context, run types.AgentRun) {
	run.LostAt, run.LostReason = nil, ""
	loser, ok := s.cfg.Store.(store.RunLoser)
	if ok && s.loseRun(ctx, loser, run, types.LostOutage, types.RunFailed, 0) {
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
			out, rerr := s.reviveFromRequest(r, run)
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
// unparseable release ("" before migration 0072) is outside.
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
