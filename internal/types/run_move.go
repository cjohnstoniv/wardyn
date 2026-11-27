// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MoveState is where one move of a run stands (run_moves.state).
type MoveState string

const (
	// MoveAdmitted: the move was admitted; nothing has been stopped or carried yet.
	MoveAdmitted MoveState = "admitted"
	// MoveExporting: the source's carried files are being read.
	MoveExporting MoveState = "exporting"
	// MoveImporting: the destination's sandbox is being created and filled.
	MoveImporting MoveState = "importing"
	// MoveCommitted: the run row names the destination. The move still holds the run until it is released.
	MoveCommitted MoveState = "committed"
	// MoveFailed: the move ended before commit after the run was stopped, or before its destination was torn down.
	MoveFailed MoveState = "failed"
	// MoveRefused: the move was refused before any destination existed. It never held the run.
	MoveRefused MoveState = "refused"
)

// InFlight reports a state that holds the run while the move's executor is alive: admitted, exporting or
// importing. A committed move also holds the run until it is released (RunMove.ReleasedAt).
func (s MoveState) InFlight() bool {
	return s == MoveAdmitted || s == MoveExporting || s == MoveImporting
}

// MoveCleanup is what is left to tear down after a move (run_moves.cleanup).
type MoveCleanup string

const (
	MoveCleanupNone    MoveCleanup = "none"
	MoveCleanupPending MoveCleanup = "pending"
	MoveCleanupDone    MoveCleanup = "done"
)

// MoveSourceRevoke is the state of the second revoke of the source's credentials after commit
// (run_moves.source_revoke).
type MoveSourceRevoke string

const (
	MoveSourceRevokeNone    MoveSourceRevoke = "none"
	MoveSourceRevokePending MoveSourceRevoke = "pending"
	MoveSourceRevokeDone    MoveSourceRevoke = "done"
)

// RunMove is one move of a run (a run_moves row). The From and To fields are where the run was and where it is
// moving to; To's runner, pool and sandbox are set as the move learns them. Failure holds the reason of a move
// that was refused or failed, and Narrowed the policy narrowing the destination applied. Nothing here is a
// credential: ToTokenJTI is the id of the destination's create-time token, never the token.
type RunMove struct {
	ID       uuid.UUID `json:"id"`
	RunID    uuid.UUID `json:"run_id"`
	State    MoveState `json:"state"`
	PauseGen int64     `json:"pause_gen"`

	FromPlacement      Placement         `json:"from_placement"`
	FromRunnerID       *uuid.UUID        `json:"from_runner_id,omitempty"`
	FromPoolID         *uuid.UUID        `json:"from_pool_id,omitempty"`
	FromExecutor       string            `json:"from_executor,omitempty"`
	FromSandboxRef     string            `json:"-"`
	FromEvidenceSource RunEvidenceSource `json:"from_evidence_source,omitempty"`
	FromTrustClass     RunnerTrustClass  `json:"from_trust_class,omitempty"`

	ToPlacement      Placement         `json:"to_placement"`
	ToRunnerID       *uuid.UUID        `json:"to_runner_id,omitempty"`
	ToPoolID         *uuid.UUID        `json:"to_pool_id,omitempty"`
	ToExecutor       string            `json:"to_executor,omitempty"`
	ToSandboxRef     string            `json:"-"`
	ToEvidenceSource RunEvidenceSource `json:"to_evidence_source,omitempty"`
	ToTrustClass     RunnerTrustClass  `json:"to_trust_class,omitempty"`
	ToTokenJTI       string            `json:"-"`

	Cleanup        MoveCleanup      `json:"cleanup"`
	SourceRevoke   MoveSourceRevoke `json:"source_revoke"`
	RecordingLevel RecordingLevel   `json:"recording_level,omitempty"`
	MovedBy        string           `json:"moved_by,omitempty"`
	ManifestSHA256 string           `json:"manifest_sha256,omitempty"`
	Bytes          int64            `json:"bytes,omitempty"`
	Skipped        int              `json:"skipped,omitempty"`
	Narrowed       json.RawMessage  `json:"narrowed,omitempty"`
	Failure        string           `json:"failure,omitempty"`

	HeartbeatAt *time.Time `json:"heartbeat_at,omitempty"`
	ReleasedAt  *time.Time `json:"released_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CommittedAt *time.Time `json:"committed_at,omitempty"`
}

// PlacementCause is why a run_placements row was written.
type PlacementCause string

const (
	// PlacementCauseOrigin is the placement the run had before its first move.
	PlacementCauseOrigin PlacementCause = "origin"
	// PlacementCauseMove is a placement a move gave the run.
	PlacementCauseMove PlacementCause = "move"
)

// RunPlacement is one entry of a moved run's placement history (a run_placements row). Seq 1 is the run's
// placement before its first move; each later entry is the placement a move gave it. A run that never moved has
// no entries: its one placement is on the run row.
type RunPlacement struct {
	RunID          uuid.UUID         `json:"run_id"`
	Seq            int               `json:"seq"`
	Cause          PlacementCause    `json:"cause"`
	Placement      Placement         `json:"placement"`
	RunnerID       *uuid.UUID        `json:"runner_id,omitempty"`
	PoolID         *uuid.UUID        `json:"pool_id,omitempty"`
	Executor       string            `json:"executor,omitempty"`
	EvidenceSource RunEvidenceSource `json:"evidence_source,omitempty"`
	TrustClass     RunnerTrustClass  `json:"trust_class,omitempty"`
	RecordingLevel RecordingLevel    `json:"recording_level,omitempty"`
	MoveID         *uuid.UUID        `json:"move_id,omitempty"`
	StartedAt      time.Time         `json:"started_at"`
}

// MoveDestinationKind is the form of a move's "to".
type MoveDestinationKind string

const (
	// MoveToRunner names one runner by id: "runner:<id>".
	MoveToRunner MoveDestinationKind = "runner"
	// MoveToPool names a runner pool by id: "pool:<id>".
	MoveToPool MoveDestinationKind = "pool"
	// MoveToLaptop is the run owner's own runner: "laptop".
	MoveToLaptop MoveDestinationKind = "laptop"
	// MoveToOrg is the organisation's own executors: "org".
	MoveToOrg MoveDestinationKind = "org"
	// MoveToSite is a site by name: "site:<name>".
	MoveToSite MoveDestinationKind = "site"
)

// MoveDestination is where a move was asked to go, as POST /runs/{id}/move's "to" names it. It is resolved for
// the run's owner, never for the caller.
type MoveDestination struct {
	Kind MoveDestinationKind `json:"kind"`
	// Ref is the runner or pool id, or the site name; empty for laptop and org.
	Ref string `json:"ref,omitempty"`
}

// String is the destination in the "to" form it was parsed from.
func (d MoveDestination) String() string {
	if d.Ref == "" {
		return string(d.Kind)
	}
	return string(d.Kind) + ":" + d.Ref
}

// ParseMoveDestination reads a move's "to": runner:<id>, pool:<id>, laptop, org or site:<name>.
func ParseMoveDestination(to string) (MoveDestination, error) {
	kind, ref, hasRef := strings.Cut(strings.TrimSpace(to), ":")
	d := MoveDestination{Kind: MoveDestinationKind(kind), Ref: ref}
	switch d.Kind {
	case MoveToLaptop, MoveToOrg:
		if hasRef {
			return MoveDestination{}, fmt.Errorf("%q takes nothing after it", kind)
		}
		return d, nil
	case MoveToRunner, MoveToPool:
		if _, err := uuid.Parse(ref); err != nil {
			return MoveDestination{}, fmt.Errorf("%s:<id> needs a %s id", kind, kind)
		}
		return d, nil
	case MoveToSite:
		if ref == "" {
			return MoveDestination{}, fmt.Errorf("site:<name> needs a site name")
		}
		return d, nil
	}
	return MoveDestination{}, fmt.Errorf("unknown destination %q: use runner:<id>, pool:<id>, laptop, org or site:<name>", to)
}
