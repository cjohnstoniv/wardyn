// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// configuredExecutors lists the ids of the remote-hosted runners this deployment itself runs: the names
// of its own substrates (never a registered runner), and only those a pool may name.
func (s *Server) configuredExecutors() []string {
	if s.cfg.Runner == nil {
		return nil
	}
	names := []string{s.cfg.Runner.Name()}
	if ex, ok := s.cfg.Runner.(interface{ Executors() []string }); ok {
		names = ex.Executors()
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if types.ValidRemoteRunnerID(n) {
			out = append(out, n)
		}
	}
	return out
}

// organizationPoolName is the name of the organization pool the first boot makes.
const organizationPoolName = "Organization"

// BootstrapRunnerPools is the upgrade step, run at boot before anything is served. Every boot gives each
// configured remote-hosted runner its row (audited remote_runner.create the once it is made). Once per
// database, a deployment that runs remote work through those runners gets one organization pool holding
// them, as the organisation's default pool, so what people could do before they still can, a launch
// that names no pool included. It gives nobody a runner or a launch right they lacked (no use policy is
// created, and no runner is made available to people's pools) and never makes a user pool or touches a
// self-hosted runner; it is a placement change all the same, because a run may then land on any member
// that advertises its class, not the first substrate that does. A catalogue that has ever held an
// organization pool, an administrator's deleted one included, is left as it is.
func (s *Server) BootstrapRunnerPools(ctx context.Context) error {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return nil
	}
	executors := s.configuredExecutors()
	if rs, ok := s.cfg.Store.(store.RemoteRunnerStore); ok {
		made, err := rs.EnsureRemoteRunners(ctx, executors)
		if err != nil {
			return fmt.Errorf("remote runner bootstrap: %w", err)
		}
		for _, rr := range made {
			s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", auditRemoteRunnerCreate, rr.ID, "success",
				mustJSON(map[string]any{"name": rr.Name, "people_pools": rr.PeoplePools, "revision": rr.Revision, "bootstrap": true})))
		}
	}
	p, defaults, err := ps.BootstrapRunnerPools(ctx, executors, organizationPoolName)
	if err != nil {
		return fmt.Errorf("runner pool bootstrap: %w", err)
	}
	if p == nil {
		return nil
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", auditPoolCreate, p.ID.String(), "success",
		mustJSON(map[string]any{"name": p.Name, "scope": p.Scope, "revision": p.Revision, "bootstrap": true, "remote_runner_ids": executors})))
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", auditPoolDefaultSet, "organisation", "success",
		mustJSON(map[string]any{"after": *defaults, "bootstrap": true})))
	return nil
}
