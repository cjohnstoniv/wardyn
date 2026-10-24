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
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func macSpec() runner.SandboxSpec {
	return runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: types.CC1, MachineKind: types.MachineMacOSVM}
}

// advertiseKinds sends caps that offer the given VM kinds (either may be nil) and waits for the substrate
// to keep them.
func advertiseKinds(t *testing.T, rig *runnertest.Rig, linux *runnerwire.VMCaps, mac *runnerwire.MacVMCaps) {
	t.Helper()
	caps := runnerwire.Caps{Version: "0.10.0", Capacity: placement.Capacity{CPUMillisMax: 4000, MemoryMiBMax: 8192},
		Support: mustClasses(t, rig), LinuxVM: linux, MacOSVM: mac}
	data, err := runnerwire.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.Server.Peer.Event(ctxT(t), runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c, _ := rig.Sub.Caps(); (linux != nil && c.LinuxVM == nil) || (mac != nil && c.MacOSVM == nil); c, _ = rig.Sub.Caps() {
		if time.Now().After(deadline) {
			t.Fatal("the caps never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func notSent(t *testing.T, rig *runnertest.Rig) {
	t.Helper()
	if len(rig.Fake.Created) != 0 || len(rig.Fake.Calls) != 0 {
		t.Fatalf("the refused VM reached the runner: created %d, calls %v", len(rig.Fake.Created), rig.Fake.Calls)
	}
}

// A macOS VM is asked of a runner only when its caps carry macos_vm: no caps, and a runner that offers
// Linux VMs alone, are refused with the reason and sent no call.
func TestCreateMacOSVMNeedsTheRunnersMacOSVMCaps(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	if _, err := rig.Sub.CreateSandbox(ctxT(t), macSpec()); err == nil || !strings.Contains(err.Error(), remote.MacOSVMNotOffered) {
		t.Fatalf("macOS VM create with no caps = %v, want the macos_vm refusal", err)
	}
	notSent(t, rig)

	advertiseKinds(t, rig, &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Virtiofs: true}, nil)
	if _, err := rig.Sub.CreateSandbox(ctxT(t), macSpec()); err == nil || !strings.Contains(err.Error(), remote.MacOSVMNotOffered) {
		t.Fatalf("macOS VM create on a Linux-VM-only runner = %v, want the macos_vm refusal", err)
	}
	notSent(t, rig)
}

// The reverse holds too: a Linux VM is never sent to a runner that offers macOS VMs alone.
func TestCreateLinuxVMNeedsTheRunnersLinuxVMCaps(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	advertiseKinds(t, rig, nil, &runnerwire.MacVMCaps{Max: 2, Base: runnerwire.MacVMBase{State: runnerwire.MacVMBaseReady}})
	if _, err := rig.Sub.CreateSandbox(ctxT(t), vmSpec()); err == nil || !strings.Contains(err.Error(), "does not offer Linux VM sandboxes") {
		t.Fatalf("Linux VM create on a macOS-VM-only runner = %v, want the linux_vm refusal", err)
	}
	notSent(t, rig)
}

// A macOS VM that carries a folder is refused with the folder-share sentence, even on a runner that
// offers macOS VMs; one without a folder passes the org's check and reaches the runner, which here
// serves no create_vm and so answers the call itself.
func TestCreateMacOSVMTakesNoFolder(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	advertiseKinds(t, rig, nil, &runnerwire.MacVMCaps{Max: 2, Base: runnerwire.MacVMBase{State: runnerwire.MacVMBaseReady}})
	spec := macSpec()
	spec.Mounts = []runner.Mount{{Source: "/Users/me/project", Target: "/Users/agent/work", MemberAuthored: true}}
	if _, err := rig.Sub.CreateSandbox(ctxT(t), spec); err == nil || !strings.Contains(err.Error(), remote.VMNoFolderShare) {
		t.Fatalf("macOS VM with a folder = %v, want the folder-share refusal", err)
	}
	notSent(t, rig)

	_, err := rig.Sub.CreateSandbox(ctxT(t), macSpec())
	if err == nil || strings.Contains(err.Error(), remote.MacOSVMNotOffered) || strings.Contains(err.Error(), remote.VMNoFolderShare) {
		t.Fatalf("macOS VM with no folder = %v, want the org's checks passed and the runner's own answer", err)
	}
}
