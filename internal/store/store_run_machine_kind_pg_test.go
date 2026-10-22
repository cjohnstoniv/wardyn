// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// machine_kind round-trips (a container run reads back exactly as before the column), the VM's
// machine is written once by its own scoped update, a container run's row never takes one, and the
// column refuses an unknown kind.
func TestPG_RunMachineKindAndVMMachine(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	ctr := persistRun(t, ctx, pool, newRun(types.RunStarting))
	if ctr.MachineKind != "" || ctr.VMMachine != nil {
		t.Fatalf("container run = %q %+v, want the empty kind and no machine", ctr.MachineKind, ctr.VMMachine)
	}
	m := types.VMMachine{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", Accelerator: "kvm", QEMUVersion: "10.0.13"}
	if err := pg.SetRunVMMachine(ctx, ctr.ID, m); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a container run took a VM machine: %v", err)
	}

	r := newRun(types.RunStarting)
	r.MachineKind = types.MachineLinuxVM
	vm := persistRun(t, ctx, pool, r)
	if vm.MachineKind != types.MachineLinuxVM || vm.VMMachine != nil {
		t.Fatalf("VM run = %q %+v", vm.MachineKind, vm.VMMachine)
	}
	if err := pg.SetRunVMMachine(ctx, vm.ID, m); err != nil {
		t.Fatal(err)
	}
	got, err := pg.GetRun(ctx, vm.ID)
	if err != nil || got.VMMachine == nil || *got.VMMachine != m || got.MachineKind != types.MachineLinuxVM {
		t.Fatalf("GetRun = %+v %v", got.VMMachine, err)
	}

	bad := newRun(types.RunStarting)
	bad.MachineKind = "windows_vm"
	if _, err := pg.CreateRun(ctx, bad); err == nil {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, bad.ID)
		t.Fatal("agent_runs took an unknown machine kind")
	}
}
