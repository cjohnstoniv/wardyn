// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runServiceJSON is the service column's value: NULL for a run that is not a service.
func runServiceJSON(sv *types.RunService) ([]byte, error) {
	if sv == nil {
		return nil, nil
	}
	raw, err := json.Marshal(sv)
	if err != nil {
		return nil, fmt.Errorf("store: marshal run service: %w", err)
	}
	return raw, nil
}

// scanRunService sets a run's service spec and its last status from scanRun's service and
// service_status columns; a NULL column leaves its field nil.
func scanRunService(r *types.AgentRun, serviceRaw, statusRaw []byte) error {
	if serviceRaw != nil {
		r.Service = &types.RunService{}
		if err := json.Unmarshal(serviceRaw, r.Service); err != nil {
			return fmt.Errorf("store: unmarshal run service: %w", err)
		}
	}
	if statusRaw != nil {
		r.ServiceStatus = &types.RunServiceStatus{}
		if err := json.Unmarshal(statusRaw, r.ServiceStatus); err != nil {
			return fmt.Errorf("store: unmarshal run service status: %w", err)
		}
	}
	return nil
}

// ServiceRunStore is the service-health surface. Optional like RunLimitsReclamper; the api layer
// mirrors no service health when a store lacks it.
type ServiceRunStore interface {
	ListServiceRuns(ctx context.Context) ([]types.AgentRun, error)
	SetRunServiceStatus(ctx context.Context, id uuid.UUID, st types.RunServiceStatus) (bool, error)
}

var _ ServiceRunStore = PG{}

// ListServiceRuns reads the runs whose service health is worth reading: RUNNING with a service spec, a
// sandbox, and neither lost nor paused (a frozen or gone sandbox answers nothing).
func (s PG) ListServiceRuns(ctx context.Context) ([]types.AgentRun, error) {
	q := `SELECT ` + runCols + ` FROM agent_runs
		WHERE state = $1 AND service IS NOT NULL AND lost_at IS NULL AND paused_at IS NULL AND sandbox_ref <> ''
		ORDER BY created_at, id`
	return collect(ctx, s.Pool, "list", "service runs", q, []any{string(types.RunRunning)}, scanRun)
}

// SetRunServiceStatus scoped-writes ONLY service_status, and only while the run is RUNNING; false when
// no row matched. Deliberately does NOT bump updated_at, as SetRunStatusDetail does not: that column is
// also what the idle reaper and the killed-run tail-upload grace measure by, and a health probe the
// control plane runs must never buy a run more life or hold a terminal run's upload door open.
func (s PG) SetRunServiceStatus(ctx context.Context, id uuid.UUID, st types.RunServiceStatus) (bool, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return false, fmt.Errorf("store: marshal run service status: %w", err)
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET service_status=$1 WHERE id=$2 AND state=$3`,
		raw, id, string(types.RunRunning))
	if err != nil {
		return false, fmt.Errorf("store: set run service status: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
