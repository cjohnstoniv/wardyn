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

// An end the runner answers with a failure still drops the kind: the substrate does no more for the sandbox.
func TestFailedEndDropsTheMachineKind(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	remote.SetVMKind(rig.Sub, sb.Ref, types.MachineLinuxVM)
	rig.Fake.EndErr = errors.New("end failed on the runner")
	err := rig.Sub.EndSandbox(ctxT(t), sb.Ref)
	if err == nil || errors.Is(err, runner.ErrRunnerOffline) || errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("End = %v, want the runner's failure", err)
	}
	if got := remote.VMKindOf(rig.Sub, sb.Ref); got != "" {
		t.Fatalf("after a failed end the kind is %q, want none", got)
	}
}

// A kill queued for a runner that is away drops the kind too: the runner replays it on its own side on
// reconnect, which never comes back through the substrate.
func TestQueuedKillDropsTheMachineKind(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	remote.SetVMKind(rig.Sub, sb.Ref, types.MachineMacOSVM)
	rig.Org.Drop()
	waitOffline(t, rig)
	if err := rig.Sub.KillSandbox(ctxT(t), sb.Ref); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("offline Kill = %v, want ErrPendingOnRunner", err)
	}
	if got := remote.VMKindOf(rig.Sub, sb.Ref); got != "" {
		t.Fatalf("a queued kill left the kind %q, want none", got)
	}
}
