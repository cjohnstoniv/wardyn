// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/runner"
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

// measureRemote is what one configured remote-hosted runner has, all zero when its substrate cannot
// say or the read fails. why is the read the substrate could not make, empty when it made them all.
func (s *Server) measureRemote(ctx context.Context, id string) (m types.RunnerAmounts, why string) {
	var err error
	switch r := s.cfg.Runner.(type) {
	case runner.ExecutorMeasurer:
		m, err = r.MeasureExecutor(ctx, id)
	case runner.Measurer:
		if s.cfg.Runner.Name() == id {
			m, err = r.Measure(ctx)
		}
	}
	if err != nil {
		slog.WarnContext(ctx, "wardynd: measure a remote-hosted runner", slog.String("remote_runner_id", id), slog.Any("err", err))
		return m, err.Error()
	}
	return m, ""
}

// remoteRunnerProposal is the limit set boot proposes for a remote-hosted runner that measured as m:
// the setup proposal a self-hosted runner gets (half of each as the total, per run the total divided
// by Runs at once), with Runs at once half its CPUs. ok is false when any amount could not be
// measured: nothing is guessed, and an administrator sets them.
func remoteRunnerProposal(m types.RunnerAmounts) (types.RunnerLimits, bool) {
	if m.CPUMillis <= 0 || m.MemoryMiB <= 0 || m.DiskMiB <= 0 {
		return types.RunnerLimits{}, false
	}
	runs := min(max(1, int(m.CPUMillis/1000)/2), types.RemoteRunnerMaxRunsCeiling)
	l := types.ProposeRunnerLimits(m, runs)
	return l, l.Validate(types.RemoteRunnerMaxRunsCeiling) == nil
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
		proposals := map[string]types.RunnerLimits{}
		for _, id := range executors {
			m, why := s.measureRemote(ctx, id)
			if p, ok := remoteRunnerProposal(m); ok {
				proposals[id] = p
				continue
			}
			slog.WarnContext(ctx, "wardynd: not every limit of a remote-hosted runner could be measured; one with no limits set takes no run until an administrator sets them",
				slog.String("remote_runner_id", id), slog.String("why", why), slog.Int64("cpu_millis", m.CPUMillis), slog.Int64("memory_mib", m.MemoryMiB), slog.Int64("disk_mib", m.DiskMiB))
		}
		made, proposed, err := rs.EnsureRemoteRunners(ctx, executors, proposals)
		if err != nil {
			return fmt.Errorf("remote runner bootstrap: %w", err)
		}
		for _, rr := range made {
			s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", auditRemoteRunnerCreate, rr.ID, "success",
				mustJSON(map[string]any{"name": rr.Name, "people_pools": rr.PeoplePools, "revision": rr.Revision, "limits": rr.Limits, "bootstrap": true})))
		}
		for _, rr := range proposed {
			s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", auditRemoteRunnerUpdate, rr.ID, "success",
				mustJSON(map[string]any{"limits_before": nil, "limits": rr.Limits, "revision": rr.Revision, "bootstrap": true})))
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
