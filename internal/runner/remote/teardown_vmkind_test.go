// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A kill or an end the runner answered drops the sandbox's machine kind, so the map holds no entry for a sandbox
// that is gone however it ended; a stop leaves it, as the sandbox can start again.
func TestTeardownDropsTheMachineKindOnKillAndEnd(t *testing.T) {
	for name, tc := range map[string]struct {
		do   func(*remote.Substrate, context.Context, string) error
		kept bool
	}{
		"kill": {do: (*remote.Substrate).KillSandbox},
		"end":  {do: (*remote.Substrate).EndSandbox},
		"stop": {do: (*remote.Substrate).StopSandbox, kept: true},
	} {
		t.Run(name, func(t *testing.T) {
			rig := runnertest.NewRig(t, rid)
			sb, _ := create(t, rig, nil)
			remote.SetVMKind(rig.Sub, sb.Ref, types.MachineMacOSVM)
			if err := tc.do(rig.Sub, ctxT(t), sb.Ref); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got := remote.VMKindOf(rig.Sub, sb.Ref); (got != "") != tc.kept {
				t.Fatalf("after %s the kind is %q, want it kept: %v", name, got, tc.kept)
			}
		})
	}
}

// An end that fails still drops the kind once the runner has answered: the second end finds the sandbox gone.
func TestFailedEndDropsTheMachineKind(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	if err := rig.Sub.KillSandbox(ctxT(t), sb.Ref); err != nil {
		t.Fatal(err)
	}
	remote.SetVMKind(rig.Sub, sb.Ref, types.MachineLinuxVM)
	_ = rig.Sub.EndSandbox(ctxT(t), sb.Ref)
	if got := remote.VMKindOf(rig.Sub, sb.Ref); got != "" {
		t.Fatalf("after an end of a sandbox the runner no longer has, the kind is %q, want none", got)
	}
}

// A teardown queued for a runner that is away keeps the kind: the runner is asked again.
func TestQueuedTeardownKeepsTheMachineKind(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	remote.SetVMKind(rig.Sub, sb.Ref, types.MachineMacOSVM)
	rig.Org.Drop()
	waitOffline(t, rig)
	if err := rig.Sub.KillSandbox(ctxT(t), sb.Ref); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("offline Kill = %v, want ErrPendingOnRunner", err)
	}
	if got := remote.VMKindOf(rig.Sub, sb.Ref); got != types.MachineMacOSVM {
		t.Fatalf("a queued kill left the kind %q, want it kept", got)
	}
}
