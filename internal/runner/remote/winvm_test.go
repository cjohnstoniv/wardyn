// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"errors"
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

// advertiseWindows sends caps offering Linux VMs as linux and Windows VMs as win, and waits until the
// substrate keeps them.
func advertiseWindows(t *testing.T, rig *runnertest.Rig, linux, win *runnerwire.VMCaps) {
	t.Helper()
	caps := runnerwire.Caps{Version: "0.11.0", Capacity: placement.Capacity{CPUMillisMax: 8000, MemoryMiBMax: 16384},
		Support: mustClasses(t, rig), LinuxVM: linux, WindowsVM: win}
	data, err := runnerwire.Marshal(caps)
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.Server.Peer.Event(ctxT(t), runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c, ok := rig.Sub.Caps(); !ok || (c.LinuxVM == nil) != (linux == nil) || (c.WindowsVM == nil) != (win == nil); c, ok = rig.Sub.Caps() {
		if time.Now().After(deadline) {
			t.Fatal("the caps never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func winVMSpec() runner.SandboxSpec {
	return runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: types.CC1, MachineKind: types.MachineWindowsVM, Interactive: true}
}

// A Windows VM is asked only of a runner whose caps carry windows_vm: a runner that offers Linux VMs
// alone (before, a Windows spec passed its Linux check) is refused with the reason and sent no call.
func TestCheckVMCapsWindows(t *testing.T) {
	vm := &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Desktop: true}
	for name, tc := range map[string]struct {
		linux, win *runnerwire.VMCaps
		offered    bool
	}{
		"no VM caps":       {},
		"Linux VMs only":   {linux: vm},
		"Windows VMs too":  {linux: vm, win: vm, offered: true},
		"Windows VMs only": {win: vm, offered: true},
	} {
		t.Run(name, func(t *testing.T) {
			rig := runnertest.NewRig(t, rid)
			if tc.linux != nil || tc.win != nil {
				advertiseWindows(t, rig, tc.linux, tc.win)
			}
			_, err := rig.Sub.CreateSandbox(ctxT(t), winVMSpec())
			refused := err != nil && strings.Contains(err.Error(), "does not offer Windows VM sandboxes")
			if refused == tc.offered {
				t.Fatalf("Windows VM create = %v, want refused %v", err, !tc.offered)
			}
			if refused && (len(rig.Fake.Created) != 0 || len(rig.Fake.Calls) != 0) {
				t.Fatalf("the refused Windows VM reached the runner: created %d, calls %v", len(rig.Fake.Created), rig.Fake.Calls)
			}
			// The fake serves no create_vm, so an offered create passes the caps check and gets the
			// runner's own answer.
			if tc.offered && err == nil {
				t.Fatalf("an offered Windows VM create = nil, want the runner's own answer to create_vm")
			}
		})
	}
}

// byKind gates a Windows VM's desktop by its own caps, never by Linux's, and never pauses its agent:
// it has none.
func TestByKindWindows(t *testing.T) {
	for name, tc := range map[string]struct {
		linux, win *runnerwire.VMCaps
		desktop    bool
	}{
		"Windows desktop":              {win: &runnerwire.VMCaps{Desktop: true}, desktop: true},
		"Linux desktop, Windows none":  {linux: &runnerwire.VMCaps{Desktop: true, SignIns: true}, win: &runnerwire.VMCaps{}},
		"Linux desktop, no Windows VM": {linux: &runnerwire.VMCaps{Desktop: true, SignIns: true}},
	} {
		t.Run(name, func(t *testing.T) {
			rig := runnertest.NewRig(t, rid)
			advertiseWindows(t, rig, tc.linux, tc.win)
			sb, _ := create(t, rig, nil)
			remote.SetVMKind(rig.Sub, sb.Ref, types.MachineWindowsVM)
			d, derr := rig.Sub.DialDesktop(ctxT(t), sb.Ref)
			if d != nil {
				_ = d.Close()
			}
			if refused := errors.Is(derr, runner.ErrDesktopUnsupported); refused == tc.desktop {
				t.Fatalf("dial desktop = %v, want refused %v", derr, !tc.desktop)
			}
			calls := len(rig.Fake.Calls)
			if _, perr := rig.Sub.PauseAgent(ctxT(t), sb.Ref, 1); !errors.Is(perr, runner.ErrAgentPauseUnsupported) || len(rig.Fake.Calls) != calls {
				t.Fatalf("pause on a Windows VM = %v (calls %v), want refused before any call", perr, rig.Fake.Calls[calls:])
			}
		})
	}
}
