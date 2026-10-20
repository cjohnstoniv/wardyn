// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func vmSpec() runner.SandboxSpec {
	return runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: types.CC1, MachineKind: types.MachineLinuxVM}
}

// A runner whose caps carry no linux_vm (every runner before 0.10, and one whose operator left VMs
// off) is asked for no VM: the org refuses with the reason and sends no call.
func TestCreateVMRefusedWithoutTheRunnersLinuxVMCaps(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	_, err := rig.Sub.CreateSandbox(ctxT(t), vmSpec())
	if err == nil || !strings.Contains(err.Error(), "does not offer Linux VM sandboxes") {
		t.Fatalf("VM create = %v, want the linux_vm refusal", err)
	}
	if len(rig.Fake.Created) != 0 || len(rig.Fake.Calls) != 0 {
		t.Fatalf("the refused VM reached the runner: created %d, calls %v", len(rig.Fake.Created), rig.Fake.Calls)
	}
}

// The mixed-version pin: a runner that claims linux_vm but serves the 0.9.4 method set (the test
// server knows no create_vm) refuses the call, so a VM run never becomes a container on it.
func TestCreateVMIsItsOwnMethodThatAnOlderRunnerRefuses(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	caps := runnerwire.Caps{Version: "0.9.4", Capacity: placement.Capacity{CPUMillisMax: 4000, MemoryMiBMax: 8192},
		Support: mustClasses(t, rig),
		LinuxVM: &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13"}}
	data, err := runnerwire.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.Server.Peer.Event(ctxT(t), runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c, _ := rig.Sub.Caps(); c.LinuxVM == nil; c, _ = rig.Sub.Caps() {
		if time.Now().After(deadline) {
			t.Fatal("the linux_vm caps never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := rig.Sub.CreateSandbox(ctxT(t), vmSpec()); err == nil {
		t.Fatal("an older runner accepted a VM create")
	}
	if len(rig.Fake.Created) != 0 {
		t.Fatalf("the older runner created %d sandboxes for a VM run", len(rig.Fake.Created))
	}
	// A container create on the same runner is today's create_sandbox, unchanged.
	if _, spec := create(t, rig, nil); rig.Fake.Created[0].RunID != spec.RunID || rig.Fake.Created[0].MachineKind != "" {
		t.Fatalf("container create = %+v", rig.Fake.Created)
	}
}

// A runner that offers VMs but not folder shares into them (one from before the flag, or whose QEMU
// image has no working virtiofsd) is sent no VM run that carries a folder; one without a folder is
// still asked, by create_vm.
func TestCreateVMWithAFolderNeedsTheRunnersVirtiofs(t *testing.T) {
	for _, virtiofs := range []bool{false, true} {
		rig := runnertest.NewRig(t, rid)
		advertiseVM(t, rig, &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Virtiofs: virtiofs})
		spec := vmSpec()
		spec.Mounts = []runner.Mount{{Source: "/home/me/project", Target: "/home/agent/work", MemberAuthored: true}}
		_, err := rig.Sub.CreateSandbox(ctxT(t), spec)
		refused := err != nil && strings.Contains(err.Error(), remote.VMNoFolderShare)
		if refused == virtiofs {
			t.Fatalf("virtiofs %v: VM create with a folder = %v", virtiofs, err)
		}
		if !virtiofs && (len(rig.Fake.Created) != 0 || len(rig.Fake.Calls) != 0) {
			t.Fatalf("the refused VM reached the runner: created %d, calls %v", len(rig.Fake.Created), rig.Fake.Calls)
		}
	}
}

// advertiseVM sends caps that offer VMs as vm describes and waits until the substrate keeps them.
func advertiseVM(t *testing.T, rig *runnertest.Rig, vm *runnerwire.VMCaps) {
	t.Helper()
	caps := runnerwire.Caps{Version: "0.10.0", Capacity: placement.Capacity{CPUMillisMax: 4000, MemoryMiBMax: 8192},
		Support: mustClasses(t, rig), LinuxVM: vm}
	data, err := runnerwire.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.Server.Peer.Event(ctxT(t), runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c, _ := rig.Sub.Caps(); c.LinuxVM == nil || *c.LinuxVM != *vm; c, _ = rig.Sub.Caps() {
		if time.Now().After(deadline) {
			t.Fatal("the linux_vm caps never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func mustClasses(t *testing.T, rig *runnertest.Rig) substrate.ClassSupport {
	t.Helper()
	cs, err := rig.Fake.Classes(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	return cs
}
