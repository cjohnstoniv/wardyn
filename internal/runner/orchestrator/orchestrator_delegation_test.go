// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

// orchestrator_delegation_test.go is #174's orchestrator half: table tests
// for the seven Orchestrator methods docs/TEST-GAPS.md listed as untested
// (AgentStatus, Attach, ExecStream, ImagePresent, Status,
// SweepOrphanedSandboxes, Wait). Every one is a delegating pass-through —
// resolve a substrate, forward the call — so a test that only checks "the
// call didn't error" proves nothing about the delegation itself. Each case
// here wires TWO substrates and asserts WHICH ONE actually ran the call
// (recorded ref on the selected fake, untouched on the other), the same
// substrate-selection assertion orchestrator_test.go's
// TestOrchestrator_RoutesByClassAndTracksRef already uses for Exec/Kill.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestOrchestrator_RefRoutedMethods_SelectSubstrateByRef covers Wait, Attach,
// ExecStream, Status and AgentStatus: all five resolve their substrate via
// subForRef, so one sandbox created on each of two substrates must route each
// method to the substrate that owns its ref, never the other.
func TestOrchestrator_RefRoutedMethods_SelectSubstrateByRef(t *testing.T) {
	oci, vmm := twoSubstrates()
	o := New(oci, vmm)
	ctx := context.Background()

	ociSb, err := o.CreateSandbox(ctx, specFor(types.CC1))
	if err != nil {
		t.Fatalf("CreateSandbox CC1: %v", err)
	}
	vmmSb, err := o.CreateSandbox(ctx, specFor(types.CC3))
	if err != nil {
		t.Fatalf("CreateSandbox CC3: %v", err)
	}

	if _, err := o.Wait(ctx, ociSb.Ref); err != nil {
		t.Fatalf("Wait(oci ref): %v", err)
	}
	if _, err := o.Attach(ctx, vmmSb.Ref, runner.AttachOptions{}); err != nil {
		t.Fatalf("Attach(vmm ref): %v", err)
	}
	if _, err := o.ExecStream(ctx, ociSb.Ref, runner.ExecSpec{}); err != nil && !errors.Is(err, runner.ErrExecStreamUnsupported) {
		t.Fatalf("ExecStream(oci ref): %v", err)
	}
	if _, err := o.Status(ctx, vmmSb.Ref); err != nil {
		t.Fatalf("Status(vmm ref): %v", err)
	}
	if _, err := o.AgentStatus(ctx, ociSb.Ref, "exec-1"); err != nil {
		t.Fatalf("AgentStatus(oci ref): %v", err)
	}

	// Wait routed to oci, not vmm.
	if len(oci.waits) != 1 || oci.waits[0] != ociSb.Ref {
		t.Errorf("Wait must route to oci by ref; oci.waits=%v", oci.waits)
	}
	if len(vmm.waits) != 0 {
		t.Errorf("Wait must NOT reach vmm; vmm.waits=%v", vmm.waits)
	}

	// Attach routed to vmm, not oci.
	if len(vmm.attaches) != 1 || vmm.attaches[0] != vmmSb.Ref {
		t.Errorf("Attach must route to vmm by ref; vmm.attaches=%v", vmm.attaches)
	}
	if len(oci.attaches) != 0 {
		t.Errorf("Attach must NOT reach oci; oci.attaches=%v", oci.attaches)
	}

	// ExecStream routed to oci, not vmm.
	if len(oci.execStreams) != 1 || oci.execStreams[0] != ociSb.Ref {
		t.Errorf("ExecStream must route to oci by ref; oci.execStreams=%v", oci.execStreams)
	}
	if len(vmm.execStreams) != 0 {
		t.Errorf("ExecStream must NOT reach vmm; vmm.execStreams=%v", vmm.execStreams)
	}

	// Status routed to vmm, not oci.
	if len(vmm.statuses) != 1 || vmm.statuses[0] != vmmSb.Ref {
		t.Errorf("Status must route to vmm by ref; vmm.statuses=%v", vmm.statuses)
	}
	if len(oci.statuses) != 0 {
		t.Errorf("Status must NOT reach oci; oci.statuses=%v", oci.statuses)
	}

	// AgentStatus routed to oci, not vmm.
	if len(oci.agentStatuses) != 1 || oci.agentStatuses[0] != ociSb.Ref {
		t.Errorf("AgentStatus must route to oci by ref; oci.agentStatuses=%v", oci.agentStatuses)
	}
	if len(vmm.agentStatuses) != 0 {
		t.Errorf("AgentStatus must NOT reach vmm; vmm.agentStatuses=%v", vmm.agentStatuses)
	}
}

// imageCheckingSubstrate wraps a fakeSubstrate to additionally implement
// runner.ImageChecker — a SEPARATE concrete type (not a field on
// fakeSubstrate) so a test can wire one substrate that implements the
// optional capability and one that structurally does not, exactly like the
// real docker (implements it) vs k8s (does not) split ImagePresent's own doc
// comment describes.
type imageCheckingSubstrate struct {
	*fakeSubstrate
	present    bool
	presentErr error
	calls      []string
}

func (i *imageCheckingSubstrate) ImagePresent(_ context.Context, ref string) (bool, error) {
	i.calls = append(i.calls, ref)
	return i.present, i.presentErr
}

var _ runner.ImageChecker = (*imageCheckingSubstrate)(nil)

// TestOrchestrator_ImagePresent_SelectsFirstImplementingSubstrate pins
// ImagePresent's actual selection rule: the FIRST wired substrate that
// implements runner.ImageChecker, skipping any that don't (k8s has no local
// image cache and simply isn't asked). Checking only "no error" would pass
// even if the wrong substrate — or none — answered.
func TestOrchestrator_ImagePresent_SelectsFirstImplementingSubstrate(t *testing.T) {
	// k8s-shaped: wired FIRST but does not implement ImageChecker at all.
	k8s := &fakeSubstrate{name: "k8s", classes: []types.ConfinementClass{types.CC3}}
	// docker-shaped: wired second, implements ImageChecker.
	docker := &imageCheckingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}, present: true}
	o := New(k8s, docker)

	got, err := o.ImagePresent(context.Background(), "wardyn/agent:local")
	if err != nil {
		t.Fatalf("ImagePresent: %v", err)
	}
	if !got {
		t.Errorf("ImagePresent = false, want true (from the docker-shaped substrate)")
	}
	if len(docker.calls) != 1 || docker.calls[0] != "wardyn/agent:local" {
		t.Errorf("ImagePresent must reach the implementing substrate; docker.calls=%v", docker.calls)
	}

	// Order matters: put the implementing substrate FIRST and confirm it
	// (not a later one) answers, proving "first implementer wins" rather
	// than "last" or "the only one that happens to be right by coincidence".
	second := &imageCheckingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker2", classes: []types.ConfinementClass{types.CC2}}, present: false}
	o2 := New(docker, second)
	got2, err := o2.ImagePresent(context.Background(), "img")
	if err != nil {
		t.Fatalf("ImagePresent (reordered): %v", err)
	}
	if !got2 {
		t.Errorf("ImagePresent (reordered) = false, want true (the FIRST implementer, docker, not docker2)")
	}
	if len(second.calls) != 0 {
		t.Errorf("ImagePresent must stop at the first implementer; second.calls=%v", second.calls)
	}
}

// TestOrchestrator_ImagePresent_FailsWhenNoSubstrateImplementsIt pins the
// fail-closed message when NOTHING wired implements the optional capability
// (a k8s-only deployment) — distinct from "implements it and returns false".
func TestOrchestrator_ImagePresent_FailsWhenNoSubstrateImplementsIt(t *testing.T) {
	k8s := &fakeSubstrate{name: "k8s", classes: []types.ConfinementClass{types.CC3}}
	o := New(k8s)
	if _, err := o.ImagePresent(context.Background(), "img"); err == nil {
		t.Fatal("ImagePresent must fail when no wired substrate implements ImageChecker")
	}
}

// sweepingSubstrate wraps a fakeSubstrate to additionally implement the
// unexported SweepOrphanedSandboxes capability Orchestrator type-asserts for
// — again a separate concrete type so one substrate can implement it and
// another (a future non-OCI VMM, or a test fake standing in for one) can not.
type sweepingSubstrate struct {
	*fakeSubstrate
	n         int
	err       error
	sweptRefs []string // records the minAge/isOrphan call happened
}

func (s *sweepingSubstrate) SweepOrphanedSandboxes(_ context.Context, _ time.Duration, isOrphan func(uuid.UUID) bool) (int, error) {
	s.sweptRefs = append(s.sweptRefs, s.name)
	_ = isOrphan(uuid.New()) // proves the predicate the caller passed was actually wired through
	return s.n, s.err
}

// TestOrchestrator_SweepOrphanedSandboxes_SumsOnlyImplementingSubstrates pins
// the fan-out/sum behaviour: every substrate that implements the optional
// sweep capability is swept and its count summed; a substrate that doesn't
// (the plain fakeSubstrate, standing in for a future VMM substrate with
// nothing to sweep) is silently skipped rather than erroring the whole call.
func TestOrchestrator_SweepOrphanedSandboxes_SumsOnlyImplementingSubstrates(t *testing.T) {
	a := &sweepingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker-a"}, n: 3}
	b := &sweepingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker-b"}, n: 4}
	plain := &fakeSubstrate{name: "vmm"} // does NOT implement the sweep capability
	o := New(a, b, plain)

	total, err := o.SweepOrphanedSandboxes(context.Background(), time.Hour, func(uuid.UUID) bool { return true })
	if err != nil {
		t.Fatalf("SweepOrphanedSandboxes: %v", err)
	}
	if total != 7 {
		t.Fatalf("total = %d, want 7 (3 from docker-a + 4 from docker-b)", total)
	}
	if len(a.sweptRefs) != 1 || len(b.sweptRefs) != 1 {
		t.Errorf("both implementing substrates must be swept exactly once; a=%v b=%v", a.sweptRefs, b.sweptRefs)
	}
}

// TestOrchestrator_SweepOrphanedSandboxes_JoinsErrorsButKeepsSumming pins that
// one substrate's sweep error does not swallow another's count nor abort the
// fan-out.
func TestOrchestrator_SweepOrphanedSandboxes_JoinsErrorsButKeepsSumming(t *testing.T) {
	ok := &sweepingSubstrate{fakeSubstrate: &fakeSubstrate{name: "ok"}, n: 2}
	failing := &sweepingSubstrate{fakeSubstrate: &fakeSubstrate{name: "failing"}, n: 1, err: errors.New("boom")}
	o := New(ok, failing)

	total, err := o.SweepOrphanedSandboxes(context.Background(), time.Minute, func(uuid.UUID) bool { return false })
	if total != 3 {
		t.Fatalf("total = %d, want 3 (both substrates' counts summed despite the error)", total)
	}
	if err == nil {
		t.Fatal("expected the failing substrate's error to be joined and returned")
	}
}
