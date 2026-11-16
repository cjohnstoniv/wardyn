// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

type launchWorkloadKey struct{}

// withLaunchWorkload marks ctx as wl's launch, so the run it creates is minted under wl's SPIFFE ID
// (identity.RunPath), and so is a queued one when it starts (queueAuthority.WorkloadOwner).
func withLaunchWorkload(ctx context.Context, wl store.Workload) context.Context {
	return context.WithValue(ctx, launchWorkloadKey{}, identity.Workload{Owner: wl.Owner, Name: wl.Name})
}

// launchWorkloadFrom is the workload ctx launches for, zero for any other launch.
func launchWorkloadFrom(ctx context.Context) identity.Workload {
	w, _ := ctx.Value(launchWorkloadKey{}).(identity.Workload)
	return w
}

// workload is the workload the queued launch was for (captureQueueAuthority), zero for any other.
func (a queueAuthority) workload() identity.Workload {
	return identity.Workload{Owner: a.WorkloadOwner, Name: a.WorkloadName}
}

// errNoWorkloadMint is a workload's run on a provider that cannot name it: the run is refused, never
// minted under the ad-hoc ID a relying party would not admit as the workload's.
var errNoWorkloadMint = errors.New("the identity provider cannot name a workload's run")

// mintRunIdentity mints runID's run token for the internal audience: under w's SPIFFE ID for a run a
// workload launched, and the ad-hoc one when w is zero. Every mint of a workload's or an ad-hoc run's own
// identity goes through it, so a run keeps the ID its first mint gave it; a workspace step's helper run
// (workspace_run_launch.go) mints the ad-hoc form directly, and is never a workload's.
func (s *Server) mintRunIdentity(ctx context.Context, runID uuid.UUID, w identity.Workload, humanSub, sponsor string, operatorOwned bool) (identity.RunIdentity, error) {
	if w == (identity.Workload{}) {
		return s.cfg.Identity.MintRunIdentity(ctx, runID, humanSub, sponsor, internalAudience, operatorOwned)
	}
	m, ok := s.cfg.Identity.(identity.WorkloadRunMinter)
	if !ok {
		return identity.RunIdentity{}, errNoWorkloadMint
	}
	return m.MintWorkloadRunIdentity(ctx, runID, w, humanSub, sponsor, internalAudience, operatorOwned)
}

// runWorkloadOf is the workload a run's SPIFFE ID names, read back so a revive or a renewal mints the
// run under the ID it started with; zero when the ID is the ad-hoc form, names another run, or is not a
// run's.
func runWorkloadOf(spiffeID string, runID uuid.UUID) identity.Workload {
	id, err := spiffeid.FromString(spiffeID)
	if err != nil {
		return identity.Workload{}
	}
	w, run, err := identity.ParseRunPath(id.Path())
	if err != nil || run != runID {
		return identity.Workload{}
	}
	return w
}
