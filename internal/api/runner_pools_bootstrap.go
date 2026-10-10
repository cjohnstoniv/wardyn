// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// configuredExecutors lists the stable IDs of the executors this deployment itself runs: the names of
// its own substrates (never a registered runner), and only those a remote-provided pool may name.
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
		if (types.RunnerPoolMember{ExecutorID: n}).Validate(types.RunnerPoolRemoteProvided) == nil {
			out = append(out, n)
		}
	}
	return out
}

// BootstrapRunnerPools is the one-time upgrade step, run at boot before anything is served: a
// deployment that already runs remote work through its configured executors gets one Remote Provided
// pool holding them, as the organisation's remote default, so what people could do before they still
// can. It widens nothing (no use policy is created, and nobody gains a launch right) and never makes
// a Self-Hosted pool or touches a runner. It runs once per database: a catalogue that has ever held a
// pool, an administrator's deleted one included, is left as it is.
func (s *Server) BootstrapRunnerPools(ctx context.Context) error {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return nil
	}
	executors := s.configuredExecutors()
	p, err := ps.BootstrapRunnerPools(ctx, executors, runnerpool.HostingLabel(types.RunnerPoolRemoteProvided))
	if err != nil {
		return fmt.Errorf("runner pool bootstrap: %w", err)
	}
	if p == nil {
		return nil
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", auditPoolCreate, p.ID.String(), "success",
		mustJSON(map[string]any{"name": p.Name, "hosting_type": p.HostingType, "revision": p.Revision, "bootstrap": true, "executors": executors})))
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", auditPoolDefaultSet, "organisation", "success",
		mustJSON(map[string]any{"after": types.RunnerPoolDefaults{RemoteProvided: &p.ID}, "bootstrap": true})))
	return nil
}
