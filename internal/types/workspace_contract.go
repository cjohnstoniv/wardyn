// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// workspace_contract.go holds the three-tier split's shared grammar and the
// one pure fold that turns it into a run contract: Tier 1 Source (repo/dir
// configured once, its own contract + scan); Tier 2 BaseImageEntry (shared
// catalog image; "recommended" is per-workspace derived, not a catalog
// kind); Tier 3 Workspace (attachments of sources + inline ephemerals + a
// base image + its own requirement rows). Lives in types, not api, since the
// store must fold contracts at its hydrate pass and can't import api.

// SourceKind is a library source's kind. Ephemeral scratch isn't a kind
// here: it's a per-workspace inline attachment, never a library entity.
type SourceKind string

const (
	SourceLocalDir SourceKind = "local_dir"
	SourceRepo     SourceKind = "repo"
)

// Source is one tier-1 library entry: a repo or dir configured once and
// attached to many workspaces. Requirements is what it expects of any
// sandbox using it, in Workspace.Requirements's "<type>:<key>" grammar,
// minus integration: keys (those compose only at the tier-3 aggregate).
type Source struct {
	ID   uuid.UUID  `json:"id"`
	Kind SourceKind `json:"kind"`
	// Locator is the identity: a host dir path (local_dir, trimmed) or
	// canonical repo slug/clone URL (repo, lowercased); with Ref, unique in the store.
	Locator string `json:"locator"`
	// Ref is an optional git ref (repo only); part of identity, so the same repo at two refs is two sources with two contracts.
	Ref string `json:"ref,omitempty"`
	// RunnerID is the person's runner a local_dir folder lives on (nil = the
	// control plane's host); part of identity, so one path on two machines is
	// two sources.
	RunnerID     *uuid.UUID                      `json:"runner_id,omitempty"`
	Name         string                          `json:"name"`
	Requirements map[string]WorkspaceRequirement `json:"requirements,omitempty"` // this source's own contract; see the type doc above
	// Profile is this source's scan result, opaque here (never interpreted, only
	// persisted/returned). json.RawMessage, not []byte: a bare []byte gets
	// base64-encoded by encoding/json (WIRE-2), shipping it to GET /sources as
	// an opaque string instead of real JSON.
	Profile     json.RawMessage `json:"profile,omitempty"`
	Status      WorkspaceStatus `json:"status"`                  // scan lifecycle: pending_scan | scanning | scanned | error
	ActiveRunID *uuid.UUID      `json:"active_run_id,omitempty"` // fences this source's in-flight scan run
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// BaseImageEntry is one tier-2 catalog row: a shared, reusable image an
// operator saved. Kind is registry|custom|byo|prewrapped, never "recommended" (a
// derived per-workspace build with no catalog identity, enforced by a store CHECK).
type BaseImageEntry struct {
	ID    uuid.UUID `json:"id"`
	Kind  string    `json:"kind"` // "registry" | "custom" | "byo" | "prewrapped"
	Name  string    `json:"name"`
	Image string    `json:"image"` // the ref: the image itself (registry/byo/prewrapped) or the FROM (custom)
	// Steps are Dockerfile lines on a "custom" Image; not applied at build
	// time (host-side build RCE — see WorkspaceBaseImage.Steps), stored only
	// so the row round-trips.
	Steps     []string  `json:"steps,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Check is a prewrapped row's registration check, nil on every other kind. Only the server writes it.
	Check *BaseImageCheck `json:"check,omitempty"`
}

// BaseImageKindPrewrapped is an image built FROM agent-base and pinned by digest, which runs as is with no
// image builder once its registration check on the cluster has passed.
const BaseImageKindPrewrapped = "prewrapped"

// A prewrapped row's check_status. Only BaseImageCheckReady is offered to a workspace or a run.
const (
	BaseImageCheckChecking = "checking"
	BaseImageCheckReady    = "ready"
	BaseImageCheckRefused  = "refused"
)

// BaseImageCheck is a prewrapped row's registration check (migration 0237).
type BaseImageCheck struct {
	// Status is checking, ready or refused. A checking row whose Deadline has passed is served as refused
	// with Reason "check_interrupted".
	Status string `json:"status"`
	// Attempt is the running (or last) attempt; only its verdict is written.
	Attempt  uuid.UUID `json:"-"`
	Deadline time.Time `json:"-"`
	// Reason is "<code>:<arch>[:<detail>]": on a refused row what refused it, e.g. "contract_missing:arm64:wardyn-rec";
	// on a ready row an architecture left out and why, e.g. "platform_missing:arm64"; "" otherwise.
	Reason string `json:"reason,omitempty"`
	// AcceptedArches are the node architectures the check passed on, e.g. ["amd64", "arm64"].
	AcceptedArches []string   `json:"accepted_arches,omitempty"`
	CheckedAt      *time.Time `json:"checked_at,omitempty"`
}

// ImageContractLine is the one line `wardyn-rec -contract` prints. A pre-wrapped image's registration check
// runs it on each architecture and accepts only this exact line with exit 0; the number is the image
// contract's version (deploy/images/README.md, Image contract).
const ImageContractLine = "wardyn image contract 1"

// ImageContractTools are the runner tools an image must carry in /usr/local/bin for the runner to exec,
// record, verify and broker git into it: the wrap COPYs them in (envbuild's requiredTools), and a prewrapped
// image's registration check looks for them.
var ImageContractTools = []string{
	"agent-run",         // task entrypoint the runner execs
	"agent-run-lib.sh",  // sourced by agent-run under `set -euo pipefail` (load-bearing)
	"wardyn-rec",        // session recorder; required by the image --selftest
	"wardyn-git-helper", // brokered-token git credential helper
}

// AttachmentOverride values: a workspace's per-attachment stance on one requirement key its source declares.
const (
	OverrideOff      = "off"      // this workspace refuses the requirement outright
	OverrideOptional = "optional" // re-lane: off by default, a run may enable it
	OverrideRequired = "required" // re-lane: rides every run
)

// WorkspaceAttachment is one tier-3 composition row: either a library source
// (SourceID set) or an inline ephemeral row (Ephemeral true, SourceID nil);
// order is load-bearing, attachments[0] is the primary.
type WorkspaceAttachment struct {
	SourceID  *uuid.UUID `json:"source_id,omitempty"`
	Ephemeral bool       `json:"ephemeral,omitempty"` // marks an inline scratch row (never a library entity)
	Target    string     `json:"target,omitempty"`    // in-sandbox mount/clone/scratch path for this attachment
	Writable  bool       `json:"writable,omitempty"`  // opts a local_dir attachment into read-write, per-attachment not per-source
	// Overrides is this workspace's stance on requirement keys the attached
	// source declares: reqKey -> off|optional|required. Never edits the
	// shared source; keys the source doesn't declare are ignored harmlessly.
	Overrides map[string]string `json:"overrides,omitempty"`
}

// SplitRequirementKey splits a "<type>:<key>" requirement key on the first
// colon only (a write:<path> key may itself contain colons) and reports
// whether the type token is known.
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

// FoldWorkspaceContract computes a workspace's effective requirements (the
// map applyWorkspaceRequirements consumes unchanged) from its attachments,
// the attached sources' own contracts, and the workspace's overlay rows.
// Precedence, in order:
//  1. Each attachment contributes its source's contract; a dangling SourceID contributes nothing.
//  2. An override of "off" drops that source's contribution for that key.
//  3. An override lane (optional|required) replaces the source's lane.
//  4. SECURITY: a write:<path> key whose path isn't the source's own Locator
//     is dropped, else a shared source could claim write on a path it
//     doesn't own and widen a sibling mount in every attaching workspace.
//  5. Collisions merge: Level = strongest, Provenance = weakest — fail-closed
//     since the run-create trust boundary auto-mints only operator_set
//     secrets (ponytail: overlay is the one-row override if too strict).
//  6. An overlay row replaces the merged value outright; it can't remove a key ("off" is for that).
//
// Pure: no store, no clock. With no attachments (or all-ephemeral), the
// result is exactly the overlay.
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
		// Sorted for a deterministic order, so goldens stay byte-exact even if
		// mergeRequirement's commutativity ever stops holding.
		for _, key := range slices.Sorted(maps.Keys(src.Requirements)) {
			row := src.Requirements[key]
			switch att.Overrides[key] {
			case OverrideOff:
				continue // rule 2
			case OverrideOptional:
				row.Level = "optional" // rule 3
			case OverrideRequired:
				row.Level = "required" // rule 3
			}
			// Rule 4: write ownership, enforced only on well-formed write keys.
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

	for key, row := range overlay { // rule 6: overlay replaces outright
		out[key] = row
	}
	if len(out) == 0 {
		return nil // fold(nil-everything) == nil, not empty, to keep JSON round-trips (omitempty) exact
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
