// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A live egress edit (PATCH /runs/{id}/egress) narrows a running run's egress at once: it removes
// allowlist entries and adds denies, and never adds a host, so widening a live run stays an approval.
// It is applied by the live revive path (run_revive.go): a new proxy built from the run's stored
// config under a fresh run token, with the edit applied after the final seal, at the owner-ceiling
// re-assertion. Each removed or denied host is withheld there as a profile deny is, so the credential
// lanes to it are dropped, or the edit is refused where a lane cannot go (the git broker, the Azure
// CLI and kubectl doors). A removed host is denied as well, so a later first-use approval in this run
// cannot reopen it; widening the run again is a new run or an approval of another host.
//
// Every 4xx refusal comes before the revive replaces anything. Past it, the edited
// config is stored before the new proxy's start is settled, so every later revive starts
// from it. The answer waits for the new proxy's own readback (run_proxy_readback.go) of the start the
// edit recorded: 200 only once that start is enforced. Otherwise the answer names what is known:
// egress_edit_unconfirmed once the new proxy's stop is confirmed (its containment recorded on the
// start, or confirmed by the edit's own stop), egress_edit_containment_unresolved while it is not, and
// egress_edit_superseded when another revive replaced the start. A config that could not be stored
// fails the start, which the readback contains like any failed start. A report under the pre-edit
// proxy's token is stale for that start, so it can never confirm the edit. An edit is never a legacy
// start: a run whose runner or proxy image predates the readback is refused before anything changes.

// egressEditPoll is how often an edit reads its start while it waits for the readback, and
// egressEditWait bounds that wait past the start's own deadline. Variables so a test can shorten them.
var (
	egressEditPoll = 500 * time.Millisecond
	egressEditWait = proxyReportGrace + 15*time.Second
)

// maxEgressEditHosts bounds each list of one edit, as allowed_domains is bounded.
const maxEgressEditHosts = types.MaxAllowedDomainsPerSpec

// egressEditRequest is PATCH /runs/{id}/egress's body. It has no field that adds a host: an unknown
// field, add_hosts or allowed_domains among them, is a 400 (decodeStrict).
type egressEditRequest struct {
	RemoveHosts []string `json:"remove_hosts"`
	DenyHosts   []string `json:"deny_hosts"`
}

// egressEditResult is the 200 answer: the policy generation the confirmed proxy enforces.
type egressEditResult struct {
	PolicyGeneration int `json:"policy_generation"`
}

// liveEgressEdit is a validated edit: lower-case entries, each list without duplicates.
type liveEgressEdit struct {
	remove []string
	deny   []string
}

// egressEditMark is the key an edit holds in s.reviving for its whole request, readback included, so
// a second edit of the run is refused (egress_edit_in_progress). Its own type keeps it apart from the
// revive's run-id key in the same map.
type egressEditMark uuid.UUID

// mountRunReviveRoutes registers a run's revive and its live egress edit: both owner or super admin,
// and both replace the run's proxy. They open no shell, so #1476 leaves them.
func (s *Server) mountRunReviveRoutes(r chi.Router) {
	r.Post("/runs/{id}/revive", s.handleReviveRun)
	r.Patch("/runs/{id}/egress", s.handleEditRunEgress)
}

// handleEditRunEgress is PATCH /api/v1/runs/{id}/egress: the owner (or a super admin, acting with the
// owner's authority, as for revive) narrows a live run's egress.
func (s *Server) handleEditRunEgress(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrSuperAdmin)
	if !ok {
		return
	}
	var req egressEditRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	edit, msg := req.validate()
	if msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonEgressEditInvalid, msg)
		return
	}
	res, rerr := s.editRunEgress(r, run, edit)
	if rerr != nil {
		writeErrorReason(w, rerr.status, rerr.reason, rerr.msg)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// validate checks each entry as a policy's own domain entries are checked, so the edit never carries
// one the proxy cannot match.
func (req egressEditRequest) validate() (liveEgressEdit, string) {
	if len(req.RemoveHosts) == 0 && len(req.DenyHosts) == 0 {
		return liveEgressEdit{}, "the edit names no host: give remove_hosts, deny_hosts or both"
	}
	remove, msg := egressEditHosts("remove_hosts", req.RemoveHosts)
	if msg != "" {
		return liveEgressEdit{}, msg
	}
	deny, msg := egressEditHosts("deny_hosts", req.DenyHosts)
	if msg != "" {
		return liveEgressEdit{}, msg
	}
	return liveEgressEdit{remove: remove, deny: deny}, ""
}

func egressEditHosts(field string, hosts []string) ([]string, string) {
	if len(hosts) > maxEgressEditHosts {
		return nil, fmt.Sprintf("%s: at most %d entries", field, maxEgressEditHosts)
	}
	out := make([]string, 0, len(hosts))
	for i, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if err := proxy.ValidDomainEntry(h); err != nil {
			return nil, fmt.Sprintf("%s[%d]: %v", field, i, err)
		}
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out, ""
}

// applyEgressEdits applies a revive's edits to the sealed config, first in reassertProxyCeiling: each
// removed entry must be on the run's sealed allowlist and leaves it, and every removed and denied host
// joins the owner ceiling's denies, so the re-assertion denies it and drops its credential lanes.
// Nothing after the re-assertion adds an allowlist entry (the seal census), so no removed host returns.
// A via_org route on an edited host is dropped too: the sidecar forwards a via_org destination to the
// organisation before its own host check, so only an absent route refuses it there, and the
// organisation's engine, rebuilt from this config, then has no route for the grant either.
func applyEgressEdits(c *ownerCeiling, cfg *proxy.Config, edits []liveEgressEdit) *reviveError {
	policy := &cfg.Policy
	for _, e := range edits {
		for _, h := range e.remove {
			if !slices.ContainsFunc(policy.AllowedDomains, func(d string) bool { return strings.EqualFold(strings.TrimSpace(d), h) }) {
				return reviveRefused(http.StatusUnprocessableEntity, reasonEgressEditHostNotListed,
					fmt.Sprintf("%s is not an entry on this run's allowlist, so there is nothing to remove; to block it, deny it instead", h))
			}
		}
		policy.AllowedDomains = slices.DeleteFunc(policy.AllowedDomains, func(d string) bool {
			return slices.Contains(e.remove, strings.ToLower(strings.TrimSpace(d)))
		})
		c.deny = slices.Concat(c.deny, e.remove, e.deny)
		cfg.ViaOrgRoutes = slices.DeleteFunc(cfg.ViaOrgRoutes, func(rt proxy.ViaOrgRoute) bool {
			return editedHost(rt.Host, slices.Concat(e.remove, e.deny))
		})
	}
	return nil
}

// editedHost reports whether host falls under any edited entry, whatever the entry's port: a via_org
// route is the host's on every port, so a port-scoped deny drops the whole route.
func editedHost(host string, entries []string) bool {
	host = domainmatch.CanonHost(host)
	return slices.ContainsFunc(entries, func(d string) bool {
		exact, wild, _ := domainmatch.Classify(d)
		return (exact != "" && exact == host) || (wild != "" && domainmatch.MatchWild(host, []string{wild}))
	})
}

// retireViaOrgEngine retires this replica's cached via_org engine for the run, cancelling its open
// streams, once the edited config is stored: the next via_org request rebuilds the engine from that
// config. Another replica's engine is reached only through the run's proxy, and the new proxy no
// longer routes an edited host to the organisation.
func (s *Server) retireViaOrgEngine(runID uuid.UUID) {
	s.viaOrg.mu.Lock()
	run := s.viaOrg.runs[runID]
	s.viaOrg.mu.Unlock()
	if run != nil {
		s.releaseViaOrgRun(run, false, false)
	}
}

// editRunEgress refuses an edit the run cannot take before anything changes, holds the edit's mark,
// and replaces the run's proxy through the live revive path with the edit applied. Every outcome
// past the body check writes one run.egress.edit row.
func (s *Server) editRunEgress(r *http.Request, run types.AgentRun, edit liveEgressEdit) (egressEditResult, *reviveError) {
	actorType, actor := actorFromRequest(r)
	res, rerr := s.editRunEgressOnce(r, run, edit)
	rerr = s.withRunFate(r.Context(), run, rerr)
	data := map[string]any{"subject": run.CreatedBy, "removed_hosts": nonNilStrings(edit.remove), "denied_hosts": nonNilStrings(edit.deny)}
	outcome := "success"
	if rerr != nil {
		outcome = "denied"
		if rerr.status >= http.StatusInternalServerError {
			outcome = "failure"
		}
		data["reason"] = rerr.reason
	} else {
		data["policy_generation"] = res.PolicyGeneration
	}
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor, "run.egress.edit", run.ID.String(), outcome, mustJSON(data)))
	return res, rerr
}

func (s *Server) editRunEgressOnce(r *http.Request, run types.AgentRun, edit liveEgressEdit) (egressEditResult, *reviveError) {
	if run.State != types.RunRunning || run.SandboxRef == "" || run.LostAt != nil {
		return egressEditResult{}, reviveRefused(http.StatusConflict, reasonEgressEditRunNotLive,
			"the run is not live (state="+string(run.State)+"), so its egress cannot be edited; revive it first")
	}
	if rerr := s.egressEditSupported(r.Context(), run); rerr != nil {
		return egressEditResult{}, rerr
	}
	if _, busy := s.reviving.LoadOrStore(egressEditMark(run.ID), struct{}{}); busy {
		return egressEditResult{}, reviveRefused(http.StatusConflict, reasonEgressEditInProgress,
			"an egress edit of this run is already in progress; try again when it answers")
	}
	defer s.reviving.Delete(egressEditMark(run.ID))
	res, rerr := s.reviveFromRequest(r, run, false, edit)
	// A revive that put the run back to lost words its own stop from a lost mark that may only have
	// logged a failed stop; an edit answers from the stop's confirmation instead.
	lost := rerr != nil && (rerr.lost || rerr.reason == reasonReviveRecoveryUnresolved)
	switch {
	case rerr != nil && !lost:
		return egressEditResult{}, egressEditRefusal(rerr)
	case lost && res.generation < 1: // lost before a start was recorded: no start to fence a stop on
		return egressEditResult{}, egressEditUnresolved(rerr.reason, nil)
	}
	// The wait outlives a caller who stops waiting: its end decides whether the proxy keeps running.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), egressEditWait)
	defer cancel()
	// A revive that failed after its start was recorded is answered by that start: a failed one once
	// its containment is recorded, an abandoned one (the replace failed) once its stop is confirmed.
	wait := s.awaitEgressEditReadback(ctx, run.ID, res.generation, rerr == nil || res.stored)
	switch {
	case wait != nil:
		return egressEditResult{}, wait
	case rerr != nil: // never enforced after the revive failed it; read as unresolved, never as stopped
		return egressEditResult{}, egressEditUnresolved("readback failed", nil)
	}
	return egressEditResult{PolicyGeneration: res.generation}, nil
}

// withRunFate ends an edit's stopped or stopping answer with the run's fate as the store now holds it
// (storedRunFate): a confirmed stop is not a recorded end or lost mark. An unresolved containment that
// recorded neither says what will happen once its stop is done: a run that can be kept is lost until
// revived, and one that cannot (a headless run off the person's own machine) is ended
// (lostRunKeepable, the predicate the stop itself follows). An unconfirmed edit's answer then names
// the likely cause (unconfirmedImageCause), as a run's failure hint does.
func (s *Server) withRunFate(ctx context.Context, run types.AgentRun, rerr *reviveError) *reviveError {
	if rerr == nil || (rerr.reason != reasonEgressEditUnconfirmed && rerr.reason != reasonEgressEditContainmentUnresolved) {
		return rerr
	}
	out := *rerr
	switch fate, read := s.readRunFate(ctx, run.ID); {
	case fate != "":
		out.msg += fate
	case !read, rerr.reason != reasonEgressEditContainmentUnresolved: // an unread run is promised nothing
	case s.lostRunKeepable(run, s.cfg.Now()):
		out.msg += "; once that is done, the run will be lost until revived"
	default:
		out.msg += "; once that is done, the run will be ended, since a headless run off your own machine cannot be kept"
	}
	if rerr.reason == reasonEgressEditUnconfirmed {
		out.msg += unconfirmedImageCause
	}
	return &out
}

// egressEditSupported refuses, before anything changes, an edit whose new proxy could not confirm
// what it loaded: a store that keeps no generations or proxy starts, a substrate that cannot answer
// the hash of the bytes it hands the proxy, or a legacy start (proxyStartMode's positive evidence).
func (s *Server) egressEditSupported(ctx context.Context, run types.AgentRun) *reviveError {
	_, gens := s.cfg.Store.(store.RunPolicyGenerations)
	_, starts := s.cfg.Store.(store.RunProxyStarts)
	_, hashed := s.cfg.Runner.(runner.ProxyReviverHashed)
	if !gens || !starts || !hashed {
		return reviveRefused(http.StatusConflict, reasonEgressEditUnsupported,
			"this deployment cannot confirm the policy a new proxy loaded, so a live egress edit is refused and nothing changed; start a new run with the narrower policy")
	}
	if s.proxyStartMode(ctx, run) == store.ProxyStartLegacy {
		return reviveRefused(http.StatusConflict, reasonEgressEditLegacyProxy,
			"this run's runner or proxy image predates the policy readback, so an edit could not be confirmed and nothing changed; start a new run with the narrower policy")
	}
	return nil
}

// awaitEgressEditReadback waits for the run's start under gen to be enforced, and answers nil then.
// Every other end answers why, and claims the proxy stopped only once ContainedAt is recorded for gen.
// A failed start is contained under the run's lock, fenced on gen (containProxyStart): by its report's
// handler, by expireProxyStart (which this wait runs past the database's deadline), or by this wait,
// which retries while the lock is busy (errRunOpBusy: not yet a stop either way). A start another
// revive superseded is that start's to settle and stops nothing here. A start still unresolved when
// the wait runs out is the reconcile sweep's to fail and contain.
func (s *Server) awaitEgressEditReadback(ctx context.Context, runID uuid.UUID, gen int, persisted bool) *reviveError {
	st, _ := s.cfg.Store.(store.RunProxyStarts) // egressEditSupported refused a store without starts
	why := "readback_wait_timeout"
	for ctx.Err() == nil {
		p, found, err := st.ActiveProxyStart(ctx, runID)
		switch {
		case err != nil: // read again until the wait runs out
		case !found || p.Generation < gen: // gen was abandoned (its replace failed), and no newer start began
			return s.stopEditStart(ctx, runID, gen, persisted, "start_abandoned")
		case p.Generation > gen || p.State == types.ProxyStartSuperseded:
			return egressEditSuperseded(persisted)
		case p.State == types.ProxyStartEnforced:
			return nil
		case p.State == types.ProxyStartFailed && p.ContainedAt != nil:
			return egressEditUnconfirmed("start_failed")
		case p.State == types.ProxyStartFailed && p.ContainmentError != "":
			return egressEditUnresolved("start_failed", errors.New(p.ContainmentError))
		case p.State == types.ProxyStartFailed:
			why = "start_failed"
			egressEditSawUncontained()
			if rerr, done := s.containEditStart(ctx, runID, gen, persisted); done {
				return rerr
			}
		case p.State == types.ProxyStartUnconfirmed: // a legacy start, if the runner changed since egressEditSupported
			return s.stopEditStart(ctx, runID, gen, persisted, "start_unconfirmed")
		default:
			s.expireProxyStart(ctx, runID, gen) // a no-op before the deadline
		}
		select {
		case <-ctx.Done():
		case <-time.After(egressEditPoll):
		}
	}
	return egressEditUnresolved(why, ctx.Err())
}

// egressEditSawUncontained runs when an edit reads its start failed with no containment recorded yet.
// A test hook.
var egressEditSawUncontained = func() {}

// stopCtx is a fresh bound for a stop the edit waits on: the readback wait may have run out.
func stopCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
}

// containEditStart contains the failed start gen and answers by the result. done is false while the
// run's lock is busy, which is no stop either way: the caller polls again within its deadline.
func (s *Server) containEditStart(ctx context.Context, runID uuid.UUID, gen int, persisted bool) (rerr *reviveError, done bool) {
	ctx, cancel := stopCtx(ctx)
	defer cancel()
	switch c := s.containProxyStart(ctx, runID, gen); {
	case c.Contained:
		return egressEditUnconfirmed("start_failed"), true
	case c.Superseded:
		return egressEditSuperseded(persisted), true
	case errors.Is(c.Err, errRunOpBusy):
		return nil, false
	default:
		return egressEditUnresolved("start_failed", c.Err), true
	}
}

// stopEditStart stops the proxy of an edit's start that no readback contains (one that ran
// unconfirmed, or one abandoned when its replace failed), under the run's lock, which a revive holds
// from its claim to its settle, and only while no start newer than gen began. It claims the stop
// only once stopUnconfirmedProxy confirms it.
func (s *Server) stopEditStart(ctx context.Context, runID uuid.UUID, gen int, persisted bool, why string) *reviveError {
	ctx, cancel := stopCtx(ctx)
	defer cancel()
	lctx, unlock, err := s.lockRunOp(ctx, runID)
	if err != nil {
		return egressEditUnresolved(why, err)
	}
	defer unlock()
	st, _ := s.cfg.Store.(store.RunProxyStarts) // egressEditSupported refused a store without starts
	switch p, found, err := st.ActiveProxyStart(lctx, runID); {
	case err != nil:
		return egressEditUnresolved(why, err)
	case found && p.Generation > gen:
		return egressEditSuperseded(persisted)
	}
	run, err := s.cfg.Store.GetRun(lctx, runID)
	if err != nil {
		return egressEditUnresolved(why, err)
	}
	// An end whose write failed (errRunEndUnwritten) after a confirmed proxy stop: an abandoned start
	// is in no start sweep, so the edit retries it (both stops are idempotent), and only a retry that
	// lands replaces the known stop. One that never lands is the lapsed-token sweep's
	// (sweepLapsedRunTokens): with the proxy stopped, nothing renews the run's token.
	err = s.stopUnconfirmedProxy(lctx, run)
	for try := 1; try < egressEditEndTries && errors.Is(err, errRunEndUnwritten) && lctx.Err() == nil; try++ {
		select {
		case <-lctx.Done():
		case <-time.After(egressEditPoll):
		}
		if cur, gerr := s.cfg.Store.GetRun(lctx, runID); gerr == nil && s.stopUnconfirmedProxy(lctx, cur) == nil {
			err = nil
		}
	}
	switch {
	case errors.Is(err, errRunEndUnwritten):
		out := egressEditUnconfirmed(why)
		out.msg += "; its end could not be recorded yet, so the lapsed-token sweep ends the run once its token lapses, " +
			strconv.Itoa(int(runTokenLapseAfter/time.Minute)) + " minutes after its last renewal"
		return out
	case err != nil:
		return egressEditUnresolved(why, err)
	}
	return egressEditUnconfirmed(why)
}

// egressEditEndTries bounds an edit's attempts at an end whose write failed.
const egressEditEndTries = 3

// storeAndSettleRevived stores a revive's config as the run's, the one every later revive starts from,
// and settles the new proxy's start (settleRevivedProxy). An ordinary revive stores after the settle,
// best-effort: a failed write keeps the previous config, from which the next revive re-derives the
// same result. An edit stores first, since its config only narrows: any revive after it, a revive
// that supersedes a failed settle included, starts from the edited config. An edit's write fails
// closed: a later revive would start from the pre-edit config and reopen the removed or denied hosts,
// so the new proxy's start is failed and contained, and the edit never answers 200.
func (s *Server) storeAndSettleRevived(ctx context.Context, run types.AgentRun, data map[string]any, actorType types.ActorType, actor string,
	start proxyStartRec, proxyHash, retiring string, renewed renewedTokens, freshJTI string, storedJSON []byte, edits []liveEgressEdit) (stored bool, rerr *reviveError) {
	if len(edits) == 0 {
		if rerr := s.settleRevivedProxy(ctx, run, data, actorType, actor, start, proxyHash, retiring, renewed, freshJTI); rerr != nil {
			return false, rerr
		}
		if err := s.storeRunProxyConfig(ctx, run.ID, storedJSON); err != nil {
			slog.WarnContext(ctx, "wardynd: storing a revived run's proxy config failed",
				slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
		return false, nil
	}
	storeErr := s.storeRunProxyConfig(ctx, run.ID, storedJSON)
	if storeErr != nil {
		// The hash is withheld, so the settle fails the start (hash missing) and contains it as the
		// readback contains any failed start: recorded on the start, and retried by its sweep.
		data["config_store_error"] = storeErr.Error()
		proxyHash = ""
		slog.WarnContext(ctx, "wardynd: storing an edited run's proxy config failed; failing its new proxy's start",
			slog.String("run_id", run.ID.String()), slog.Any("err", storeErr))
	} else {
		s.retireViaOrgEngine(run.ID) // no engine built from the pre-edit config serves again
	}
	rerr = s.settleRevivedProxy(ctx, run, data, actorType, actor, start, proxyHash, retiring, renewed, freshJTI)
	if rerr == nil && storeErr != nil { // a start with no hash always fails; never let an unstored edit stand
		return false, s.reviveFailedAfterClaim(ctx, run, data, actorType, actor, reasonRevivePolicyUnconfirmedLost,
			"the edited proxy config could not be stored, so the run's new proxy is being stopped", storeErr)
	}
	return storeErr == nil, rerr
}

// egressEditUnconfirmed is the 503 of an edit whose new proxy did not confirm it, once that proxy's
// stop is confirmed.
func egressEditUnconfirmed(why string) *reviveError {
	return &reviveError{status: http.StatusServiceUnavailable, reason: reasonEgressEditUnconfirmed, lost: true,
		msg: "the run's new proxy did not confirm the edited policy (" + why + "), so it was stopped and the run has no egress"}
}

// egressEditSuperseded is the 503 of an edit whose start another revive replaced before it confirmed.
// persisted says this request stored the edited config: that revive then started from it, and its
// own readback settles it. Otherwise (the edit's replace or config write failed) the edit was never
// applied, and that revive started from the config stored before it.
func egressEditSuperseded(persisted bool) *reviveError {
	msg := "another revive replaced the run's proxy before the edit took effect, so the edit was not applied; send it again"
	if persisted {
		msg = "another revive replaced the edit's proxy before it confirmed; the run's proxy now starts from the edited config, and its own readback confirms it"
	}
	return &reviveError{status: http.StatusServiceUnavailable, reason: reasonEgressEditSuperseded, msg: msg}
}

// egressEditUnresolved is the 503 of an edit whose new proxy did not confirm it and whose stop is not
// confirmed yet: the run is lost or being stopped, and the readback sweep or the lease sweep retries it.
func egressEditUnresolved(why string, err error) *reviveError {
	msg := "the run's new proxy did not confirm the edited policy (" + why + "); stopping it is not confirmed yet, and the run is being stopped"
	if err != nil {
		msg += ": " + err.Error()
	}
	return &reviveError{status: http.StatusServiceUnavailable, reason: reasonEgressEditContainmentUnresolved, lost: true, msg: msg}
}

// egressEditRefusal is a revive refusal as an edit answers it: a new proxy that did not confirm its
// policy is egress_edit_unconfirmed, a substrate that cannot replace a sandbox's proxy at all
// (Kubernetes) is egress_edit_unsupported, not the revive's own reason, and where the revive's own
// words name only the owner's profile they also name the edit, whose denies are checked as that
// profile's are.
func egressEditRefusal(rerr *reviveError) *reviveError {
	if rerr.reason == reasonReviveUnsupported {
		out := *rerr
		out.reason = reasonEgressEditUnsupported
		return &out
	}
	door := map[string]string{
		reasonReviveCeilingDeniesGitBroker:  "GitHub, which this run's git broker needs",
		reasonReviveCeilingDeniesAzureCLI:   "Azure Resource Manager, which this run's Azure CLI door needs",
		reasonReviveCeilingDeniesKubectlCLI: "the API server this run's kubectl door needs",
	}[rerr.reason]
	if door == "" {
		return rerr
	}
	out := *rerr
	out.msg = "the edit, or the owner's governance profile, denies " + door + ", so nothing changed; leave that host out of the edit"
	return &out
}
