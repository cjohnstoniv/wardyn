// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run admitted against one executor of a remote-provided pool is created there and nowhere else,
// even when the first substrate (what class routing would pick) can run the spec.
func TestOrchestrator_CreateSandboxOnIsExactAndNeverFallsBack(t *testing.T) {
	first := &fakeSubstrate{name: "first", classes: []types.ConfinementClass{types.CC1, types.CC2}}
	second := &fakeSubstrate{name: "second", classes: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}}
	r := runnerSub("r1")
	o := New(first, second)
	o.Add(r)
	ctx := context.Background()

	sb, err := o.CreateSandboxOn(ctx, "second", specFor(types.CC1))
	if err != nil || len(second.created) != 1 || len(first.created) != 0 || len(r.created) != 0 {
		t.Fatalf("create on second: err=%v first=%d second=%d runner=%d", err, len(first.created), len(second.created), len(r.created))
	}
	// The ref follows its executor for every later call.
	if _, err := o.Status(ctx, sb.Ref); err != nil || len(second.statuses) != 1 || len(first.statuses) != 0 {
		t.Fatalf("status routed first=%v second=%v err=%v", first.statuses, second.statuses, err)
	}

	for name, executor := range map[string]string{"an unknown executor": "third", "a registered runner is no executor": "runner:r1", "empty": ""} {
		if _, err := o.CreateSandboxOn(ctx, executor, specFor(types.CC1)); !errors.Is(err, runner.ErrExecutorUnavailable) {
			t.Errorf("%s: %v, want ErrExecutorUnavailable", name, err)
		}
	}
	// An executor that cannot enforce the class refuses; the one that can is not substituted.
	if _, err := o.CreateSandboxOn(ctx, "first", specFor(types.CC3)); !errors.Is(err, runner.ErrExecutorUnavailable) {
		t.Errorf("CC3 on first = %v, want ErrExecutorUnavailable", err)
	}
	if len(first.created) != 0 || len(second.created) != 1 || len(r.created) != 0 {
		t.Fatalf("a refused create reached a substrate: first=%d second=%d runner=%d", len(first.created), len(second.created), len(r.created))
	}
}

func TestOrchestrator_TargetFactsArePerTarget(t *testing.T) {
	a := &fakeSubstrate{name: "a", classes: []types.ConfinementClass{types.CC1}}
	b := &fakeSubstrate{name: "b", classes: []types.ConfinementClass{types.CC1, types.CC3}}
	o := New(a, b)
	ctx := context.Background()
	fa, ok := o.ExecutorFacts(ctx, "a")
	fb, okb := o.ExecutorFacts(ctx, "b")
	if !ok || !okb || len(fa.Classes) != 1 || len(fb.Classes) != 2 || fa.CapacityAdvertised {
		t.Fatalf("executor facts a=%+v b=%+v", fa, fb)
	}
	if _, ok := o.ExecutorFacts(ctx, "missing"); ok {
		t.Error("an executor this deployment does not run reported facts")
	}
	// A runner is not an executor, and a runner that advertised nothing is unknown, not zero.
	o.Add(runnerSub("r1"))
	if _, ok := o.ExecutorFacts(ctx, "runner:r1"); ok {
		t.Error("a registered runner answered as an executor")
	}
	if _, ok := o.RunnerFacts("r1"); ok {
		t.Error("a runner that advertised nothing reported facts")
	}
	if _, ok := o.RunnerFacts("nobody"); ok {
		t.Error("an unregistered runner reported facts")
	}
}

// An executor's facts carry its disk word and whether it can keep an ended sandbox: the pool says only
// what every target does.
func TestExecutorFactsCarryDiskWordAndKeep(t *testing.T) {
	ender := &endingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1},
		diskEnf: types.StorageEnforcementFilesystem, drives: true}}
	plain := &fakeSubstrate{name: "k8s", classes: []types.ConfinementClass{types.CC1}, diskEnf: types.StorageEnforcementEviction}
	o := New(ender, plain)
	ctx := context.Background()
	fd, okd := o.ExecutorFacts(ctx, "docker")
	fk, okk := o.ExecutorFacts(ctx, "k8s")
	if !okd || !okk {
		t.Fatalf("executor facts unknown: docker=%v k8s=%v", okd, okk)
	}
	if fd.EphemeralDiskEnforcement != types.StorageEnforcementFilesystem || !fd.KeepsEnded || !fd.UserDrives {
		t.Errorf("docker facts %+v, want filesystem, keeps, drives", fd)
	}
	if fk.EphemeralDiskEnforcement != types.StorageEnforcementEviction || fk.KeepsEnded || fk.UserDrives {
		t.Errorf("k8s facts %+v, want eviction, no keep, no drives", fk)
	}
}

// R2-0155: an executor whose probe fails answers unknown with its probe error, so placement can tell it
// from one this deployment does not run (unknown, no error).
func TestExecutorFactsCarryTheProbeError(t *testing.T) {
	o := New(deadSubstrate{&fakeSubstrate{name: "dead"}}, &fakeSubstrate{name: "live", classes: []types.ConfinementClass{types.CC1}})
	ctx := context.Background()
	if f, ok := o.ExecutorFacts(ctx, "dead"); ok || f.ProbeErr == nil {
		t.Errorf("an executor that cannot report: facts %+v ok %v, want unknown with the probe error", f, ok)
	}
	if f, ok := o.ExecutorFacts(ctx, "missing"); ok || f.ProbeErr != nil {
		t.Errorf("an executor this deployment does not run: facts %+v ok %v", f, ok)
	}
}
