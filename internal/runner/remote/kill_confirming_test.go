// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// A runner's kill that is only still stopping crosses the wire as its own code: the organisation reads
// runner.ErrTeardownConfirming, and the kill is neither queued nor offline. An older runner's plain answer
// for the same kill is not read as one.
func TestStillStoppingKillCrossesTheWire(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	rig.Fake.KillErr = fmt.Errorf("k8s: teardown of run held, NetworkPolicies kept: %w", runner.ErrTeardownConfirming)
	err := rig.Sub.KillSandbox(ctxT(t), sb.Ref)
	if !errors.Is(err, runner.ErrTeardownConfirming) || errors.Is(err, runner.ErrPendingOnRunner) || errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("still-stopping kill = %v, want ErrTeardownConfirming and not queued or offline", err)
	}
	rig.Fake.KillErr = errors.New("k8s: teardown of run held, NetworkPolicies kept: no completion evidence yet")
	if err := rig.Sub.KillSandbox(ctxT(t), sb.Ref); err == nil || errors.Is(err, runner.ErrTeardownConfirming) {
		t.Fatalf("an older runner's held kill = %v, want a plain failure", err)
	}
}
