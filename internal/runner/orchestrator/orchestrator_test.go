// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeSubstrate is a minimal substrate.Substrate that records which refs each
// lifecycle op was routed to, so the orchestrator's multiplexing is observable.
type fakeSubstrate struct {
	name       string
	classes    []types.ConfinementClass
	resolved   map[types.ConfinementClass]string
	structural bool
	recording  bool
	refPrefix  string

	mu                            sync.Mutex
	created                       []runner.SandboxSpec
	execs, statuses, stops, kills []string
}

func (f *fakeSubstrate) Name() string { return f.name }

func (f *fakeSubstrate) Classes(context.Context) (substrate.ClassSupport, error) {
	return substrate.ClassSupport{
		Classes:          f.classes,
		Resolved:         f.resolved,
		StructuralEgress: f.structural,
		SessionRecording: f.recording,
	}, nil
}

func (f *fakeSubstrate) CreateSandbox(_ context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	f.mu.Lock()
	f.created = append(f.created, spec)
	f.mu.Unlock()
	return runner.Sandbox{Ref: f.refPrefix + spec.RunID.String(), Driver: f.name, EnforcedClass: spec.ConfinementClass}, nil
}

func (f *fakeSubstrate) rec(slot *[]string, ref string) {
	f.mu.Lock()
	*slot = append(*slot, ref)
	f.mu.Unlock()
}

func (f *fakeSubstrate) Exec(_ context.Context, ref string, _ []string) (string, error) {
	f.rec(&f.execs, ref)
	return "", nil
}
func (f *fakeSubstrate) Wait(context.Context, string) (int, error) { return 0, nil }
func (f *fakeSubstrate) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	return nil, nil
}
func (f *fakeSubstrate) Status(_ context.Context, ref string) (runner.Status, error) {
	f.rec(&f.statuses, ref)
	return runner.Status{State: types.RunRunning}, nil
}
func (f *fakeSubstrate) AgentStatus(_ context.Context, ref, _ string) (runner.Status, error) {
	f.rec(&f.statuses, ref)
	return runner.Status{State: types.RunRunning}, nil
}
func (f *fakeSubstrate) StopSandbox(_ context.Context, ref string) error {
	f.rec(&f.stops, ref)
	return nil
}
func (f *fakeSubstrate) KillSandbox(_ context.Context, ref string) error {
	f.rec(&f.kills, ref)
	return nil
}

func specFor(class types.ConfinementClass) runner.SandboxSpec {
	return runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: class}
}

func TestOrchestrator_CapabilitiesAggregateAndSort(t *testing.T) {
	oci := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1, types.CC2}, resolved: map[types.ConfinementClass]string{types.CC1: "oci/runc", types.CC2: "oci/runsc"}, structural: true, recording: true}
	vmm := &fakeSubstrate{name: "smolvm", classes: []types.ConfinementClass{types.CC3}, resolved: map[types.ConfinementClass]string{types.CC3: "vmm/firecracker"}, structural: true}
	o := New(vmm, oci) // intentionally out of order

	caps, err := o.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	// Union, strongest last regardless of substrate order.
	want := []types.ConfinementClass{types.CC1, types.CC2, types.CC3}
	if len(caps.ConfinementClasses) != 3 || caps.ConfinementClasses[0] != want[0] || caps.ConfinementClasses[2] != want[2] {
		t.Fatalf("ConfinementClasses = %v, want %v (strongest last)", caps.ConfinementClasses, want)
	}
	if caps.Resolved[types.CC3] != "vmm/firecracker" || caps.Resolved[types.CC1] != "oci/runc" {
		t.Fatalf("Resolved = %v", caps.Resolved)
	}
	if !caps.StructuralEgress || !caps.SessionRecording {
		t.Fatalf("aggregated bools = %+v", caps)
	}
}

func TestOrchestrator_RoutesByClassAndTracksRef(t *testing.T) {
	oci := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1, types.CC2}, refPrefix: "oci-"}
	vmm := &fakeSubstrate{name: "smolvm", classes: []types.ConfinementClass{types.CC3}, refPrefix: "vmm-"}
	o := New(oci, vmm)

	// CC3 must route to the VMM substrate...
	sb, err := o.CreateSandbox(context.Background(), specFor(types.CC3))
	if err != nil {
		t.Fatalf("CreateSandbox CC3: %v", err)
	}
	if len(vmm.created) != 1 || len(oci.created) != 0 {
		t.Fatalf("CC3 must route to vmm; vmm.created=%d oci.created=%d", len(vmm.created), len(oci.created))
	}
	// ...and subsequent lifecycle ops must follow the ref to the SAME substrate.
	_, _ = o.Exec(context.Background(), sb.Ref, []string{"x"})
	_ = o.KillSandbox(context.Background(), sb.Ref)
	if len(vmm.execs) != 1 || vmm.execs[0] != sb.Ref {
		t.Fatalf("Exec must route to vmm by ref; got %v", vmm.execs)
	}
	if len(vmm.kills) != 1 || len(oci.kills) != 0 {
		t.Fatalf("Kill must route to vmm; vmm.kills=%v oci.kills=%v", vmm.kills, oci.kills)
	}
	// CC1 routes to the OCI substrate.
	if _, err := o.CreateSandbox(context.Background(), specFor(types.CC1)); err != nil {
		t.Fatalf("CreateSandbox CC1: %v", err)
	}
	if len(oci.created) != 1 {
		t.Fatalf("CC1 must route to oci; oci.created=%d", len(oci.created))
	}
}

func TestOrchestrator_FailsClosedWhenNoSubstrateEnforcesClass(t *testing.T) {
	oci := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}
	o := New(oci)
	if _, err := o.CreateSandbox(context.Background(), specFor(types.CC3)); err == nil {
		t.Fatal("CC3 with no enforcing substrate must fail closed")
	}
	if len(oci.created) != 0 {
		t.Fatal("nothing must be created on fail-closed")
	}
}

func TestOrchestrator_UntrackedRefFallsBackToSoleSubstrate(t *testing.T) {
	// Crash-recovery: after a restart byRef is empty; with a single substrate the
	// orchestrator still routes (the substrate's teardown rebuilds state).
	oci := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}
	o := New(oci)
	if err := o.KillSandbox(context.Background(), "wardyn-agent-unknown"); err != nil {
		t.Fatalf("sole-substrate fallback must route an untracked ref: %v", err)
	}
	if len(oci.kills) != 1 {
		t.Fatalf("untracked ref must fall back to the sole substrate; kills=%v", oci.kills)
	}
}

func TestOrchestrator_NameIsSoleSubstrate(t *testing.T) {
	if got := New(&fakeSubstrate{name: "docker"}).Name(); got != "docker" {
		t.Fatalf("single-substrate Name = %q, want docker", got)
	}
	if got := New(&fakeSubstrate{name: "a"}, &fakeSubstrate{name: "b"}).Name(); got != "orchestrator" {
		t.Fatalf("multi-substrate Name = %q, want orchestrator", got)
	}
}
