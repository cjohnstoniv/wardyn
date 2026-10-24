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

// SetRunVMMachine scoped-writes the virtual hardware a VM run (either kind) was started with, once, after
// its create. A container run's row is never touched.
func (s PG) SetRunVMMachine(ctx context.Context, id uuid.UUID, m types.VMMachine) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: marshal vm machine: %w", err)
	}
	return s.execRun(ctx, "set run vm machine",
		`UPDATE agent_runs SET vm_machine=$1, updated_at=now() WHERE id=$2 AND machine_kind IN ('linux_vm','macos_vm')`, b, id)
}

// scanRunMachine sets a run's machine kind and, for a VM run, the machine recorded for it, from
// scanRun's machine_kind and vm_machine columns.
func scanRunMachine(r *types.AgentRun, kind string, vmMachineRaw []byte) error {
	r.MachineKind = types.MachineKind(kind)
	if vmMachineRaw == nil {
		return nil
	}
	if err := json.Unmarshal(vmMachineRaw, &r.VMMachine); err != nil {
		return fmt.Errorf("store: unmarshal run vm machine: %w", err)
	}
	return nil
}

// RunnerMacOSVMHolders counts the macos_vm runs holding a sandbox on each of ids, by the predicate
// RunnerSlotHolders uses, so placement can say a Mac is at its licence cap before it sends a create. A
// runner with none is absent from the map.
func (s PG) RunnerMacOSVMHolders(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]int, error) {
	rows, err := s.Pool.Query(ctx, `SELECT runner_id, count(*)::int FROM agent_runs
		WHERE runner_id = ANY($1) AND machine_kind = 'macos_vm' AND state = ANY($2) AND `+HoldsSandboxSQL+` GROUP BY 1`,
		ids, slotHoldingStateNames())
	if err != nil {
		return nil, fmt.Errorf("store: count macos_vm runs by runner: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]int, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: scan macos_vm runs by runner: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}
