// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The durable write-back: what a decided approval leaves behind on a WORKSPACE.
// An `always` decision and a verify-loop approval both edit a workspace the
// operator may not be looking at, and share one contract: fail silent but
// audited. The decision already stands when these run, so none may fail the
// request; every give-up path must leave an audit row instead.
//
// internal/api/approvals_reconcile.go is the BOOT-TIME twin: it replays the same
// `always` verdicts and calls this file's two direction-specific reject predicates
// so the two cannot disagree about what a workspace will accept.
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

// approveAlwaysRejects is the set of hosts an approve·always must refuse: entries
// a real run's proxy never consults, so promoting one writes dead weight — the
// rule handleSetApprovedEgress and promoteSkipHosts enforce at the other two
// write points. It is the UNION of those sets, as neither is a superset:
// promoteSkipHosts lacks only the control plane's OWN host (controlPlaneHost
// lowercases, matching how handleSetApprovedEgress inserts it raw).
//
// Allow-shaped only — do not reuse it for the deny direction: a deny entry for a
// git-broker host IS consulted (runs_dispatch_gitbroker.go extends
// policy.DeniedDomains), and the deny hazard is the opposite (denyAlwaysReject).
func (s *Server) approveAlwaysRejects(ctx context.Context, ws types.Workspace) map[string]struct{} {
	skip := s.promoteSkipHosts(ctx, ws)
	if self := controlPlaneHost(s.cfg.ControlPlaneURL); self != "" {
		skip[self] = struct{}{}
	}
	return skip
}

// denyAlwaysReject reports why a deny·always on host must be refused, or "" to
// allow it. It is NOT approveAlwaysRejects' mirror: deny·always on a model
// provider BRICKS the workspace — deny beats everything the proxy evaluates, and
// Policy.AllowedExactHost (the gate for proxy-side credential injection) returns
// false on a denied host — and on an injected INTEGRATION host buildInjector
// fails the sidecar outright rather than disabling one credential.
//
// Best-effort BY DESIGN, and the message says so: every host with a proxy-side
// injection rule is in the hazard class, but these two are the ones that fail
// SILENTLY; the rest fail loudly at proxy build, where an operator can undo them.
func (s *Server) denyAlwaysReject(ctx context.Context, ws types.Workspace, host string) string {
	const caveat = " (this guard covers model-provider and required-integration hosts only; " +
		"a deny on another injected host fails loudly at proxy build instead)"
	// isModelProviderRejectHost, not isModelProviderHost: the Bedrock lane's
	// bedrock-runtime.<region> (and the WARDYN_BEDROCK_BASE_URL override host)
	// carries proxy-side bearer injection too, and this guard consulted the
	// anthropic/openai-only predicate, so a workspace-bricking deny·always on it
	// was accepted.
	if s.isModelProviderRejectHost(ctx, ws, host) {
		return "deny always on " + host + " would permanently break model access for this workspace: " +
			"proxy-side credential injection refuses a denied host" + caveat
	}
	for _, h := range s.integrationRequirementHosts(ctx, ws) {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return "deny always on " + host + " would break a required integration this workspace declares: " +
				"its credential is injected at that host" + caveat
		}
	}
	// A permanent deny must not contradict the workspace's own contract.
	if requiredEgressHost(ws, host) {
		return "deny always on " + host + " contradicts this workspace's requirements contract, which marks " +
			"egress:" + host + " required — a confined replay unions every required egress row into its own " +
			"allowlist, so the workspace would declare a need it can never satisfy. Turn that requirement off " +
			"(or set it optional) on the workspace first."
	}
	return ""
}

// requiredEgressHost reports whether ws's EFFECTIVE contract marks
// egress:<host> required — the self-contradiction check behind
// denyAlwaysReject. confinedEgressDomains unions these rows into a confined
// replay's AllowedDomains, so requiring and permanently denying one host fails
// every replay on it; learnVerifyEgress writes such a row on approve, so it is
// reachable. Refusing beats silently clearing the requirement: this runs BEFORE
// Decide(), where a 4xx still tells the operator which knob to turn, and clearing
// would delete an operator-declared contract entry as a side effect of a click.
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

// persistWorkspaceEgressDecision is `always`'s durable half: the decided host
// lands on the run's PRIMARY workspace — approved_egress on approve, denied_egress
// on deny, removed from the other list either way (deny beats allow, so a host on
// both would make one direction a silent no-op) — so FUTURE runs inherit it.
//
// Validation belongs in decide()'s rule 7, BEFORE Decide() flips the row; this
// runs after the decision is durable, so its runtime failures (cap reached,
// workspace deleted) are SILENT-BUT-AUDITED like learnVerifyEgress. Audited under
// workspace.egress.approve / workspace.egress.deny with the approved-egress PUT's
// {"domains": [...]} payload, so one query answers "how did this host get onto
// this workspace's list" across all three writers.
func (s *Server) persistWorkspaceEgressDecision(ctx context.Context, ap types.ApprovalRequest, wsID uuid.UUID, allow bool, byType types.ActorType, by string) {
	action := "workspace.egress.deny"
	if allow {
		action = "workspace.egress.approve"
	}
	host := approvalHost(ap)
	data := map[string]any{"domains": []string{host}, "source": "approval:" + ap.ID.String()}
	// Audit the give-up paths too: a bare `return` would make the contract silent
	// but not audited. Both are reachable: a nil Store leaves the decision with
	// nothing durable behind it, and an empty host means the post-Decide RETURNING
	// row stopped listing requested_scope, turning `always` into a silent no-op.
	//
	// Every emit stamps the cross-user marker (auditWorkspaceDataFor): deciding is
	// owner-OR-ADMIN (routes.go), so this is the commonest path on which an admin
	// rewrites a MEMBER-owned workspace. The owner comes from the write's returned
	// row where there is one, and from a marker-only re-read on the give-up paths.
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
	if ap.Kind != types.ApprovalEgressDomain {
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
	// UNFOLDED from the kind check above and from the run-shape check below,
	// because a missing store and an unreadable run are GIVE-UPS, not
	// not-applicable conditions: this file's contract is fail silent but audited
	// (header), so each one has to leave a workspace.requirement.write failure
	// naming its cause. Both carry an EMPTY target: the workspace link lives on
	// the run row neither branch got to read, so naming a workspace here would
	// be inventing one. Mirrors persistWorkspaceEgressDecision's own
	// "no store configured" arm, and sits AFTER the scope gate so an explicitly
	// ephemeral decision — which was never going to teach the contract — does
	// not report a miss it never had.
	if s.cfg.Store == nil {
		s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, "workspace.requirement.write",
			"", "failure", auditWorkspaceDataFor(by, "", map[string]any{
				"source": "verify:" + ap.RunID.String(), "detail": "no store configured",
			})))
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, ap.RunID)
	if err != nil {
		s.recordAudit(ctx, s.auditEvent(&ap.RunID, byType, by, "workspace.requirement.write",
			"", "failure", auditWorkspaceDataFor(by, "", map[string]any{
				"source": "verify:" + ap.RunID.String(), "detail": "read run: " + err.Error(),
			})))
		return
	}
	// NOT audited, unlike the two arms above and the two below: these are the
	// genuinely NOT-APPLICABLE conditions — a plain run, or a run carrying no
	// workspace link — where there is no contract to teach and so no miss to
	// report. A failure row here would cry wolf on every ordinary approval.
	if run.WorkspaceID == nil || run.Task != "workspace record" {
		return
	}
	// Folded with the host check below, because they are one give-up with two
	// causes: a requested_scope that is valid JSON but not an OBJECT (an array, a
	// string, a number) would otherwise produce the same symptom as an empty or
	// malformed host — HTTP 200, an empty requirements map, and nothing in the
	// audit stream saying why. The two are indistinguishable to the operator and
	// answer the same way here, with the detail naming which of them it was.
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
	// Same cross-user marker as the deny·always write-back beside it, and reachable the
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
