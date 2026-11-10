// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A live egress edit (PATCH /runs/{id}/egress) narrows a running run's egress at once: it removes
// allowlist entries and adds denies, and never adds a host, so widening a live run stays an approval.
// It is applied by the live revive path (run_revive.go): a new proxy built from the run's stored
// config, with the edit applied before the final seal, under a fresh run token. Each removed or
// denied host goes through the owner-ceiling re-assertion as a profile deny does, so the credential
// lanes to it are dropped, or the edit is refused where a lane cannot go (the git broker, the Azure
// CLI and kubectl doors). A removed host is denied as well, so a later first-use approval in this run
// cannot reopen it; widening the run again is a new run or an approval of another host.
//
// The answer waits for the new proxy's own readback (run_proxy_readback.go) of the start the edit
// recorded: 200 only once that start is enforced. Any other end (no report by the start's deadline,
// a hash that differs or is missing, a start another revive superseded, or one that ran unconfirmed)
// leaves the run with no proxy and answers 503 egress_edit_unconfirmed. A report under the pre-edit
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

// egressEdit is a validated edit: lower-case entries, each list without duplicates.
type egressEdit struct {
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
func (req egressEditRequest) validate() (egressEdit, string) {
	if len(req.RemoveHosts) == 0 && len(req.DenyHosts) == 0 {
		return egressEdit{}, "the edit names no host: give remove_hosts, deny_hosts or both"
	}
	remove, msg := egressEditHosts("remove_hosts", req.RemoveHosts)
	if msg != "" {
		return egressEdit{}, msg
	}
	deny, msg := egressEditHosts("deny_hosts", req.DenyHosts)
	if msg != "" {
		return egressEdit{}, msg
	}
	return egressEdit{remove: remove, deny: deny}, ""
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

// applyEgressEdits applies a revive's edits to the config it starts from, before the final seal: each
// removed entry must be on the run's allowlist and leaves it, and every removed and denied host joins
// the owner ceiling's denies, so the re-assertion denies it and drops its credential lanes. The seal
// after this only intersects the allowlist and merges denies, so nothing brings a removed host back.
func applyEgressEdits(c *ownerCeiling, policy *types.RunPolicySpec, edits []egressEdit) *reviveError {
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
	}
	return nil
}

// editRunEgress refuses an edit the run cannot take before anything changes, holds the edit's mark,
// and replaces the run's proxy through the live revive path with the edit applied. Every outcome
// past the body check writes one run.egress.edit row.
func (s *Server) editRunEgress(r *http.Request, run types.AgentRun, edit egressEdit) (egressEditResult, *reviveError) {
	actorType, actor := actorFromRequest(r)
	res, rerr := s.editRunEgressOnce(r, run, edit)
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

func (s *Server) editRunEgressOnce(r *http.Request, run types.AgentRun, edit egressEdit) (egressEditResult, *reviveError) {
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
	if rerr != nil {
		return egressEditResult{}, egressEditRefusal(rerr)
	}
	// The wait outlives a caller who stops waiting: its end decides whether the proxy keeps running.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), egressEditWait)
	defer cancel()
	if why := s.awaitEgressEditReadback(ctx, run.ID, res.generation); why != "" {
		return egressEditResult{}, egressEditUnconfirmed(why)
	}
	return egressEditResult{PolicyGeneration: res.generation}, nil
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

// awaitEgressEditReadback waits for the run's start under gen to be enforced, and answers "" then.
// Otherwise it answers why not, and the run's proxy is stopped: a start the readback failed was
// stopped by it, and one past the database's deadline is failed and stopped by expireProxyStart (a
// no-op before that deadline). Any other end is stopped here: a start that ran unconfirmed or that
// another start superseded, or one still pending when the wait runs out (a store that did not answer).
func (s *Server) awaitEgressEditReadback(ctx context.Context, runID uuid.UUID, gen int) string {
	why := "readback_wait_timeout"
	st, ok := s.cfg.Store.(store.RunProxyStarts)
	if !ok || gen < 1 { // egressEditSupported refuses this before any change; never confirmed by default
		why = "readback_unavailable"
	}
	for ok && gen >= 1 && ctx.Err() == nil {
		p, found, err := st.ActiveProxyStart(ctx, runID)
		switch {
		case err != nil: // read again until the wait runs out
		case !found || p.Generation != gen:
			why = "start_superseded"
		case p.State == types.ProxyStartEnforced:
			return ""
		case p.State == types.ProxyStartFailed:
			return "start_failed"
		case p.State != types.ProxyStartPending:
			why = "start_" + string(p.State)
		default:
			s.expireProxyStart(ctx, runID, gen)
		}
		if why != "readback_wait_timeout" {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(egressEditPoll):
		}
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	s.stopUnconfirmedProxy(stopCtx, runID)
	return why
}

// egressEditUnconfirmed is the 503 of an edit whose new proxy did not confirm it.
func egressEditUnconfirmed(why string) *reviveError {
	return &reviveError{status: http.StatusServiceUnavailable, reason: reasonEgressEditUnconfirmed, lost: true,
		msg: "the run's new proxy did not confirm the edited policy (" + why + "), so it was stopped and the run has no egress until revived"}
}

// egressEditRefusal is a revive refusal as an edit answers it: a new proxy that did not confirm its
// policy is egress_edit_unconfirmed, and where the revive's own words name only the owner's profile
// they also name the edit, whose denies are checked as that profile's are.
func egressEditRefusal(rerr *reviveError) *reviveError {
	if rerr.reason == reasonRevivePolicyUnconfirmedLost {
		return egressEditUnconfirmed("readback failed")
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
