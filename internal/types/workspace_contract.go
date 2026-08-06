// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// workspace_contract.go — the three-tier split's shared grammar and the ONE
// pure fold that turns it into a run contract.
//
// Tier 1: Source — a repo/dir configured ONCE, in a shared library: its own
// requirements contract, its own scan. Tier 2: BaseImageEntry — a shared
// catalog image (registry/custom/byo; "recommended" is per-workspace derived
// and deliberately NOT a catalog kind). Tier 3: Workspace — attachments of
// sources (+ inline ephemerals) + a base image + its own requirement rows.
//
// This file lives in types, not api, because the STORE must fold contracts at
// its hydrate pass and store cannot import api.

// SourceKind is a library source's kind. Ephemeral scratch is NOT a kind here:
// an ephemeral row is a per-workspace inline attachment (nothing to configure,
// scan, or share), never a library entity.
type SourceKind string

const (
	SourceLocalDir SourceKind = "local_dir"
	SourceRepo     SourceKind = "repo"
)

// Source is one tier-1 library entry: a repo or directory configured once and
// attached to many workspaces. Its Requirements are what THIS source expects
// of any sandbox it is used in ("a given repo has its expected requirements") —
// the same "<type>:<key>" grammar Workspace.Requirements documents, MINUS
// integration: keys (integrations compose at the aggregate, tier 3, by owner
// decision; the fold would pass them through unharmed, so relaxing later is a
// validation change only).
type Source struct {
	ID   uuid.UUID  `json:"id"`
	Kind SourceKind `json:"kind"`
	// Locator is the identity: a host directory path (local_dir, trailing
	// slashes trimmed) or the canonical repo slug/clone URL (repo, lowercased).
	// Together with Ref it is UNIQUE in the store — the dedupe rule.
	Locator string `json:"locator"`
	// Ref is an optional git ref (repo only; "" otherwise). Part of identity:
	// the same repo at two refs is two sources with two contracts.
	Ref  string `json:"ref,omitempty"`
	Name string `json:"name"`
	// Requirements is this source's OWN contract. See the type doc above.
	Requirements map[string]WorkspaceRequirement `json:"requirements,omitempty"`
	// Profile is this source's scan result (workspacescan.WorkspaceProfile).
	Profile []byte `json:"profile,omitempty"`
	// Status is the source's scan lifecycle — the same one-word states the
	// workspace used to own: pending_scan | scanning | scanned | error.
	Status WorkspaceStatus `json:"status"`
	// ActiveRunID fences this source's in-flight scan run, exactly as the
	// workspace's own field fenced whole-workspace scans before the retarget.
	ActiveRunID *uuid.UUID `json:"active_run_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// BaseImageEntry is one tier-2 catalog row: a shared, reusable image an
// operator saved. Kind is registry|custom|byo — NEVER "recommended", which is
// a per-workspace DERIVED build (computed from that workspace's merged source
// profiles) and so has no catalog identity; the store enforces this with a
// CHECK constraint so it is structural, not a convention.
type BaseImageEntry struct {
	ID   uuid.UUID `json:"id"`
	Kind string    `json:"kind"` // "registry" | "custom" | "byo"
	Name string    `json:"name"`
	// Image is the ref: the image itself (registry/byo) or the FROM (custom).
	Image string `json:"image"`
	// Steps are Dockerfile lines layered on Image (custom only).
	Steps     []string  `json:"steps,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AttachmentOverride values — a workspace's per-attachment stance on ONE
// requirement key its attached source declares.
const (
	OverrideOff      = "off"      // this workspace refuses the requirement outright
	OverrideOptional = "optional" // re-lane: off by default, a run may enable it
	OverrideRequired = "required" // re-lane: rides every run
)

// WorkspaceAttachment is one tier-3 composition row: EITHER a library source
// reference (SourceID set) or an inline ephemeral scratch row (Ephemeral true,
// SourceID nil). Order is load-bearing — attachments[0] is the primary, the
// same rule Sources[0] carried before the split.
type WorkspaceAttachment struct {
	SourceID *uuid.UUID `json:"source_id,omitempty"`
	// Ephemeral marks an inline scratch row (never a library entity).
	Ephemeral bool `json:"ephemeral,omitempty"`
	// Target is the in-sandbox mount/clone/scratch path for THIS attachment.
	Target string `json:"target,omitempty"`
	// Writable opts a local_dir attachment into read-write (per-attachment,
	// not per-source: the same dir can be writable in one workspace and
	// read-only in another).
	Writable bool `json:"writable,omitempty"`
	// Overrides is this workspace's stance on requirement keys the attached
	// source declares: reqKey -> off|optional|required. "off" means THIS
	// workspace refuses that requirement (mount the repo read-only to read
	// code, without its build secrets/egress) — it never edits the shared
	// source. Keys the source doesn't declare are ignored harmlessly.
	Overrides map[string]string `json:"overrides,omitempty"`
}

// SplitRequirementKey splits a "<type>:<key>" requirement key on the FIRST
// colon only (a write:<path> key may itself legally contain colons) and
// reports whether the type token is known. This is the grammar's own parser,
// living beside the grammar; internal/api aliases it for its call sites.
func SplitRequirementKey(key string) (typ, rest string, ok bool) {
	typ, rest, found := strings.Cut(key, ":")
	if !found || rest == "" {
		return "", "", false
	}
	switch typ {
	case "secret", "egress", "write", "integration":
		return typ, rest, true
	default:
		return "", "", false
	}
}

// FoldWorkspaceContract computes a workspace's EFFECTIVE requirements — the
// map applyWorkspaceRequirements consumes unchanged — from its attachments,
// the attached sources' own contracts, and the workspace's overlay rows.
//
// Precedence, evaluated in order:
//
//  1. Each attachment in order contributes its source's contract. A dangling
//     SourceID (source deleted out from under it) contributes nothing — the
//     mount gate is where that failure surfaces loudly, not here.
//  2. An override of "off" drops that source's contribution for that key.
//  3. An override lane (optional|required) replaces the source's lane for
//     that contribution.
//  4. SECURITY: a write:<path> key whose path is not the source's own Locator
//     is DROPPED. applyWriteNarrowing widens ANY mount whose source path
//     matches, so without this a shared source could declare write on a path
//     it doesn't own and silently widen a sibling mount in every workspace
//     that attaches it. A source may only claim write on itself.
//  5. Collisions across attachments merge: Level = strongest (required beats
//     optional); Provenance = WEAKEST (scan_seeded beats operator_set).
//     Fail-closed on purpose: the run-create trust boundary auto-mints only
//     operator_set secrets, so if ANY contributor's row came from reading
//     untrusted repo content, the merged row must not auto-grant. A source's
//     own operator_set row alone still auto-grants — an operator editing a
//     shared source's contract is a direct operator act.
//     ponytail: fail-closed on provenance collision; the workspace overlay is
//     the one-row override if this is ever too strict.
//  6. An overlay row REPLACES the merged value outright (level AND
//     provenance). The overlay is what this workspace additionally declares
//     or restates; overrides are how it refuses or re-lanes a source's row.
//     The overlay deliberately cannot REMOVE a key — "off" is for that.
//
// Pure: no store, no clock. With no attachments (or all-ephemeral), the
// result is exactly the overlay — the identity that makes migration 0031
// provably behavior-identical for every pre-split workspace.
func FoldWorkspaceContract(
	attachments []WorkspaceAttachment,
	sources map[uuid.UUID]Source,
	overlay map[string]WorkspaceRequirement,
) map[string]WorkspaceRequirement {
	out := make(map[string]WorkspaceRequirement, len(overlay)+8)

	for _, att := range attachments {
		if att.SourceID == nil {
			continue // ephemeral, or a malformed row: nothing to contribute
		}
		src, found := sources[*att.SourceID]
		if !found {
			continue // rule 1: dangling reference contributes nothing
		}
		for _, key := range sortedRequirementKeys(src.Requirements) {
			row := src.Requirements[key]
			switch att.Overrides[key] {
			case OverrideOff:
				continue // rule 2
			case OverrideOptional:
				row.Level = "optional" // rule 3
			case OverrideRequired:
				row.Level = "required" // rule 3
			}
			// Rule 4: write ownership. Only enforce on well-formed write keys;
			// a malformed key never got past validation anyway.
			if typ, path, ok := SplitRequirementKey(key); ok && typ == "write" && path != src.Locator {
				continue
			}
			if have, seen := out[key]; seen {
				out[key] = mergeRequirement(have, row) // rule 5
			} else {
				out[key] = row
			}
		}
	}

	// Rule 6: overlay replaces outright.
	for key, row := range overlay {
		out[key] = row
	}
	if len(out) == 0 {
		return nil // fold(nil-everything) == nil, not an empty map — keeps
		// JSON round-trips (omitempty) and the identity golden byte-exact.
	}
	return out
}

// mergeRequirement is rule 5: strongest level, weakest provenance.
func mergeRequirement(a, b WorkspaceRequirement) WorkspaceRequirement {
	out := a
	if a.Level != "required" && b.Level == "required" {
		out.Level = "required"
	}
	if a.Provenance != "scan_seeded" && b.Provenance == "scan_seeded" {
		out.Provenance = "scan_seeded"
	}
	return out
}

// sortedRequirementKeys gives the fold a deterministic contribution order so
// equal inputs always produce byte-equal marshalled output (the goldens
// compare bytes).
func sortedRequirementKeys(m map[string]WorkspaceRequirement) []string {
	return slices.Sorted(maps.Keys(m))
}
