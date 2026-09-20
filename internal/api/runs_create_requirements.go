// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// This file is the WORKSPACE-REQUIREMENTS fold: turning an onboarded workspace's
// declared contract (the needs-scanner's secret/egress/write/integration rows,
// plus each attachment's overrides) into entries on a run's resolved spec.
// EXTRACTED from runs_create.go, unchanged, when that file crossed the 1000-line
// gate — one seam, one file: everything here answers "what does this workspace
// say it needs", and nothing here decides confinement, mints a grant or wires a
// sandbox.

// requirementAuditEntry is one audit-worthy fact applyWorkspaceRequirements
// produced (an auto-attached secret grant, or a non-empty egress addition).
// applyWorkspaceRequirements itself never audits — like foldRunIntegration, the
// "fold" is separated from "audit" (the create path emits the events once the
// run id is minted) so preflight, which persists nothing, can call the SAME
// fold and simply discard these.
type requirementAuditEntry struct {
	action string
	target string
	data   map[string]any
}

// resolveWorkspaceSelections builds the per-workspace selection map
// applyWorkspaceRequirements consults, keyed by workspace id string, from
// CreateRunRequest.Workspaces PLUS the legacy singular WorkspaceID — kept a
// "working single-selection alias": a caller using ONLY workspace_id (every
// pre-existing SDK/CLI/policy caller) must fold IDENTICALLY to explicitly
// selecting that one workspace with no optional requirement enabled and no
// write narrowing, which is exactly the zero-value WorkspaceSelection. An
// explicit Workspaces[] entry for the same id wins over the synthesized alias.
// Shared by create (runs.go) and preflight so the two can never disagree about
// which selection a workspace resolves to.
func resolveWorkspaceSelections(req createRunRequest) map[string]client.WorkspaceSelection {
	out := make(map[string]client.WorkspaceSelection, len(req.Workspaces))
	for _, sel := range req.Workspaces {
		if sel.WorkspaceID != "" {
			out[sel.WorkspaceID] = sel
		}
	}
	if req.WorkspaceID != nil {
		id := req.WorkspaceID.String()
		if _, exists := out[id]; !exists {
			out[id] = client.WorkspaceSelection{WorkspaceID: id}
		}
	}
	return out
}

// applyWorkspaceRequirements folds each referenced workspace's requirements
// contract (types.Workspace.Requirements) into the run's RESOLVED policy — the
// per-workspace analogue of unionWorkspaceEgress/applyWorkspaceCreds, kept as
// its OWN function and never inlined into either of those (nor into
// ensureLLMGrant): a reviewer specifically flagged that a fourth folder
// inlined among those three is where double-grants and dropped hosts hide.
//
// Owner's contract, verbatim: "Any setting that is required always comes by
// default with the workspace whenever it is used; anything optional is then
// configurable / enabled when necessary." Concretely, per requirement type:
//
//   - egress:<host>  Required unions host into AllowedDomains unconditionally
//     (mirrors unionWorkspaceEgress's own unconditional append). Optional
//     unions it ONLY when the run's selection lists the key in
//     EnabledOptional.
//   - secret:<NAME>  Required+operator_set mints the SAME api_key-style grant
//     shape the pre-Integration applyWorkspaceCreds used for a workspace's
//     api_key binding (git history: `git show ecc1903~1:internal/api/llmcred.go`,
//     the pre-Integration api_key case), scoped to the run's agent's own
//     model-provider host (applyRequiredSecretGrant). Required+scan_seeded
//     NEVER auto-grants — see the TRUST BOUNDARY comment below. An optional
//     secret follows the identical rule, gated additionally on the run's
//     selection enabling the key.
//   - write:<path>   Resolves the SOURCE's effective writability onto every
//     already-seeded mount at that path: Required defaults writable, Optional
//     defaults read-only unless enabled — and the per-run ReadOnly selection
//     may only NARROW that default (force read-only), never widen it (see
//     applyWriteNarrowing).
//   - integration:<id>  Unions the named integration's hosts into
//     AllowedDomains and, when it delivers a credential by header, authors the
//     proxy-side injection grant for each of them
//     (applyIntegrationRequirement, integrations_run.go). This is the ONLY way
//     an integration reaches a run: configuring one grants nothing by itself.
//
// A workspace with NO requirements declared is architecturally a no-op here —
// the loop below only ever visits declared keys — which is exactly what keeps
// every pre-contract (or simply unconfigured) workspace's resolved spec
// byte-identical to today's behavior.
//
// Must run BEFORE persistRunGrants, which snapshots spec.EligibleGrants into
// persisted grants + proxy injections, and AFTER the workspace's own
// mounts/repos are already seeded onto spec (seedRequestWorkspace / an
// inline/stored policy) so the write-path narrowing below has a mount to
// adjust.
//
// Returns one entry per auto-attached secret grant and one per workspace with
// a non-empty egress addition, for the CALLER to audit (launch does; preflight
// discards them — see requirementAuditEntry).
func (s *Server) applyWorkspaceRequirements(ctx context.Context, spec *types.RunPolicySpec, agent string, wsRefs []types.Workspace, selections map[string]client.WorkspaceSelection) []requirementAuditEntry {
	return s.applyWorkspaceRequirementsFor(ctx, nil, spec, agent, wsRefs, selections)
}

// applyWorkspaceRequirementsFor is applyWorkspaceRequirements with the
// CALLER's presence map. A request handler passes the owner-scoped map it
// already computed (presentSecretNamesFor with secretOwnerFromRequest), so a
// workspace's integration requirement resolves a member's own key exactly as
// their run will; nil means the operator namespace, computed lazily — the
// record route and every existing test keep that.
func (s *Server) applyWorkspaceRequirementsFor(ctx context.Context, present map[string]bool, spec *types.RunPolicySpec, agent string, wsRefs []types.Workspace, selections map[string]client.WorkspaceSelection) []requirementAuditEntry {
	var events []requirementAuditEntry
	// Resolved at most once per call, lazily on the first integration key
	// found — effectiveIntegrations reads the site-config
	// store, a full secret listing, and peeks the subscription/Bedrock state,
	// so recomputing it per requirement (a workspace with 6 integration
	// requirements = 6 full derivations) multiplied that I/O by the
	// requirement count on every POST /runs and /runs/preflight.
	var integrationRows []integrationRow
	integrationRowsLoaded := false
	for _, ws := range wsRefs {
		if len(effectiveRequirements(ws)) == 0 {
			continue
		}
		sel := selections[ws.ID.String()] // zero value when absent: nothing optional enabled, no narrowing
		var addedEgress []string
		// Sorted iteration: map order is otherwise nondeterministic, and this
		// drives grant-creation and audit-event ordering.
		reqs := effectiveRequirements(ws)
		for _, key := range sortedKeys(reqs) {
			req := reqs[key]
			typ, name, ok := splitRequirementKey(key)
			if !ok {
				continue // defense only: the write endpoint already rejects a bad key
			}
			enabled := req.Level == "required" || slices.Contains(sel.EnabledOptional, key)
			switch typ {
			case "egress":
				if !enabled {
					continue
				}
				// Same TRUST BOUNDARY as the secret case below, gated
				// behind RequireOperatorSetEgress (DEFAULT TRUE —
				// see the Config field doc). When enabled, a scan_seeded
				// egress host (the workspace scanner reading untrusted repo
				// content) is skipped; only an operator's DIRECT declaration
				// auto-adds. The predicate is shared with the confined-replay
				// path — see egressProvenanceAllowed in workspace_egress.go.
				if !s.egressProvenanceAllowed(req) {
					continue
				}
				addedEgress = append(addedEgress, unionAllowedDomains(spec, []string{name})...)
			case "secret":
				if !enabled {
					continue
				}
				// TRUST BOUNDARY (security-critical — do not relax): a
				// scan_seeded secret requirement comes from the WORKSPACE
				// SCANNER reading UNTRUSTED repo content (e.g. a committed
				// .env template naming a var), never from an operator's own
				// action. Auto-minting a grant from it would let a hostile,
				// or simply never-reviewed, workspace route the OPERATOR's
				// own stored secrets into a run just by naming them in a
				// dotenv. Only an operator's DIRECT declaration
				// (operator_set) may ever auto-grant.
				if req.Provenance != "operator_set" {
					continue
				}
				if present == nil {
					present = s.presentSecretNames(ctx)
				}
				if ev, ok := s.applyRequiredSecretGrant(present, spec, agent, name); ok {
					events = append(events, ev)
				}
			case "integration":
				if !enabled {
					continue
				}
				// No provenance gate, unlike secret: above. A scan can propose a
				// HOST it saw, but it cannot propose an INTEGRATION — an
				// integration id only exists because an operator configured that
				// row and then named it here, so naming one is already a direct
				// operator act. The trust boundary that rule protects (untrusted
				// repo content routing the operator's stored secrets into a run)
				// has no path here.
				if !integrationRowsLoaded {
					if present == nil {
						present = s.presentSecretNames(ctx)
					}
					// Operator scope (integrations_write.go's reason): a workspace
					// requirement names an integration ROW, never a principal's session.
					integrationRows = s.effectiveIntegrations(ctx, present, s.setupBedrock(ctx, present, awsSSOScope{}))
					integrationRowsLoaded = true
				}
				if ev, ok := s.applyIntegrationRequirement(ctx, present, integrationRows, spec, name); ok {
					events = append(events, ev)
				}
			case "write":
				applyWriteNarrowing(spec, name, enabled, sel.ReadOnly)
			}
		}
		if len(addedEgress) > 0 {
			events = append(events, requirementAuditEntry{
				action: "run.workspace.requirement.egress", target: ws.ID.String(),
				data: map[string]any{"workspace_id": ws.ID.String(), "added_domains": addedEgress},
			})
		}
	}
	return events
}

// effectiveRequirements is the contract a run actually consumes: the store's
// hydrate pass folds attachments + source contracts + the overlay into
// EffectiveRequirements. A workspace that never passed through hydration (a
// hand-built fixture, a fake store) carries none — and for it the fold's own
// zero-source identity says the overlay IS the contract, so falling back to
// Requirements is the correct semantic, not a compatibility shim.
func effectiveRequirements(ws types.Workspace) map[string]types.WorkspaceRequirement {
	if ws.EffectiveRequirements != nil {
		return ws.EffectiveRequirements
	}
	return ws.Requirements
}

// applyRequiredSecretGrant mints the api_key-style grant an operator-declared
// required (or enabled-optional) secret requirement promises, coupling it to
// an exact egress allowlist entry — the SAME grant/injection/egress shape the
// pre-Integration applyWorkspaceCreds used for a workspace's api_key binding
// (git history: `git show ecc1903~1:internal/api/llmcred.go`,
// pre-Integration api_key case), scoped to the run's AGENT's own
// model-provider host (agentLLMProvider) — the only host this generic
// requirement key has any deterministic binding to. ok=false — no grant, no
// mutation — when: the agent has no LLM-provider convention (nothing to bind
// to); a grant for that host is already proposed (never double-grant the same
// host — whichever caller proposed it first wins, mirroring
// ensureLLMGrant/applyWorkspaceCreds); or the named secret is not actually
// stored (an auto-mint grant with no resolvable secret would fail the proxy
// CLOSED at startup — degrade silently to no-model-access instead of bricking
// the run; compose_setup.go's checklist escalates the gap to blocking styling).
func (s *Server) applyRequiredSecretGrant(present map[string]bool, spec *types.RunPolicySpec, agent, secretName string) (requirementAuditEntry, bool) {
	p, ok := s.llmProviderFor(agent)
	if !ok {
		return requirementAuditEntry{}, false
	}
	if _, exists := apiKeyGrantForHost(spec, p.host); exists {
		return requirementAuditEntry{}, false
	}
	if !present[secretName] {
		return requirementAuditEntry{}, false
	}
	scope, _ := json.Marshal(map[string]string{
		"host": p.host, "header": p.header, "format": p.format, "secret_name": secretName,
	})
	spec.EligibleGrants = append(spec.EligibleGrants, types.GrantSpec{
		Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600, RequiresApproval: false,
	})
	// Couple the exact-host egress entry UNCONDITIONALLY, even under allow-all:
	// AllowedExactHost does not honor allow-all, so a grant whose host
	// is missing from AllowedDomains fails the proxy injector closed at startup
	// (zero egress) — e.g. a required operator_set secret on an allow_all_egress
	// ceiling would otherwise brick every run granted it.
	unionAllowedDomains(spec, []string{p.host})
	return requirementAuditEntry{
		action: "run.workspace.requirement.secret", target: secretName,
		data: map[string]any{"secret_name": secretName, "host": p.host},
	}, true
}

// applyWriteNarrowing resolves ONE write:<path> requirement's effective mount
// writability onto every spec.WorkspaceMounts entry whose Source is path (a
// no-op when path isn't actually mounted this run — e.g. a multi-source
// workspace attached only partially). grantedDefault is the contract's own
// default BEFORE narrowing (true for Required, or an enabled Optional); narrow
// is the run's OPTIONAL per-source override and is NARROW-ONLY: narrow=true
// forces read-only regardless of grantedDefault (a run may drop a Required
// write); narrow=false or nil never WIDENS past grantedDefault (a run may not
// add an Optional write it never enabled by putting the key in
// EnabledOptional — only Level/EnabledOptional can grant write, never this
// field alone).
func applyWriteNarrowing(spec *types.RunPolicySpec, path string, grantedDefault bool, narrow *bool) {
	granted := grantedDefault
	if narrow != nil && *narrow {
		granted = false
	}
	for i := range spec.WorkspaceMounts {
		if spec.WorkspaceMounts[i].Source != path {
			continue
		}
		ro := !granted
		spec.WorkspaceMounts[i].ReadOnly = &ro
	}
}
