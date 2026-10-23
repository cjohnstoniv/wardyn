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

// SetRunVMMachine scoped-writes the virtual hardware a Linux VM run was started with, once, after its
// create. A container run's row is never touched.
func (s PG) SetRunVMMachine(ctx context.Context, id uuid.UUID, m types.VMMachine) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("store: marshal vm machine: %w", err)
	}
	return s.execRun(ctx, "set run vm machine",
		`UPDATE agent_runs SET vm_machine=$1, updated_at=now() WHERE id=$2 AND machine_kind='linux_vm'`, b, id)
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
