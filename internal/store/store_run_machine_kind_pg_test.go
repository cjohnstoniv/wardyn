// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

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

// A macos_vm run is a VM run: its kind round-trips, it takes the scoped machine write (a macOS machine
// reads back with its guest build), its desktop may keep its sign-ins on the run's disk, the org counts
// the macos_vm runs that hold a sandbox on each runner (no other kind, no ended run), and the column
// still refuses an unknown kind (migration 0234).
func TestPG_MacOSVMKindAndHolders(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := queuePool(t, pg)
	mac, other := capacityRunner(t, pg, 4), capacityRunner(t, pg, 4)
	on := func(rn types.Runner, kind types.MachineKind, state types.RunState) types.AgentRun {
		t.Helper()
		r := onRunner(p, rn, state)
		r.MachineKind = kind
		got, err := pg.CreateRun(ctx, r)
		if err != nil {
			t.Fatalf("create %q run: %v", kind, err)
		}
		return got
	}
	first := on(mac, types.MachineMacOSVM, types.RunPending)
	on(mac, types.MachineMacOSVM, types.RunPending)
	on(mac, types.MachineLinuxVM, types.RunPending)
	ctr := on(mac, "", types.RunPending)
	on(other, types.MachineMacOSVM, types.RunPending)
	on(mac, types.MachineMacOSVM, types.RunCompleted)

	if first.MachineKind != types.MachineMacOSVM {
		t.Fatalf("macos_vm run read back as %q", first.MachineKind)
	}
	held, err := pg.RunnerMacOSVMHolders(ctx, []uuid.UUID{mac.ID, other.ID, uuid.New()})
	if err != nil || len(held) != 2 || held[mac.ID] != 2 || held[other.ID] != 1 {
		t.Fatalf("holders = %v (%v), want 2 on the Mac, 1 on the other and nothing for an unknown runner", held, err)
	}
	// RunnerUsage counts the same macos_vm runs among each runner's slot-holders, in its one query.
	used, err := pg.RunnerUsage(ctx, []uuid.UUID{mac.ID, other.ID}, nil, uuid.Nil)
	if err != nil || used[mac.ID.String()].Runs != 4 || used[mac.ID.String()].MacOSVMRuns != held[mac.ID] || used[other.ID.String()].MacOSVMRuns != held[other.ID] {
		t.Fatalf("usage = %+v (%v), want 4 runs on the Mac, 2 of them macOS VMs, and 1 on the other", used, err)
	}

	m := types.VMMachine{MachineType: types.MacVMMachineType, Accelerator: types.MacVMAccelerator, GuestOS: "macOS 27.0.1 (26A434)", HostOS: "macOS 27.0 (26A400)"}
	if err := pg.SetRunVMMachine(ctx, first.ID, m); err != nil {
		t.Fatal(err)
	}
	if got, err := pg.GetRun(ctx, first.ID); err != nil || got.VMMachine == nil || *got.VMMachine != m {
		t.Fatalf("GetRun = %+v %v", got.VMMachine, err)
	}
	if err := pg.SetRunVMMachine(ctx, ctr.ID, m); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a container run took a macOS machine: %v", err)
	}

	if err := pg.RecordDesktopVMSignIns(ctx, first.ID, "run_disk"); err != nil {
		t.Fatalf("run_disk sign-ins: %v", err)
	}
	if row, found, err := pg.DesktopVMSignIns(ctx, first.ID); err != nil || !found || row.Store != "run_disk" {
		t.Fatalf("sign-ins row = %+v %v %v", row, found, err)
	}
	if err := pg.RecordDesktopVMSignIns(ctx, first.ID, "elsewhere"); err == nil {
		t.Fatal("desktop_vm_sign_ins took an unknown store")
	}

	bad := newRun(types.RunStarting)
	bad.MachineKind = "windows_vm"
	if _, err := pg.CreateRun(ctx, bad); err == nil {
		_, _ = pg.Pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, bad.ID)
		t.Fatal("agent_runs took an unknown machine kind")
	}
}
