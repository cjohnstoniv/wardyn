// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// The run placement vocabulary (0.9, OD-8). These two scalar types live here, not
// in internal/placement, because a run row carries them: internal/placement
// already imports internal/types, so the canonical declaration has to be the
// inner package. placement.Placement and placement.EvidenceSource are aliases of
// these, and its Remote/Local/EvidenceSubstrate/EvidenceRunnerAsserted constants
// keep their own names and values.

// Placement says where a run's sandbox lives.
type Placement string

const (
	PlacementRemote Placement = "remote" // the organisation's own executor
	PlacementLocal  Placement = "local"  // a registered runner on the person's laptop
)

// RunEvidenceSource records who vouches for the evidence a run produces: its
// confinement, decisions, recordings, exit codes and capabilities. It is not the
// run's `confinement_source` (`requested` or `defaulted`, written by run.create):
// that says who chose the class, this says who can attest it.
type RunEvidenceSource string

const (
	RunEvidenceSubstrate      RunEvidenceSource = "substrate"       // the organisation's own substrate
	RunEvidenceRunnerAsserted RunEvidenceSource = "runner_asserted" // the runner said so; nobody can verify it
)
