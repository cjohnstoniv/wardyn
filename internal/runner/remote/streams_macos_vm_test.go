// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner that offers macOS VMs alone is asked to pause the agent when its caps keep sign-ins, and to
// open the desktop only when its Mac's Screen Sharing can be controlled: an observe-only Mac, or one
// with no Screen Sharing, offers no desktop and is asked nothing.
func TestPauseAndDesktopFollowTheMacOSVMCaps(t *testing.T) {
	for name, tc := range map[string]struct {
		caps           runnerwire.MacVMCaps
		pause, desktop bool
	}{
		"no sign-ins, no desktop": {caps: runnerwire.MacVMCaps{Max: 2, Desktop: runnerwire.MacVMDesktopNone}},
		"observe only":            {caps: runnerwire.MacVMCaps{Max: 2, Desktop: runnerwire.MacVMDesktopObserve, SignIns: true}, pause: true},
		"control":                 {caps: runnerwire.MacVMCaps{Max: 2, Desktop: runnerwire.MacVMDesktopControl, SignIns: true}, pause: true, desktop: true},
	} {
		t.Run(name, func(t *testing.T) {
			rig := runnertest.NewRig(t, rid)
			advertiseKinds(t, rig, nil, &tc.caps)
			sb, _ := create(t, rig, nil)
			calls := len(rig.Fake.Calls)
			_, perr := rig.Sub.PauseAgent(ctxT(t), sb.Ref, 1)
			if refused := errors.Is(perr, runner.ErrAgentPauseUnsupported); refused == tc.pause {
				t.Fatalf("pause = %v, want refused %v", perr, !tc.pause)
			}
			if !tc.pause && len(rig.Fake.Calls) != calls {
				t.Fatalf("the refused pause reached the runner: %v", rig.Fake.Calls[calls:])
			}
			d, derr := rig.Sub.DialDesktop(ctxT(t), sb.Ref)
			if d != nil {
				_ = d.Close()
			}
			if refused := errors.Is(derr, runner.ErrDesktopUnsupported); refused == tc.desktop {
				t.Fatalf("dial desktop = %v, want refused %v", derr, !tc.desktop)
			}
		})
	}
}

// The desktop and the pause are gated by the caps of the ref's own kind: a runner whose Linux VMs have a
// desktop and whose Mac's Screen Sharing is observe-only opens a Linux VM's desktop and refuses a macOS
// VM's, and the reverse.
func TestDesktopAndPauseGatedByTheRefsKind(t *testing.T) {
	for name, tc := range map[string]struct {
		linux        runnerwire.VMCaps
		mac          runnerwire.MacVMCaps
		linuxDesktop bool
		macDesktop   bool
		linuxPause   bool
		macPause     bool
	}{
		"linux desktop, mac observe": {linux: runnerwire.VMCaps{Desktop: true, SignIns: true}, mac: runnerwire.MacVMCaps{Max: 2, Desktop: runnerwire.MacVMDesktopObserve},
			linuxDesktop: true, linuxPause: true},
		"mac control, linux none": {mac: runnerwire.MacVMCaps{Max: 2, Desktop: runnerwire.MacVMDesktopControl, SignIns: true},
			macDesktop: true, macPause: true},
	} {
		t.Run(name, func(t *testing.T) {
			rig := runnertest.NewRig(t, rid)
			advertiseKinds(t, rig, &tc.linux, &tc.mac)
			for kind, want := range map[types.MachineKind][2]bool{
				types.MachineLinuxVM: {tc.linuxDesktop, tc.linuxPause},
				types.MachineMacOSVM: {tc.macDesktop, tc.macPause},
			} {
				// The fake runner creates containers only; the ref is recorded as a VM of kind as a VM
				// create's success records it.
				sb, _ := create(t, rig, nil)
				remote.SetVMKind(rig.Sub, sb.Ref, kind)
				d, derr := rig.Sub.DialDesktop(ctxT(t), sb.Ref)
				if d != nil {
					_ = d.Close()
				}
				if refused := errors.Is(derr, runner.ErrDesktopUnsupported); refused == want[0] {
					t.Fatalf("%s: dial desktop = %v, want refused %v", kind, derr, !want[0])
				}
				if _, perr := rig.Sub.PauseAgent(ctxT(t), sb.Ref, 1); errors.Is(perr, runner.ErrAgentPauseUnsupported) == want[1] {
					t.Fatalf("%s: pause = %v, want refused %v", kind, perr, !want[1])
				}
			}
		})
	}
}
