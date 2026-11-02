// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// diskDispatcher is a dispatcher over a fresh fake, with one sandbox created on it.
func diskDispatcher(t *testing.T) (*Dispatcher, *runnertest.Fake, runner.Sandbox, string) {
	t.Helper()
	runnerID := uuid.NewString()
	fake := runnertest.NewFake()
	sb, err := fake.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	d, err := newTestDispatcher(Config{RunnerID: runnerID, Substrate: fake,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return d, fake, sb, runnerID
}

func callRef(t *testing.T, d *Dispatcher, method, runnerID, ref string) (any, error) {
	t.Helper()
	a, b := runnerwire.Loopback()
	t.Cleanup(func() { _ = b.Close() })
	peer := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	t.Cleanup(func() { peer.Close() })
	data, _ := runnerwire.Marshal(runnerwire.RefArgs{Ref: placement.RunnerRefPrefix(runnerID) + ref})
	return d.Call(t.Context(), peer, 1, method, data)
}

var teardownVerbs = []string{runnerwire.MethodStop, runnerwire.MethodKill, runnerwire.MethodEnd}

// A runner of this release answers stop, kill and end with the run's macOS VM disk report, and keeps it.
func TestTeardownReplyCarriesTheMacOSVMDisk(t *testing.T) {
	for _, method := range teardownVerbs {
		t.Run(method, func(t *testing.T) {
			d, fake, sb, runnerID := diskDispatcher(t)
			want := runner.MacOSVMDisk{RunID: uuid.New(), State: runner.MacOSVMDiskRemoved, At: time.Now().UTC()}
			fake.SetMacOSVMDisk(sb.Ref, want)
			res, err := callRef(t, d, method, runnerID, sb.Ref)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := res.(runnerwire.TeardownResult)
			if !ok || got.MacOSVMDisk == nil || *got.MacOSVMDisk != want {
				t.Fatalf("result = %#v, want the report %+v", res, want)
			}
			if held := fake.MacOSVMDisks(); len(held) != 1 || held[0] != want {
				t.Fatalf("the runner kept %+v, want the report still held", held)
			}
		})
	}
}

// With no report (a container run, or what an older runner sends) each teardown verb replies with no result.
func TestTeardownReplyWithoutAReportIsNil(t *testing.T) {
	for _, method := range teardownVerbs {
		t.Run(method, func(t *testing.T) {
			d, _, sb, runnerID := diskDispatcher(t)
			res, err := callRef(t, d, method, runnerID, sb.Ref)
			if err != nil {
				t.Fatal(err)
			}
			if res != nil {
				t.Fatalf("result = %#v, want nil", res)
			}
		})
	}
}

// Both sweep passes carry every report the runner holds, with no teardown call first: the offline queue's path.
func TestSweepRepliesCarryEveryHeldMacOSVMDisk(t *testing.T) {
	d, fake, sb, _ := diskDispatcher(t)
	one := runner.MacOSVMDisk{RunID: uuid.New(), State: runner.MacOSVMDiskRemoved, At: time.Now().UTC()}
	two := runner.MacOSVMDisk{RunID: uuid.New(), State: runner.MacOSVMDiskKept, Reason: runner.MacOSVMDiskHelperAlive, At: time.Now().UTC()}
	fake.SetMacOSVMDisk(sb.Ref, one)
	fake.SetMacOSVMDisk("other", two)
	for _, pass := range []struct {
		name   string
		remove []uuid.UUID
	}{{"list", nil}, {"remove", []uuid.UUID{uuid.New()}}} {
		t.Run(pass.name, func(t *testing.T) {
			data, _ := runnerwire.Marshal(runnerwire.SweepArgs{MinAge: time.Minute, Remove: pass.remove})
			res, err := d.sweep(t.Context(), data)
			if err != nil {
				t.Fatal(err)
			}
			got := res.(runnerwire.SweepResult).MacOSVMDisks
			if len(got) != 2 || !(got[0] == one && got[1] == two || got[0] == two && got[1] == one) {
				t.Fatalf("the %s pass carried %+v, want both reports", pass.name, got)
			}
		})
	}
}
