// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newProbeRun is newStepRun behind the host-capacity refusal, so a probe on a
// saturated host mints no identity. Split out of site_config_probe.go, which
// sits at the file-size cap.
func (s *Server) newProbeRun(ctx context.Context, runID uuid.UUID, actor, task string, cc types.ConfinementClass, gov stepRunGovernance, set func(*types.AgentRun)) (types.AgentRun, string, error) {
	if err := s.cfg.HostCapacity.Admit(); err != nil {
		return types.AgentRun{}, "", err
	}
	return s.newStepRun(ctx, runID, actor, task, cc, gov, set)
}
