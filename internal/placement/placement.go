// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package placement is the leaf package behind local placement (0.9, OD-8 and
// OD-12): the placement request types, the capacity a runner advertises, the
// refusal vocabulary, the credential classification of every credential-bearing
// SandboxSpec and ProxyConfig field, and the credential delivery policy shape.
// Everything here is pure; store-backed facts are passed in by the callers.
package placement

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// Placement says where a run's sandbox lives.
type Placement string

const (
	Remote Placement = "remote" // the organisation's own executor
	Local  Placement = "local"  // a registered runner on the person's laptop
)

// Request is the placement a caller asked for (#116's shape). The zero value
// asks for no placement, which the resolver fills only when exactly one is
// eligible.
type Request struct {
	Placement Placement `json:"placement,omitempty"`
	RunnerID  string    `json:"runner_id,omitempty"`
}

// ErrInvalidRequest marks a Request that is malformed on its face, before any
// eligibility is looked at.
var ErrInvalidRequest = errors.New("placement: invalid request")

// Validate checks the request's shape: a known placement, and a runner id only
// with local placement, as a UUID.
func (r Request) Validate() error {
	switch r.Placement {
	case "", Remote, Local:
	default:
		return fmt.Errorf("%w: placement %q is neither %q nor %q", ErrInvalidRequest, r.Placement, Remote, Local)
	}
	if r.RunnerID == "" {
		return nil
	}
	if r.Placement != Local {
		return fmt.Errorf("%w: runner_id needs placement %q", ErrInvalidRequest, Local)
	}
	if _, err := uuid.Parse(r.RunnerID); err != nil {
		return fmt.Errorf("%w: runner_id is not a UUID", ErrInvalidRequest)
	}
	return nil
}

// Capacity is what one local run may use on a runner, asserted by the runner.
type Capacity struct {
	CPUMillisMax int64 `json:"cpu_millis_max"`
	MemoryMiBMax int64 `json:"memory_mib_max"`
}

// Fits reports whether a request for cpuMillis and memoryMiB is within the
// advertised capacity; a zero request means the driver's default and always fits.
func (c Capacity) Fits(cpuMillis, memoryMiB int64) bool {
	return (cpuMillis <= 0 || cpuMillis <= c.CPUMillisMax) && (memoryMiB <= 0 || memoryMiB <= c.MemoryMiBMax)
}

// EvidenceSource records who vouches for the evidence a run produces: its
// confinement, decisions, recordings, exit codes and capabilities. It is not
// the run's `confinement_source` (`requested` or `defaulted`, written by
// run.create): that says who chose the class, this says who can attest it.
type EvidenceSource string

const (
	EvidenceSubstrate      EvidenceSource = "substrate"       // the organisation's own substrate
	EvidenceRunnerAsserted EvidenceSource = "runner_asserted" // the runner said so; nobody can verify it
)

// EvidenceFor is the evidence source a placement records.
func EvidenceFor(p Placement) EvidenceSource {
	if p == Local {
		return EvidenceRunnerAsserted
	}
	return EvidenceSubstrate
}

// RunnerSubstrateName is the name an Orchestrator registers a runner's
// substrate under, and the prefix of every ref it creates.
func RunnerSubstrateName(runnerID string) string { return "runner:" + runnerID }

// RunnerRefPrefix prefixes every ref a runner returns, so ref routing never
// falls back to the sole substrate for one.
func RunnerRefPrefix(runnerID string) string { return RunnerSubstrateName(runnerID) + "/" }
