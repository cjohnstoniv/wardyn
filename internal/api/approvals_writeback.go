// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// THE DURABLE WRITE-BACK: what a decided approval leaves behind on a WORKSPACE,
// as opposed to on the approval row itself. Split out of approvals.go for the
// 1000-line file-size gate (scripts/check-file-size.sh).
//
// They belong together because they are one question — an `always` decision and
// a verify-loop approval both reach past the approval and edit a workspace an
// operator may not have been looking at — and because they share one contract
// the rest of this package does not: FAIL SILENT BUT AUDITED. The decision
// itself already stands by the time these run, so none of them may fail the
// request; every give-up path therefore has to leave an audit row instead, or
// the operator gets a green UI and a workspace that learned nothing.
//
// internal/api/approvals_reconcile.go is this file's BOOT-TIME twin: it replays
// the same `always` verdicts after a restart, and it calls this file's two
// direction-specific reject predicates so the two cannot disagree about what a
// workspace will accept.
package api

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// primaryWorkspace is an `always` decision's target workspace: WorkspaceIDs[0]
// (the documented PRIMARY — referencedWorkspaces builds it in a stable order,
// mounts then repos, deduped, and it also drives image selection), else the
// trusted scan/verify/record WorkspaceID, else uuid.Nil when the run records no
// workspace link. Shared by resolveAlwaysTarget's live write-back and
// ReconcileWorkspaceEgressDecisions' boot heal so both resolve the same target.
func primaryWorkspace(run types.AgentRun) uuid.UUID {
	switch {
	case len(run.WorkspaceIDs) > 0:
		return run.WorkspaceIDs[0]
	case run.WorkspaceID != nil:
		return *run.WorkspaceID
	default:
		return uuid.Nil
	}
}

// approvalHost extracts an egress_domain approval's lowercased host from its
// RequestedScope JSON ("" when absent or malformed) — the same shape the proxy
// authors and approval.requestedScopeHost reads for the audit stream.
func approvalHost(ap types.ApprovalRequest) string {
	var scope struct {
		Host string `json:"host"`
	}
	if json.Unmarshal(ap.RequestedScope, &scope) != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(scope.Host))
}

// approveAlwaysRejects is the set of hosts an approve·always must refuse:
// entries a real run's proxy will never consult, so promoting one writes dead
// weight the operator believes is granting them something — the same honesty
// rule handleSetApprovedEgress and promoteSkipHosts already enforce at the
// other two write points.
//
// It is the UNION of those two sets, because they are NOT identical and neither
// is a superset: promoteSkipHosts (workspace-aware — model provider, the
// workspace's own bedrock transport, required-integration hosts, clone hosts,
// broker + brokered-SSH hosts) lacks only the control plane's OWN host, which
// handleSetApprovedEgress's inline static set carries. controlPlaneHost already
// lowercases, matching how that set inserts it raw.
//
// ALLOW-SHAPED ONLY — do not reuse this for the deny direction. A deny entry for
// a git-broker host IS consulted (runs_dispatch_gitbroker.go reads and extends
// policy.DeniedDomains), so the "never consulted" rationale does not transfer,
// and the deny direction's real hazard is the opposite one (see
// denyAlwaysReject).
func (s *Server) approveAlwaysRejects(ctx context.Context, ws types.Workspace) map[string]struct{} {
	skip := s.promoteSkipHosts(ctx, ws)
	if self := controlPlaneHost(s.cfg.ControlPlaneURL); self != "" {
		skip[self] = struct{}{}
	}
	return skip
}

// denyAlwaysReject reports why a deny·always on host must be refused, or "" to
// allow it. It is deliberately NOT approveAlwaysRejects' mirror: the hazard is
// not symmetric, and one shared set would be wrong in both directions.
// approve·always on api.anthropic.com is merely redundant, while deny·always on
// it BRICKS the workspace — deny beats everything the proxy evaluates, and
// Policy.AllowedExactHost (the gate for proxy-side credential injection)
// returns false on a denied host, so every future run of this workspace would
// launch with a model credential it can never use. An injected INTEGRATION host
// is worse still: buildInjector returns an error for a rule whose host is not
// exactly allowlisted, failing the sidecar outright rather than quietly
// disabling one credential.
//
// Best-effort BY DESIGN, and the message says so rather than implying the check
// is exhaustive. The hazard class is every host carrying a proxy-side injection
// rule (model providers, header-delivered integrations, artifact redirects);
// the two guarded here are the two that fail SILENTLY. The rest fail loudly at
// proxy build, where an operator can see and undo them.
func (s *Server) denyAlwaysReject(ctx context.Context, ws types.Workspace, host string) string {
	const caveat = " (this guard covers model-provider and required-integration hosts only; " +
		"a deny on another injected host fails loudly at proxy build instead)"
	if s.isModelProviderHost(host) {
		return "deny always on " + host + " would permanently break model access for this workspace: " +
			"proxy-side credential injection refuses a denied host" + caveat
	}
	for _, h := range s.integrationRequirementHosts(ctx, ws) {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return "deny always on " + host + " would break a required integration this workspace declares: " +
				"its credential is injected at that host" + caveat
		}
	}
	// M1 — a permanent deny must not contradict the workspace's own contract.
	if requiredEgressHost(ws, host) {
		return "deny always on " + host + " contradicts this workspace's requirements contract, which marks " +
			"egress:" + host + " required — a confined replay unions every required egress row into its own " +
			"allowlist, so the workspace would declare a need it can never satisfy. Turn that requirement off " +
			"(or set it optional) on the workspace first."
	}
	return ""
}

// requiredEgressHost reports whether ws's EFFECTIVE contract marks
// egress:<host> required — the M1 self-contradiction check behind
// denyAlwaysReject. confinedEgressDomains unions exactly these rows into a
// confined replay's AllowedDomains, so a workspace that both requires and
// permanently denies one host fails every replay on it. The path is reachable,
// not hypothetical: learnVerifyEgress writes precisely such a row on approve,
// so approve·always then deny·always the same host is one operator away.
//
// Refusing is chosen over silently clearing the requirement: this runs BEFORE
// Decide(), where a 4xx still means something and the operator learns which
// knob to turn, whereas clearing the row would delete an operator-declared
// contract entry as an invisible side effect of an approval click — from the
// write-back, after the decision is already durable and unauditable as a
// rejection.
func requiredEgressHost(ws types.Workspace, host string) bool {
	for key, req := range effectiveRequirements(ws) {
		if req.Level != "required" {
			continue
		}
		if typ, h, ok := types.SplitRequirementKey(key); ok && typ == "egress" &&
			strings.EqualFold(strings.TrimSpace(h), host) {
			return true
		}
	}
	return false
}

// persistWorkspaceEgressDecision is `always`'s durable half: the host the
// operator just decided lands on the run's PRIMARY workspace — approved_egress
// on approve, denied_egress on deny, and removed from the other list either way
// (deny beats allow everywhere the proxy evaluates policy, so a host left on
// both would make one direction a silent no-op) — so FUTURE runs inherit the
// decision instead of re-raising it.
//
// Host shape and the two direction-specific reject sets are validated in
// decide()'s rule 7, BEFORE Decide() flips the row. Nothing validating belongs
// here: this runs after a decision that is already durable and cannot be taken
// back, and answering 4xx on it would be worse than useless. Only genuine
// runtime failures reach here — cap reached, workspace deleted mid-flight — and
// they fail SILENT-BUT-AUDITED exactly like learnVerifyEgress: the approval
// itself stands either way, and the audit record is what lets an operator add
// the row by hand instead of wondering why the next run still asks.
//
// Audited under the workspace.egress.approve namespace the approved-egress PUT
// already owns, plus its workspace.egress.deny sibling, and with the same
// {"domains": [...]} payload shape, so one audit query answers "how did this
// host get onto this workspace's list" across all three writers.
func (s *Server) persistWorkspaceEgressDecision(ctx context.Context, ap types.ApprovalRequest, wsID uuid.UUID, allow bool, byType types.ActorType, by string) {
	action := "workspace.egress.deny"
	if allow {
		action = "workspace.egress.approve"
	}
	host := approvalHost(ap)
	data := map[string]any{"domains": []string{host}, "source": "approval:" + ap.ID.String()}
	// AUDIT the give-up paths too — this function's contract is fail-SILENT-BUT-
	// AUDITED, and a bare `return` here delivered only the first half. Both are
	// reachable and neither is cosmetic: a nil Store means the decision stands
	// with nothing durable behind it, and an empty host means we re-derived it
	// from the post-Decide RETURNING row rather than the `ap` rule 7 validated —
	// so if that RETURNING ever stops listing requested_scope, `always` silently
	// becomes a no-op. That is the same failure class as the SET-clause trap this
	// package already warns about, and it deserves the same visibility: an
	// operator who clicked Always and got a green UI must be able to find out
	// from the audit stream that nothing was written.
	//
	// Every emit below stamps the O5 cross-user marker (auditWorkspaceDataFor).
	// Deciding an approval is owner-OR-ADMIN (routes.go), so this is the most
	// common path on which an admin durably rewrites a MEMBER-owned workspace —
	// an `always` on someone else's run — and "which member's data did this
	// admin touch" has to stay a query here too, not just on the workspace
	// routes. The owner comes from the write's own returned row where there is
	// one, and from a marker-only re-read on the give-up paths.
	if s.cfg.Store == nil || host == "" {
		reason := "no store configured"
		if s.cfg.Store != nil {
			reason = "approval carries no host in requested_scope"
		}
		data["detail"] = reason
		s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, action, wsID.String(), "failure",
			auditWorkspaceDataFor(by, s.workspaceOwner(ctx, wsID), data)))
		return
	}
	ws, err := s.cfg.Store.AddWorkspaceEgressDecision(ctx, wsID, host, allow, maxApprovedEgress)
	if err != nil {
		data["detail"] = err.Error()
		s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, action, wsID.String(), "failure",
			auditWorkspaceDataFor(by, s.workspaceOwner(ctx, wsID), data)))
		return
	}
	s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, action, wsID.String(), "success",
		auditWorkspaceDataFor(by, ws.OwnedBy, data)))
}

// learnVerifyEgress is the verify loop's write-back, hooked at the ONE
// chokepoint every approval decision funnels through: approving an
// egress_domain request raised DURING a workspace verify/record session lands
// the host as an `egress:<host>` row in THAT workspace's own requirements
// contract (required, operator_set — a human just clicked) the moment the
// decision is made. The gates are the trusted linkages, never sandbox input:
// the run row's WorkspaceID and its "workspace record" task discriminator — a
// PLAIN run's approval widens only its own run and writes NOTHING durable.
// The row lands on the WORKSPACE overlay, never a shared library source:
// approving a host for this aggregate must not leak the approval into every
// other workspace attaching the same source. Every guard fails silent — the
// approval itself already stands; this is the durable echo, not the decision.
func (s *Server) learnVerifyEgress(ctx context.Context, ap types.ApprovalRequest, byType types.ActorType, by string) {
	if ap.Kind != types.ApprovalEgressDomain || s.cfg.Store == nil {
		return
	}
	// Scope gate, reading the RETURNING echo (ap.DecisionScope) like the
	// deny·always write-back does: an explicitly EPHEMERAL decision must not
	// leave the widest durable mark this handler can make. `once` promised
	// "the next attempt asks again"; `until` promised a time-box — a permanent
	// required contract row contradicts both. ""/run (the verify loop's normal
	// path) and always keep learning. If time-boxed grants should teach the
	// contract after all, drop ScopeUntil here — a one-line owner decision.
	if sc := ap.DecisionScope.Normalize(); sc == types.ScopeOnce || sc == types.ScopeUntil {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, ap.RunID)
	if err != nil || run.WorkspaceID == nil || run.Task != "workspace record" {
		return
	}
	// FOLDED WITH THE HOST CHECK BELOW, because they are one give-up with two
	// causes. W19-W19b-5 made "empty or malformed host" audit its miss, and the
	// UNMARSHAL failure four lines above it kept failing silent — so a
	// requested_scope that is valid JSON but not an OBJECT (an array, a string, a
	// number) still produced the exact symptom that fix was written to eliminate:
	// HTTP 200, an empty requirements map, and nothing in the audit stream saying
	// why. The two are indistinguishable to the operator and now answer the same
	// way, with the detail naming which of them it was.
	var scope struct {
		Host string `json:"host"`
	}
	decodeErr := json.Unmarshal(ap.RequestedScope, &scope) != nil
	host := strings.ToLower(strings.TrimSpace(scope.Host))
	if decodeErr || host == "" || !hostrules.ValidApprovedHost(host) {
		detail := "invalid or empty host in requested_scope"
		if decodeErr {
			detail = "requested_scope is not a JSON object"
		}
		s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, "workspace.requirement.write",
			run.WorkspaceID.String(), "failure", auditWorkspaceDataFor(by, s.workspaceOwner(ctx, *run.WorkspaceID), map[string]any{
				"source": "verify:" + ap.RunID.String(), "detail": detail,
			})))
		return
	}
	key := "egress:" + host
	// Same O5 marker as the deny·always write-back beside it, and reachable the
	// same way: a record run an ADMIN launched on a member-owned workspace ends
	// with the admin approving a host into that member's requirements contract.
	ws, err := s.cfg.Store.MergeWorkspaceRequirements(ctx, *run.WorkspaceID, map[string]types.WorkspaceRequirement{
		key: {Level: "required", Provenance: "operator_set"},
	})
	if err != nil {
		// The approval stands either way; the contract write is audited as the
		// miss it is (cap hit / workspace gone) so the operator can add the row
		// on Reach instead of wondering why the replay still denies the host.
		s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, "workspace.requirement.write",
			run.WorkspaceID.String(), "failure", auditWorkspaceDataFor(by, s.workspaceOwner(ctx, *run.WorkspaceID), map[string]any{
				"key": key, "source": "verify:" + ap.RunID.String(), "detail": err.Error(),
			})))
		return
	}
	s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, "workspace.requirement.write",
		run.WorkspaceID.String(), "success", auditWorkspaceDataFor(by, ws.OwnedBy, map[string]any{
			"key": key, "source": "verify:" + ap.RunID.String(),
		})))
}
