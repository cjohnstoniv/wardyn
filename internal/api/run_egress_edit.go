// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
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

	"github.com/cjohnstoniv/wardyn/internal/db"
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
// config is stored in the edit's own policy generation write, before the replace, so every
// revive that reads that generation starts from it (advanceRunPolicyGeneration). The answer waits for the new proxy's own readback (run_proxy_readback.go) of the start the
// edit recorded: 200 only once that start is enforced. Otherwise the answer names what is known:
// egress_edit_unconfirmed once the new proxy's stop is confirmed (its containment recorded on the
// start, or confirmed by the edit's own stop), egress_edit_containment_unresolved while it is not, and
// egress_edit_superseded when another revive replaced the start. A config that could not be stored
// takes no generation and no start, and the run keeps its current proxy. A report under the pre-edit
// proxy's token is stale for that start, so it can never confirm the edit. An edit is never a legacy
// start: a run whose runner or proxy image predates the readback is refused before anything changes.

// egressEditPoll is how often an edit reads its start while it waits for the readback, and
// egressEditWait bounds that wait past the start's own deadline. Variables so a test can shorten them.
var (
	egressEditPoll = 500 * time.Millisecond
	egressEditWait = proxyReportGrace + 15*time.Second
)

// A live egress edit has three ends, all counted from its first check (editCtx). Its request-bound calls end by
// egressEditCallBudget, so the revive's claim lands by then. The readback wait ends by egressEditBudget, the
// edit's deadline. Every window the edit opens past its request, and the tail of every run's end made under it,
// ends by that deadline plus killCascadeTimeout, the edit's hard end. The census pin
// (TestEgressEditBudget_CoversEveryWindowAfterTheClaim) checks that a revive's own windows fit before the call
// budget, and that every window after a claim made by then closes before the hard end. Past the hard end only
// calls that do not honour a done ctx remain. The longest chain of them is lock checks and releases, Docker's
// sandbox stop, and the audit sinks' 2 s read for each of the two stop-failure rows: 458 s, stated as 8 minutes
// (docs/operations/run-lifetime.md). A kept run's store of a renewed Azure sign-in (heldStoreCallBound) makes a
// shorter chain. Variables so a test can shorten them.
var (
	egressEditCallBudget = 230 * time.Second
	egressEditBudget     = 350 * time.Second
)

// editDeadlineKey carries a live egress edit's deadline on its ctx as a value: a call that must outlive the
// request drops the request's cancellation, and its deadline with it (editDetached).
type editDeadlineKey struct{}

// withEditDeadline is ctx under a live egress edit whose deadline is dl.
func withEditDeadline(ctx context.Context, dl time.Time) context.Context {
	return context.WithValue(ctx, editDeadlineKey{}, dl)
}

// editDeadline is the deadline of the live egress edit ctx is under, if it is under one.
func editDeadline(ctx context.Context) (time.Time, bool) {
	dl, ok := ctx.Value(editDeadlineKey{}).(time.Time)
	return dl, ok
}

// editCtx is the ctx a live egress edit runs on, from now: ctx's values and cancellation, ending
// egressEditCallBudget from now, and carrying the edit's deadline (egressEditBudget from now) and its hard end,
// that deadline plus killCascadeTimeout. The hard end rides as an end hold's deadline (withHoldDeadline), so the
// tail of any run's end made under the edit stops there, whether that end took a hold of its own or none (a run
// with no computer folder takes none, lockRunEnd): its fresh windows end there (teardownCtxWithin,
// settleAfterTeardown), past it the sandbox is not stopped (finalizeRunEffects), and the tail owner finishes the
// run. A hold taken under the edit nests in it (holdRunEnd): it takes no end-hold slot, since every end on the
// edit's paths re-enters the run's lock the edit's caller already holds, and so takes no lock connection.
func editCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	now := time.Now()
	dl := now.Add(egressEditBudget)
	ctx = withHoldDeadline(withEditDeadline(ctx, dl), dl.Add(killCascadeTimeout))
	return context.WithDeadline(ctx, now.Add(egressEditCallBudget))
}

// editDetached is a window of d for a call an edit's answer waits on that must outlive the request: ctx's
// values with none of its cancellation but a lost lock's (db.WithoutCancelUnlessLost), ending d from now.
// Under an edit it ends by the edit's hard end when that comes first, and keeps the edit's end hold, so a
// stop begun in it, nested or not, lets its tail run to the hard end, not to this window's end (editCtx).
// Off an edit it is the plain window.
func editDetached(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	fenced, release := db.WithoutCancelUnlessLost(ctx)
	end := time.Now().Add(d)
	if dl, ok := editDeadline(ctx); ok {
		end = minTime(end, dl.Add(killCascadeTimeout))
	}
	out, cancel := context.WithDeadline(fenced, end)
	return out, func() { cancel(); release() }
}

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
	PolicyGeneration int                `json:"policy_generation"`
	inPlace          *egressEditInPlace // set when the edit narrowed the proxy in place; never serialised
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
	// The edit's ends (editCtx). Its request-bound calls end by the first, as when its caller stops waiting, and
	// the deadline and the hard end ride on the ctx to every window and run's end the answer waits on. The audit
	// row stays on the request.
	ectx, cancel := editCtx(r.Context())
	defer cancel()
	res, rerr := s.editRunEgressOnce(r.WithContext(ectx), run, edit)
	fctx, cancelFate := editDetached(ectx, killCascadeTimeout)
	defer cancelFate()
	if res.inPlace != nil {
		rerr = s.withNarrowFate(fctx, run, rerr)
	} else {
		rerr = s.withRunFate(fctx, run, rerr)
	}
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
		if res.inPlace != nil {
			data["applied"] = "in_place"
			if res.inPlace.proxyWasBehind {
				data["proxy_was_behind"] = true
			}
		}
	}
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor, "run.egress.edit", run.ID.String(), outcome, mustJSON(data)))
	return res, rerr
}

func (s *Server) editRunEgressOnce(r *http.Request, run types.AgentRun, edit liveEgressEdit) (out egressEditResult, rerr *reviveError) {
	if run.State != types.RunRunning || run.SandboxRef == "" || run.LostAt != nil {
		return egressEditResult{}, reviveRefused(http.StatusConflict, reasonEgressEditRunNotLive,
			"the run is not live (state="+string(run.State)+"), so its egress cannot be edited; revive it first")
	}
	if noNetworkPreset(run.WallPreset) {
		return egressEditResult{}, reviveRefused(http.StatusConflict, reasonEgressEditNoNetwork,
			"This run has no network, so an edit has nothing to change. Start a new run with the policy you want.")
	}
	if rerr := s.egressEditSupported(r.Context(), run); rerr != nil {
		return egressEditResult{}, rerr
	}
	if _, busy := s.reviving.LoadOrStore(egressEditMark(run.ID), struct{}{}); busy {
		return egressEditResult{}, reviveRefused(http.StatusConflict, reasonEgressEditInProgress,
			"an egress edit of this run is already in progress; try again when it answers")
	}
	defer s.reviving.Delete(egressEditMark(run.ID))
	res, rerr, inPlace := s.narrowRunEgress(r, run, edit)
	defer func() { out.inPlace = inPlace }()
	if inPlace == nil {
		res, rerr = s.reviveFromRequest(r, run, false, edit)
	}
	// A revive that put the run back to lost words its own stop from a lost mark that may only have
	// logged a failed stop; an edit answers from the stop's confirmation instead.
	lost := rerr != nil && (rerr.lost || rerr.reason == reasonReviveRecoveryUnresolved)
	switch {
	case rerr != nil && !lost:
		return egressEditResult{}, egressEditRefusal(rerr, s.egressEditRunnerToUpdate(run))
	case lost && res.generation < 1: // lost before a start was recorded: no start to fence a stop on
		return egressEditResult{}, egressEditUnresolved(rerr.reason, nil)
	}
	// The wait outlives a caller who stops waiting: its end decides whether the proxy keeps running. It ends by
	// the edit's deadline, so a stop it leads to keeps its whole window, and that stop's tail runs to the hard
	// end (editDetached).
	ctx, cancel := editDetached(r.Context(), egressEditWait)
	defer cancel()
	if dl, ok := editDeadline(ctx); ok {
		var endWait context.CancelFunc
		ctx, endWait = context.WithDeadline(ctx, dl)
		defer endWait()
	}
	if inPlace != nil {
		ctx = withNarrowedStart(ctx)
	}
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
	case s.lostRunKeepable(ctx, run):
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
// the hash of the bytes it hands the proxy, or a runner or proxy image known to predate firstRenewGateRelease (olderThanFirstRenewGate's positive evidence, which covers the readback's own floor).
func (s *Server) egressEditSupported(ctx context.Context, run types.AgentRun) *reviveError {
	_, gens := s.cfg.Store.(store.RunPolicyGenerations)
	_, starts := s.cfg.Store.(store.RunProxyStarts)
	_, hashed := s.cfg.Runner.(runner.ProxyReviverHashed)
	if !gens || !starts || !hashed {
		return reviveRefused(http.StatusConflict, reasonEgressEditUnsupported,
			"this deployment cannot confirm the policy a new proxy loaded, so a live egress edit is refused and nothing changed; start a new run with the narrower policy")
	}
	if s.olderThanFirstRenewGate(ctx, run) {
		return reviveRefused(http.StatusConflict, reasonEgressEditLegacyProxy,
			"this run's runner or proxy image is older than "+firstRenewGateRelease+", so an edit could not be confirmed safely and nothing changed; update the runner and the proxy image to "+firstRenewGateRelease+" or later, or start a new run with the narrower policy")
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

// stopCtx is a fresh bound for a stop the edit waits on, or a read after one: the readback wait may have run
// out. Under an edit it ends by the edit's hard end (editDetached).
func stopCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return editDetached(ctx, killCascadeTimeout)
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
	err = s.stopUnconfirmedProxyAt(lctx, run, gen)
	for try := 1; try < egressEditEndTries && errors.Is(err, errRunEndUnwritten) && lctx.Err() == nil; try++ {
		select {
		case <-lctx.Done():
		case <-time.After(egressEditPoll):
		}
		if cur, gerr := s.cfg.Store.GetRun(lctx, runID); gerr == nil && s.stopUnconfirmedProxyAt(lctx, cur, gen) == nil {
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

// storeAndSettleRevived settles the new proxy's start (settleRevivedProxy) and answers whether the revive's
// config is the run's stored one. With policy generations (gen > 0) it already is: every revive, an ordinary
// one or an edit, stored it in its generation's own write, before the replace (advanceRunPolicyGeneration),
// so nothing is stored here and no write after the replace can overwrite a newer revive's config. A store
// without generations has nothing to fence that write on, and stores it after the settle, best-effort, as
// before: a failed write keeps the previous config, from which the next revive re-derives the same result
// (an edit is refused on such a store, egressEditSupported).
func (s *Server) storeAndSettleRevived(ctx context.Context, run types.AgentRun, data map[string]any, actorType types.ActorType, actor string,
	gen int, start proxyStartRec, proxyHash, retiring string, renewed renewedTokens, freshJTI string, storedJSON []byte) (stored bool, rerr *reviveError) {
	if rerr := s.settleRevivedProxy(ctx, run, data, actorType, actor, start, proxyHash, retiring, renewed, freshJTI); rerr != nil {
		return gen > 0, rerr
	}
	if gen > 0 {
		return true, nil
	}
	if err := s.storeRunProxyConfig(ctx, run.ID, storedJSON); err != nil {
		slog.WarnContext(ctx, "wardynd: storing a revived run's proxy config failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
	return false, nil
}

// egressEditUnconfirmed is the 503 of an edit whose new proxy did not confirm it, once that proxy's
// stop is confirmed.
func egressEditUnconfirmed(why string) *reviveError {
	return &reviveError{status: http.StatusServiceUnavailable, reason: reasonEgressEditUnconfirmed, lost: true,
		msg: "the run's new proxy did not confirm the edited policy (" + why + "), so it was stopped and the run has no egress"}
}

// egressEditSuperseded is the 503 of an edit whose start another revive replaced before it confirmed.
// persisted says this request stored the edited config, as every edit that recorded a start did, in
// its generation's own write: that revive then started from it, and its own readback settles it.
// Otherwise the edit was never applied, and that revive started from the config stored before it.
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

// egressEditUnsupportedMsg is the 409 egress_edit_unsupported of an edit whose substrate cannot replace the run's proxy or confirm a new one's policy: what to do, not the runner's error.
const egressEditUnsupportedMsg = "This run's network can't be changed while it runs on this substrate. Start a new run with the narrower policy."

// egressEditUpdateRunnerMsg is egressEditUnsupportedMsg for a runner that must be updated first (egressEditRunnerToUpdate).
const egressEditUpdateRunnerMsg = "This run's network can't be changed while it runs: the runner on %s must be updated first. Update it, or start a new run with the narrower policy."

// egressEditRunnerToUpdate names the computer of a container run on a runner whose last caps say it
// can keep a sandbox (not Kubernetes). Such a runner can replace a container's proxy, so an unsupported
// answer from it means the runner is older; "" for any other run. It picks the text only, never a refusal.
func (s *Server) egressEditRunnerToUpdate(run types.AgentRun) string {
	if run.RunnerID == nil || run.MachineKind.IsVM() {
		return ""
	}
	caps, ok := s.runnerLastCaps(*run.RunnerID)
	if !ok || caps.EndsUnkept {
		return ""
	}
	return cmp.Or(caps.MachineName, "its computer")
}

// egressEditRefusal is a revive refusal as an edit answers it: a new proxy that did not confirm its
// policy is egress_edit_unconfirmed, a substrate that cannot replace a sandbox's proxy at all
// (Kubernetes) or cannot confirm a new proxy's policy (revive_readback_unsupported) is
// egress_edit_unsupported, not the revive's own reason, and where the revive's own
// words name only the owner's profile they also name the edit, whose denies are checked as that
// profile's are.
func egressEditRefusal(rerr *reviveError, updateOn string) *reviveError {
	if rerr.reason == reasonReviveUnsupported || rerr.reason == reasonReviveReadbackUnsupported {
		out := *rerr
		out.reason = reasonEgressEditUnsupported
		out.msg = egressEditUnsupportedMsg
		if updateOn != "" {
			out.msg = fmt.Sprintf(egressEditUpdateRunnerMsg, updateOn)
		}
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
