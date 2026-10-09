// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// boundUserSpec is THE member bounding pipeline — the three stages that turn
// a spec a member chose into one an admin authorized, in the one order that is
// correct:
//
//  1. composer.Clamp against the member's ceiling. It bounds egress,
//     confinement and TTL and drops grant KINDS outside the ceiling, but NOT a
//     stored-secret grant's SCOPE (which operator secret is paired with which
//     host) — that stays member-authored, and on its own would be a
//     secret-exfil primitive. Hence stage 2.
//  2. filterUserGrants drops any api_key/git_pat/ssh_key grant whose pairing
//     the operator did not eligible-list. The run's own model-access grant
//     comes from its model provider, or is re-added at launch by
//     applyWorkspaceRequirements (a workspace requirement).
//  3. narrowUserInlinePolicy bounds what survived to what THIS member
//     personally holds: stage 1 is the OPERATOR's deployment-wide ceiling,
//     stage 3 is this member's own capability grants. A pairing clears BOTH.
//
// Every drop is AUDITED, not merely warned: a warning alone left an operator
// unable to tell that a member had tried to pair one of their secrets with a
// host of the member's own choosing (the gap ROADMAP names). dryRun suppresses
// that write for the same reason it suppresses policy.inline.apply (see below).
//
// It is ONE function because the inline branch and the stored branch must bound
// identically: "member-selected content is bounded by the member's ceiling
// whether it arrived as a body or as a row id" is only true while both run
// THIS, and two hand-copied pipelines could drift into a member smuggling
// through one what the other refuses.
//
// errPrefix is the ONLY thing the two callers differ on, and it stays theirs —
// naming the wrong input back at the caller is a worse error message, not a
// smaller one. Returns ok==false when the response is already written.
//
// It takes the WHOLE resolved ceiling rather than its spec: stage 1 needs the
// request-shape limits beside the spec too (GovernanceLimits.MaxEphemeralDiskMiB,
// which dispatch is the authority on — this call is what makes the member's
// PREVIEW of it agree, see composer.Clamp).
func (s *Server) boundUserSpec(ctx context.Context, w http.ResponseWriter, r *http.Request, spec types.RunPolicySpec, ceiling governanceCeiling, errPrefix string, dryRun bool) (types.RunPolicySpec, []string, bool) {
	spec, warnings, refusal := s.boundRunUserSpec(ctx, r, spec, ceiling, errPrefix, dryRun)
	return spec, warnings, !refusal.write(s, w, r)
}

// resolveRunPolicy resolves the RunPolicySpec + policy id to attach to a run,
// from the create-run request. It is the inline-policy-aware replacement for the
// bare resolvePolicy call: it owns the XOR check, the inline validation, and the
// stored/default fallback. It writes its own HTTP error and returns ok=false
// when it has already responded; callers must stop on ok=false.
//
// Resolution (fail closed; shared by both handleCreateRun and the
// handlePreflightRun dry-run, so a member's preview can never disagree with
// what launch actually does):
//   - inline_policy AND policy_id both set  => 400 (mutually exclusive).
//   - inline_policy set                     => for a MEMBER caller,
//     clamp to composer.Clamp(spec, THEIR CEILING) FIRST — an admin-authored
//     ceiling a member's own inline_policy can never exceed. An admin is
//     UNCLAMPED (they ARE the ceiling-setting authority). Only THEN validate
//     via validatePolicySpec (so runner.ValidateMount gates any inline mount)
//     AND validateInlineSecretRefs (so any inline api_key grant references a
//     real, non-reserved secret); on success the (possibly clamped) inline
//     spec attaches with a NIL policy id (it is not a stored row) and a
//     policy.inline.apply audit event is emitted — which therefore already
//     reflects the clamped spec, not the raw member-submitted one.
//   - else (policy_id set, or neither)      => the existing resolvePolicy path
//     (stored row, else the caller's own CEILING),
//     THEN validateInlineSecretRefs against the resolved spec — same
//     secret-existence check as the inline branch, including a 422 when the
//     spec's grants exist but no secret store is configured. This is a
//     deliberate behavior change (see CHANGELOG): a stored or default policy
//     naming a missing/reserved secret now 422s at create instead of only
//     failing later at first proxy injection or clone. A member who SELECTED a
//     stored row runs the same three-stage member pipeline the inline branch
//     does, when and only when a governance profile applies to them — see that
//     branch for the scoping and for why a bare Clamp is not enough.
//
// The ceiling is resolved once, here, and is this PRINCIPAL's rather than the
// deployment's (effectiveCeiling). For a member with no governance assignment
// it IS Config.DefaultPolicy, so every path below is byte-for-byte today for
// them; for an assigned member it is the profile an admin bound to them.
//
// dryRun suppresses the policy.inline.apply audit write: a preflight preview is not an
// inline-policy USE, and the audit feed is the system of record — orphan
// policy.inline.apply rows with no following run.create would be indistinguishable
// from real authorizations.
//
// The 4th return is L6's clamp-warning list (composer.Clamp's own "what did I
// change" notes, plus the capability/grant drops, plus any grant a governance
// profile can no longer serve after a DefaultPolicy redeploy) — non-nil on the
// member branches, which are the only resolution paths that ever clamp. BOTH
// callers surface it: handlePreflightRun in Review, so a member
// sees WHY their inline_policy differs from what they typed before they
// launch, and handleCreateRun on the 201, because the console launches without
// preflighting and a silent narrowing is a run that quietly is not the run the
// member asked for.
func (s *Server) resolveRunPolicy(ctx context.Context, w http.ResponseWriter, r *http.Request, req *createRunRequest, dryRun bool) (types.RunPolicySpec, *uuid.UUID, []string, policySourceRecord, bool) {
	spec, id, warnings, source, refusal := s.resolveRunPolicyFacts(ctx, r, req, dryRun, true)
	return spec, id, warnings, source, !refusal.write(s, w, r)
}

// capEphemeralDiskPreview bounds spec's disk_mib by the profile's
// MaxEphemeralDiskMiB and reports whether it changed anything. The LAUNCH half
// of the pair above; previewEphemeralDisk (runs_dispatch_ceiling.go) is the
// dry-run half that also fills and applies the org ceiling.
//
// A FRESH Resources block, never an in-place write: one shared *ResourceLimits
// would re-size every later run that reads it — the same aliasing rule dispatch
// obeys, and the reason composer.Clamp rebuilds the struct instead of editing
// the caller's.
func capEphemeralDiskPreview(spec *types.RunPolicySpec, maxEphemeralDiskMiB int) bool {
	if spec.Resources == nil {
		return false
	}
	capped := composer.CapDiskMiB(spec.Resources.DiskMiB, maxEphemeralDiskMiB)
	if capped == spec.Resources.DiskMiB {
		return false
	}
	rl := *spec.Resources
	rl.DiskMiB = capped
	spec.Resources = &rl
	return true
}
