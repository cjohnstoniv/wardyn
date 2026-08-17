// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// workspace_requirements.go owns the requirements CONTRACT — the
// "<type>:<key>" → level/provenance map a workspace carries and
// PUT /workspaces/{id}/requirements writes: the key grammar and its
// per-type suffix validation, the two closed enums, the endpoint itself, and
// the server-side belt that keeps the operator's overlay from shadowing a
// source's own scanned contract. Split out of workspaces.go when that file
// crossed its size cap; the grammar is the seam — every helper here answers
// "is this key+value a thing a run's fold can honestly act on", and the fold
// that acts on it lives in runs_create.go (applyWorkspaceRequirements), not
// here. workspace_requirements_test.go already covered exactly this set.

// integrationRefRE is the contract-side integration reference grammar:
// operator-authored slugs (secretNameRE's charset) plus up to three
// colon-joined qualifier segments — the shapes Wardyn's own legacy adoption
// mints and stores verbatim. 128-char cap matches the id rule.
var integrationRefRE = regexp.MustCompile(`^[a-z0-9._-]{1,128}(:[a-z0-9._-]{1,64}){0,3}$`)

// maxWorkspaceRequirements bounds the requirements-contract map a single PUT
// may set — a sane ceiling against a hostile/misbehaving request, not a sizing
// of any real contract.
const maxWorkspaceRequirements = 256

// validRequirementLevel / validRequirementProvenance are WorkspaceRequirement's
// two closed enums (types.go documents both on WorkspaceRequirement).
func validRequirementLevel(l string) bool      { return l == "required" || l == "optional" }
func validRequirementProvenance(p string) bool { return p == "scan_seeded" || p == "operator_set" }

// splitRequirementKey splits a Workspace.Requirements key on the FIRST colon
// into its fixed type prefix ("secret"|"egress"|"write") and suffix, per the
// grammar documented on types.Workspace.Requirements — never the LAST colon: a
// write:<path> suffix may itself legally contain colons. ok=false when there
// is no colon, the suffix is empty, or the prefix isn't one of the three known
// tokens. Shared with the fold this contract feeds (runs_create.go's
// applyWorkspaceRequirements) and the preflight checklist escalation
// (compose_setup.go's setupWorkspaceSecretItems).
func splitRequirementKey(key string) (typ, rest string, ok bool) {
	// The grammar moved home to internal/types (the store's hydrate pass folds
	// contracts and cannot import api); this alias keeps api's call sites put.
	return types.SplitRequirementKey(key)
}

// validateWorkspaceRequirement validates one Workspace.Requirements key+value
// pair before it is ever persisted: the key's grammar (splitRequirementKey),
// the suffix shape appropriate to its type — egress host via
// workspacescan.ValidApprovedHost (the SAME rule approved-egress promotion
// enforces: a plain lowercase host, no scheme/port/wildcard — deliberately
// stricter than a run policy's AllowedDomains, which also accepts a
// "*."-wildcard; a requirement is written here through an operator-gated
// endpoint like approved-egress, so the same conservative rule applies),
// secret name via the existing secret-name rule (validSecretRef: secretNameRE
// plus the reserved-platform-name guard), write path via
// runner.ValidateMountSource (the same host bind-mount deny-list a policy
// mount is checked against) — and the value's level/provenance enums. A
// non-empty return is the 400 message.
func validateWorkspaceRequirement(key string, req types.WorkspaceRequirement) string {
	typ, rest, ok := splitRequirementKey(key)
	if !ok {
		return fmt.Sprintf("invalid requirement key %q (want secret:<name>, egress:<host>, write:<path>, or integration:<id>)", key)
	}
	switch typ {
	case "secret":
		// sinkReservedSecret, not validSecretRef (WSPIPE-6): this key feeds
		// applyRequiredSecretGrant, a real credential SINK (runs_create.go),
		// which every other sink (policy.go, inline_policy.go) guards with
		// sinkReservedSecret — validSecretRef's plain reservedSecret misses the
		// three resident AWS SigV4 names, so a "required" row naming one
		// validated clean here and only 403'd at the run's first model call,
		// the real reason buried in an audit event instead of this write-time 400.
		if !secretNameRE.MatchString(rest) || sinkReservedSecret(rest) {
			return fmt.Sprintf("requirement %q: invalid secret name", key)
		}
	case "integration":
		// Shape only. EXISTENCE is deliberately not checked here: a workspace
		// may name an integration before it is configured (the contract states
		// an intent), and the fold degrades silently to "opens nothing" until
		// the row exists. Requiring it to exist first would make ordering the
		// operator's problem.
		//
		// The ref grammar is WIDER than an operator-authored id
		// (validateIntegrationWrite's secretNameRE): Wardyn itself mints
		// colon-qualified ids for ADOPTED legacy rows
		// ("anthropic_subscription:managed", "git_host:<host>") and stores
		// them verbatim — a contract must
		// be able to name what the store holds. Split on the FIRST colon at
		// the key layer keeps this unambiguous.
		if !integrationRefRE.MatchString(rest) {
			return fmt.Sprintf("requirement %q: invalid integration id", key)
		}
	case "egress":
		if !hostrules.ValidApprovedHost(rest) {
			return fmt.Sprintf("requirement %q: invalid egress host (plain lowercase host, no scheme/port/wildcard)", key)
		}
	case "write":
		if err := runner.ValidateMountSource(rest); err != nil {
			return fmt.Sprintf("requirement %q: invalid write path: %s", key, err.Error())
		}
	}
	if !validRequirementLevel(req.Level) {
		return fmt.Sprintf("requirement %q: level must be \"required\" or \"optional\"", key)
	}
	if !validRequirementProvenance(req.Provenance) {
		return fmt.Sprintf("requirement %q: provenance must be \"scan_seeded\" or \"operator_set\"", key)
	}
	return ""
}

// handleSetWorkspaceRequirements replaces the workspace's requirements
// contract — the scoped write behind PUT /workspaces/{id}/requirements. Body:
// {"requirements": {"<type>:<key>": {"level":"required"|"optional",
// "provenance":"scan_seeded"|"operator_set"}, ...}}. Every key+value pair is
// validated (validateWorkspaceRequirement) before the store write; the map
// itself is capped (maxWorkspaceRequirements). See types.Workspace.Requirements
// for the key grammar and runs_create.go's applyWorkspaceRequirements for the
// load-bearing fold this contract feeds into a run's resolved policy.
func (s *Server) handleSetWorkspaceRequirements(w http.ResponseWriter, r *http.Request) {
	type body struct {
		Requirements map[string]types.WorkspaceRequirement `json:"requirements"`
	}
	scopedWorkspaceWrite(s, w, r, "workspace.requirements.write",
		func(req body) (map[string]types.WorkspaceRequirement, string) {
			if len(req.Requirements) > maxWorkspaceRequirements {
				return nil, fmt.Sprintf("too many requirements (max %d)", maxWorkspaceRequirements)
			}
			for _, key := range sortedKeys(req.Requirements) {
				if msg := validateWorkspaceRequirement(key, req.Requirements[key]); msg != "" {
					return nil, msg
				}
			}
			return req.Requirements, ""
		},
		// Wrapped, not passed as a method value: the store call must not be
		// resolved until validation has passed.
		func(ctx context.Context, id uuid.UUID, reqs map[string]types.WorkspaceRequirement) (types.Workspace, error) {
			return s.cfg.Store.SetWorkspaceRequirements(ctx, id, s.dropSourceContributedScanSeeded(ctx, id, reqs))
		},
		func(reqs map[string]types.WorkspaceRequirement) map[string]any {
			return map[string]any{"count": len(reqs)}
		})
}

// dropSourceContributedScanSeeded strips provenance:"scan_seeded" rows from
// reqs whose key an ATTACHED SOURCE already contributes to this workspace's
// fold — WSPIPE-3's server-side belt. The overlay this endpoint writes is
// meant to carry only the OPERATOR's own edits (setRequirementLane always
// stamps operator_set); a scan_seeded row here can only be the wizard's
// client-side seeding, which FoldWorkspaceContract's rule 6 makes win over
// the SOURCE's own (correctly rescanned) contract forever — a name a rescan
// drops from the source stays stuck in the overlay with no way to remove it.
// Dropped silently, never rejected: the wizard still sends these until its
// own fix lands (source_scan.go's design note), and a dropped row is
// provably redundant — the source already contributes it — never a lost
// operator intent, unlike WSPIPE-8's identity-hit case. Best-effort: a store
// read error leaves reqs untouched (fail OPEN on the belt; the write itself
// must not become unavailable because of it).
func (s *Server) dropSourceContributedScanSeeded(ctx context.Context, id uuid.UUID, reqs map[string]types.WorkspaceRequirement) map[string]types.WorkspaceRequirement {
	hasScanSeeded := false
	for _, req := range reqs {
		if req.Provenance == "scan_seeded" {
			hasScanSeeded = true
			break
		}
	}
	if !hasScanSeeded {
		return reqs
	}
	ws, err := s.cfg.Store.GetWorkspace(ctx, id)
	if err != nil || len(ws.Attachments) == 0 {
		return reqs
	}
	var sourceIDs []uuid.UUID
	for _, att := range ws.Attachments {
		if att.SourceID != nil {
			sourceIDs = append(sourceIDs, *att.SourceID)
		}
	}
	sources, err := s.cfg.Store.GetSourcesByIDs(ctx, sourceIDs)
	if err != nil {
		return reqs
	}
	contributed := map[string]bool{}
	for _, src := range sources {
		for key := range src.Requirements {
			contributed[key] = true
		}
	}
	out := make(map[string]types.WorkspaceRequirement, len(reqs))
	for key, req := range reqs {
		if req.Provenance == "scan_seeded" && contributed[key] {
			continue // redundant: an attached source's OWN contract already carries this
		}
		out[key] = req
	}
	return out
}
