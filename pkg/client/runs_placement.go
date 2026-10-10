// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import "github.com/cjohnstoniv/wardyn/internal/placement"

// The placement a create-run request carries (0.9, OD-8). CreateRunRequest.Placement
// and .RunnerID are the wire carrier; these are the vocabulary it is expressed in,
// aliased from internal/placement so a caller needs neither that package nor a
// second definition of the values.

// Placement says where a run's sandbox lives: PlacementRemote, the
// organisation's own executor, or PlacementLocal, a runner the person
// registered. The console calls the field "Runs on".
type Placement = placement.Placement

// The placements CreateRunRequest.Placement accepts.
const (
	PlacementRemote = placement.Remote
	PlacementLocal  = placement.Local
)

// PlacementRequest is the placement a request asks for, in the shape the
// placement package validates and resolves.
func (r CreateRunRequest) PlacementRequest() placement.Request {
	return placement.Request{Placement: r.Placement, RunnerID: r.RunnerID}
}
