// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnertest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const diskRunner = "runner-macos-disk"

// diskRig is a rig with one sandbox created through the org's substrate, and its report set on the fake.
func diskRig(t *testing.T, report bool) (*Rig, runner.Sandbox, runner.MacOSVMDisk) {
	t.Helper()
	r := NewRig(t, diskRunner)
	sb, err := r.Sub.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: types.CC1})
	if err != nil {
		t.Fatal(err)
	}
	d := runner.MacOSVMDisk{RunID: uuid.New(), State: runner.MacOSVMDiskRemoved, At: time.Now().UTC().Truncate(time.Microsecond)}
	if report {
		r.Fake.SetMacOSVMDisk(strings.TrimPrefix(sb.Ref, placement.RunnerRefPrefix(diskRunner)), d)
	}
	return r, sb, d
}

func sameDisks(got []runner.MacOSVMDisk, want ...runner.MacOSVMDisk) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !got[i].At.Equal(want[i].At) || got[i].RunID != want[i].RunID || got[i].State != want[i].State || got[i].Reason != want[i].Reason {
			return false
		}
	}
	return true
}

// Stop, kill and end hand the run's report to the org's hook.
func TestTeardownHandsTheMacOSVMDiskToTheOrg(t *testing.T) {
	verbs := map[string]func(*Rig, context.Context, string) error{
		"stop": func(r *Rig, ctx context.Context, ref string) error { return r.Sub.StopSandbox(ctx, ref) },
		"kill": func(r *Rig, ctx context.Context, ref string) error { return r.Sub.KillSandbox(ctx, ref) },
		"end":  func(r *Rig, ctx context.Context, ref string) error { return r.Sub.EndSandbox(ctx, ref) },
	}
	for name, call := range verbs {
		t.Run(name, func(t *testing.T) {
			r, sb, d := diskRig(t, true)
			if err := call(r, t.Context(), sb.Ref); err != nil {
				t.Fatal(err)
			}
			if got := r.MacOSVMDisks(); !sameDisks(got, d) {
				t.Fatalf("the org received %+v, want exactly %+v", got, d)
			}
		})
	}
}

// A sweep hands the reports from its list pass, and from its remove pass when it has one.
func TestSweepHandsTheMacOSVMDisksFromBothPasses(t *testing.T) {
	all := func(uuid.UUID) bool { return true }
	t.Run("with an orphan", func(t *testing.T) {
		r, _, d := diskRig(t, true)
		r.Fake.mu.Lock()
		r.Fake.Orphans = []uuid.UUID{uuid.New()}
		r.Fake.mu.Unlock()
		if _, err := r.Sub.SweepOrphanedSandboxes(t.Context(), 0, all); err != nil {
			t.Fatal(err)
		}
		if got := r.MacOSVMDisks(); !sameDisks(got, d, d) {
			t.Fatalf("the org received %+v, want the report from both passes", got)
		}
	})
	t.Run("with none", func(t *testing.T) {
		r, _, d := diskRig(t, true)
		if _, err := r.Sub.SweepOrphanedSandboxes(t.Context(), 0, all); err != nil {
			t.Fatal(err)
		}
		if got := r.MacOSVMDisks(); !sameDisks(got, d) {
			t.Fatalf("the org received %+v, want the report from the list pass alone", got)
		}
	})
}

// A runner with no report, as an older runner looks: stop and sweep succeed, and the org receives nothing.
func TestNoMacOSVMDiskReachesTheOrgWithoutAReport(t *testing.T) {
	r, sb, _ := diskRig(t, false)
	if err := r.Sub.StopSandbox(t.Context(), sb.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Sub.SweepOrphanedSandboxes(t.Context(), 0, func(uuid.UUID) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if got := r.MacOSVMDisks(); len(got) != 0 {
		t.Fatalf("the org received %+v, want nothing", got)
	}
}

// An org older than the report decodes the new replies: its sweep result has no field for the reports, and its
// stop passes no result at all.
func TestAnOlderOrgDecodesTheNewReplies(t *testing.T) {
	r, sb, _ := diskRig(t, true)
	var old struct {
		Candidates []uuid.UUID `json:"candidates,omitempty"`
		Removed    int         `json:"removed"`
	}
	if err := r.Link.Call(t.Context(), runnerwire.MethodSweep, runnerwire.SweepArgs{}, &old); err != nil {
		t.Fatalf("sweep into the old result = %v", err)
	}
	if err := r.Link.Call(t.Context(), runnerwire.MethodStop, runnerwire.RefArgs{Ref: sb.Ref}, nil); err != nil {
		t.Fatalf("stop with no result = %v", err)
	}
}
