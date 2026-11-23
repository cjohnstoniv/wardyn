// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runPolicyEvidence is the policy view's reading of a run's first audit rows and the create facts in them.
type runPolicyEvidence struct {
	Events []types.AuditEvent // the run's first effectivePolicyAuditScan rows
	Create createFacts
}

// readRunPolicyEvidence reads a run's first audit rows and its create facts.
func (s *Server) readRunPolicyEvidence(ctx context.Context, runID uuid.UUID) (runPolicyEvidence, error) {
	events, err := s.cfg.Store.QueryAuditEvents(ctx, runID, effectivePolicyAuditScan)
	if err != nil {
		return runPolicyEvidence{}, err
	}
	return runPolicyEvidence{Events: events, Create: runCreateFacts(events)}, nil
}

// runPolicyServed reads the run's restarts and returns the evidence the explain causes read and the spec the doors
// serve: the enforced envelope's, with the run row's data_out rule and the restart hosts denied.
func (s *Server) runPolicyServed(ctx context.Context, run types.AgentRun, pe runPolicyEvidence, envelope effectivePolicyDatum) (auditEvidence, types.RunPolicySpec, error) {
	revives, err := s.runReviveEvents(ctx, run.ID)
	if err != nil {
		return auditEvidence{}, types.RunPolicySpec{}, err
	}
	create := pe.Create
	ev := collectEvidence(pe.Events, revives)
	ev.diskFilled, ev.diskBasis, ev.diskDefaultSource = envelope.DiskMiBFilled, envelope.DiskMiBBasis, envelope.DiskMiBDefaultSource
	ev.bounded = create.Source != nil && create.Source.Bounded
	if create.Source != nil {
		ev.clamp, ev.overlay, ev.walls = create.ClampWarnings, create.Source.Overlay, create.Source.PersonalWalls
		if p := create.Source.WallPreset; p != nil {
			ev.preset = p.WallsSourceRecord
		}
	} else {
		ev.legacyGitBroker = s.holdsGitHubGrant(ctx, run.ID)
	}

	served := envelope.RunPolicySpec
	// data_out as the doors read it: the run row's rule when it keeps one (runDataOutRule).
	if run.DataOut != nil && *run.DataOut != served.DataOut.DataOut() {
		served.DataOut = *run.DataOut
	}
	for _, h := range ev.restartHosts {
		if !slices.Contains(served.DeniedDomains, h) {
			served.DeniedDomains = append(served.DeniedDomains, h)
		}
	}
	return ev, served, nil
}
