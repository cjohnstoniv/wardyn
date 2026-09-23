// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package orchestrator

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	drives     bool
	managed    bool
	diskEnf    types.StorageEnforcement
	freeze     map[types.ConfinementClass]bool
	refPrefix  string

	mu                            sync.Mutex
	created                       []runner.SandboxSpec
	execs, statuses, stops, kills []string
	classesCalls                  atomic.Int64 // counts live Classes() probes
}

func (f *fakeSubstrate) Name() string { return f.name }

func (f *fakeSubstrate) Classes(context.Context) (substrate.ClassSupport, error) {
	f.classesCalls.Add(1)
	return substrate.ClassSupport{
		Classes:                  f.classes,
		Resolved:                 f.resolved,
		StructuralEgress:         f.structural,
		SessionRecording:         f.recording,
		UserDrives:               f.drives,
		ManagedFiles:             f.managed,
		EphemeralDiskEnforcement: f.diskEnf,
		Freeze:                   f.freeze,
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
func (f *fakeSubstrate) ExecStream(context.Context, string, runner.ExecSpec) (*runner.ExecSession, error) {
	return nil, runner.ErrExecStreamUnsupported
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

// fakeRefStore is an in-memory RefStore; putErr forces PutRef failures so the
// fail-closed create path is testable.
type fakeRefStore struct {
	mu     sync.Mutex
	m      map[string]string
	putErr error
}

func (f *fakeRefStore) PutRef(_ context.Context, ref, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	if f.m == nil {
		f.m = map[string]string{}
	}
	f.m[ref] = name
	return nil
}

func (f *fakeRefStore) GetRef(_ context.Context, ref string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := f.m[ref]
	return name, ok, nil
}

func (f *fakeRefStore) DeleteRef(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, ref)
	return nil
}

func twoSubstrates() (*fakeSubstrate, *fakeSubstrate) {
	oci := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1, types.CC2}, refPrefix: "oci-"}
	vmm := &fakeSubstrate{name: "smolvm", classes: []types.ConfinementClass{types.CC3}, refPrefix: "vmm-"}
	return oci, vmm
}

func TestOrchestrator_RefStoreSurvivesRestartMultiSubstrate(t *testing.T) {
	ctx := context.Background()
	rs := &fakeRefStore{}
	oci, vmm := twoSubstrates()
	sb, err := New(oci, vmm).WithRefStore(rs).CreateSandbox(ctx, specFor(types.CC3))
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if name, ok, _ := rs.GetRef(ctx, sb.Ref); !ok || name != "smolvm" {
		t.Fatalf("ref must be write-through persisted as smolvm; got %q ok=%v", name, ok)
	}

	// "Restart": a FRESH orchestrator (byRef empty) over fresh substrates but
	// the same durable store. StopSandbox must rehydrate the route to #2.
	// Counterfactual: without the RefStore consultation a 2-substrate fresh
	// orchestrator returns "no substrate tracked for ref".
	oci2, vmm2 := twoSubstrates()
	if err := New(oci2, vmm2).WithRefStore(rs).StopSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("StopSandbox after restart: %v", err)
	}
	if len(vmm2.stops) != 1 || vmm2.stops[0] != sb.Ref || len(oci2.stops) != 0 {
		t.Fatalf("Stop must rehydrate to vmm; vmm.stops=%v oci.stops=%v", vmm2.stops, oci2.stops)
	}
	// Successful teardown must have garbage-collected the durable row.
	if _, ok, _ := rs.GetRef(ctx, sb.Ref); ok {
		t.Fatal("successful StopSandbox must DeleteRef")
	}

	// Same for the kill switch: re-seed the row, restart again, KillSandbox.
	if err := rs.PutRef(ctx, sb.Ref, "smolvm"); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	oci3, vmm3 := twoSubstrates()
	if err := New(oci3, vmm3).WithRefStore(rs).KillSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("KillSandbox after restart: %v", err)
	}
	if len(vmm3.kills) != 1 || vmm3.kills[0] != sb.Ref || len(oci3.kills) != 0 {
		t.Fatalf("Kill must rehydrate to vmm; vmm.kills=%v oci.kills=%v", vmm3.kills, oci3.kills)
	}
	if _, ok, _ := rs.GetRef(ctx, sb.Ref); ok {
		t.Fatal("successful KillSandbox must DeleteRef")
	}
}

func TestOrchestrator_CreateFailsClosedWhenRefPersistFails(t *testing.T) {
	ctx := context.Background()
	oci, vmm := twoSubstrates()
	o := New(oci, vmm).WithRefStore(&fakeRefStore{putErr: context.DeadlineExceeded})
	if _, err := o.CreateSandbox(ctx, specFor(types.CC3)); err == nil {
		t.Fatal("CreateSandbox must fail closed when the ref cannot be persisted")
	}
	// The just-created (untracked) sandbox must be best-effort torn down, and
	// the failed ref must not linger in byRef.
	if len(vmm.kills) != 1 {
		t.Fatalf("untracked sandbox must be killed on persist failure; kills=%v", vmm.kills)
	}
	o.mu.Lock()
	n := len(o.byRef)
	o.mu.Unlock()
	if n != 0 {
		t.Fatalf("byRef must not retain the failed ref; len=%d", n)
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

// TestOrchestrator_ClassesCachedWithinTTL pins Capabilities()/substrateFor()
// memoize each substrate's ClassSupport for capsCacheTTL, so repeated hot-path
// calls collapse to ONE daemon probe per substrate per TTL (they previously did a
// live docker Info() round-trip every call). A countable fake proves the probe
// count; a fake clock proves the TTL boundary forces exactly one refresh.
func TestOrchestrator_ClassesCachedWithinTTL(t *testing.T) {
	oci := &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1, types.CC2}, resolved: map[types.ConfinementClass]string{types.CC1: "oci/runc"}}
	o := New(oci)
	clock := time.Now()
	o.now = func() time.Time { return clock }

	// Many hot-path reads within the TTL must probe the daemon exactly once.
	for i := 0; i < 5; i++ {
		if _, err := o.Capabilities(context.Background()); err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
	}
	if _, err := o.substrateFor(context.Background(), types.CC1); err != nil {
		t.Fatalf("substrateFor: %v", err)
	}
	if n := oci.classesCalls.Load(); n != 1 {
		t.Fatalf("within TTL: Classes probed %d times, want 1", n)
	}

	// Crossing the TTL boundary forces exactly one refresh (not one-per-call).
	clock = clock.Add(capsCacheTTL + time.Second)
	if _, err := o.Capabilities(context.Background()); err != nil {
		t.Fatalf("Capabilities after TTL: %v", err)
	}
	if n := oci.classesCalls.Load(); n != 2 {
		t.Fatalf("after TTL: Classes probed %d times, want 2", n)
	}
}

// TestCapabilitiesUserDrivesIsAConjunction pins the ONE flag this aggregate
// does not union, and the reason is which side of ROUTING it is read on.
//
// Every other flag describes a control that must hold for the run routed to
// that substrate, and CreateSandbox routes by confinement class — so a union is
// right for them. A drive request is refused BEFORE routing, so a union here
// would let a deployment with one drive-capable substrate promise a mount to a
// run the orchestrator then hands to one that cannot bind it: the "previewed
// green, failed at dispatch" shape the flag exists to close.
//
// It is not hypothetical. Today both substrates report false. D3 lands the
// Docker mount and D4 lands the Kubernetes one, so between them exactly this
// mixed deployment exists.
func TestCapabilitiesUserDrivesIsAConjunction(t *testing.T) {
	ctx := context.Background()
	sub := func(name string, drives bool) *fakeSubstrate {
		return &fakeSubstrate{name: name, classes: []types.ConfinementClass{types.CC1}, drives: drives}
	}

	for _, tc := range []struct {
		name string
		subs []*fakeSubstrate
		want bool
	}{
		{name: "every substrate can bind", subs: []*fakeSubstrate{sub("a", true), sub("b", true)}, want: true},
		{name: "one cannot, so the deployment cannot", subs: []*fakeSubstrate{sub("a", true), sub("b", false)}, want: false},
		{name: "order does not matter", subs: []*fakeSubstrate{sub("a", false), sub("b", true)}, want: false},
		{name: "none can", subs: []*fakeSubstrate{sub("a", false)}, want: false},
		{name: "a single capable substrate can", subs: []*fakeSubstrate{sub("a", true)}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var subs []substrate.Substrate
			for _, s := range tc.subs {
				subs = append(subs, s)
			}
			caps, err := New(subs...).Capabilities(ctx)
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if caps.UserDrives != tc.want {
				t.Errorf("UserDrives = %v, want %v", caps.UserDrives, tc.want)
			}
		})
	}

	// With nothing wired there is nothing to bind, so the flag must not read
	// true out of an empty conjunction.
	caps, err := New().Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities (no substrates): %v", err)
	}
	if caps.UserDrives {
		t.Error("UserDrives = true with no substrates wired")
	}
}

// TestCapabilitiesManagedFilesIsAConjunction pins the second non-union flag,
// for UserDrives' reason: whether a run gets its root-owned ceiling is decided
// BEFORE a substrate is picked, so a union would let a deployment promise a
// file one of its substrates cannot deliver. The failure that would cause is
// worse than a refused mount — the control plane would record the ceiling as
// delivered on a run that never got one.
func TestCapabilitiesManagedFilesIsAConjunction(t *testing.T) {
	ctx := context.Background()
	sub := func(name string, managed bool) *fakeSubstrate {
		return &fakeSubstrate{name: name, classes: []types.ConfinementClass{types.CC1}, managed: managed}
	}

	for _, tc := range []struct {
		name string
		subs []*fakeSubstrate
		want bool
	}{
		{name: "every substrate can deliver", subs: []*fakeSubstrate{sub("a", true), sub("b", true)}, want: true},
		{name: "one cannot, so the deployment cannot", subs: []*fakeSubstrate{sub("a", true), sub("b", false)}, want: false},
		{name: "order does not matter", subs: []*fakeSubstrate{sub("a", false), sub("b", true)}, want: false},
		{name: "a single capable substrate can", subs: []*fakeSubstrate{sub("a", true)}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var subs []substrate.Substrate
			for _, s := range tc.subs {
				subs = append(subs, s)
			}
			caps, err := New(subs...).Capabilities(ctx)
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if caps.ManagedFiles != tc.want {
				t.Errorf("ManagedFiles = %v, want %v", caps.ManagedFiles, tc.want)
			}
		})
	}

	caps, err := New().Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities (no substrates): %v", err)
	}
	if caps.ManagedFiles {
		t.Error("ManagedFiles = true with no substrates wired")
	}
}

// TestCapabilities_EphemeralDiskEnforcementIsTheWeakestWord pins the aggregation
// direction. A UNION would be the bug: the word is what an admin is told a disk
// number MEANS, and a deployment with one docker-on-xfs substrate must not tell
// them `filesystem` when the run might be routed to a substrate that only evicts
// — or to one that binds nothing at all.
func TestCapabilities_EphemeralDiskEnforcementIsTheWeakestWord(t *testing.T) {
	for _, tc := range []struct {
		name string
		subs []types.StorageEnforcement
		want types.StorageEnforcement
	}{
		{"no substrates bind nothing", nil, ""},
		{"one substrate reports its own word", []types.StorageEnforcement{types.StorageEnforcementEviction}, types.StorageEnforcementEviction},
		{"eviction is weaker than a filesystem quota", []types.StorageEnforcement{types.StorageEnforcementFilesystem, types.StorageEnforcementEviction}, types.StorageEnforcementEviction},
		{"order does not matter", []types.StorageEnforcement{types.StorageEnforcementEviction, types.StorageEnforcementFilesystem}, types.StorageEnforcementEviction},
		{"one substrate that binds nothing wins", []types.StorageEnforcement{types.StorageEnforcementFilesystem, types.StorageEnforcementNone}, types.StorageEnforcementNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subs := make([]substrate.Substrate, 0, len(tc.subs))
			for i, e := range tc.subs {
				subs = append(subs, &fakeSubstrate{name: "sub" + strconv.Itoa(i), classes: []types.ConfinementClass{types.CC1}, diskEnf: e})
			}
			caps, err := New(subs...).Capabilities(context.Background())
			if err != nil {
				t.Fatalf("Capabilities: %v", err)
			}
			if caps.EphemeralDiskEnforcement != tc.want {
				t.Errorf("EphemeralDiskEnforcement = %q, want %q", caps.EphemeralDiskEnforcement, tc.want)
			}
		})
	}
}

// endingSubstrate is a fakeSubstrate that can keep an ended sandbox.
type endingSubstrate struct {
	*fakeSubstrate
	ends []string
}

func (e *endingSubstrate) EndSandbox(_ context.Context, ref string) error {
	e.rec(&e.ends, ref)
	return nil
}

// TestOrchestrator_EndSandbox: the lease end reaches a substrate that can keep
// a stopped sandbox, and keeps the route so a later kill still finds it. One
// that cannot keep it (Kubernetes) answers ErrEndUnsupported, which the control
// plane turns into a full teardown.
func TestOrchestrator_EndSandbox(t *testing.T) {
	ctx := context.Background()
	oci := &endingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}}
	o := New(oci)
	if err := o.EndSandbox(ctx, "wardyn-agent-x"); err != nil {
		t.Fatalf("EndSandbox: %v", err)
	}
	if err := o.KillSandbox(ctx, "wardyn-agent-x"); err != nil {
		t.Fatalf("KillSandbox after the end: %v", err)
	}
	if len(oci.ends) != 1 || len(oci.kills) != 1 {
		t.Errorf("ends %v kills %v; want the end forwarded and the route kept for the kill", oci.ends, oci.kills)
	}

	k8s := New(&fakeSubstrate{name: "k8s", classes: []types.ConfinementClass{types.CC1}})
	if err := k8s.EndSandbox(ctx, "wardyn-agent-y"); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Errorf("EndSandbox on a substrate that cannot keep a sandbox = %v, want ErrEndUnsupported", err)
	}
}

// proxyStoppingSubstrate is a fakeSubstrate that can remove a proxy alone.
type proxyStoppingSubstrate struct {
	*fakeSubstrate
	proxyStops []string
}

func (p *proxyStoppingSubstrate) StopProxy(_ context.Context, ref string) error {
	p.rec(&p.proxyStops, ref)
	return nil
}

// TestOrchestrator_StopProxy: a lost run's proxy removal reaches a substrate
// that can do it and keeps the route; one that cannot (Kubernetes) answers
// ErrEndUnsupported, which the control plane turns into a full teardown.
func TestOrchestrator_StopProxy(t *testing.T) {
	ctx := context.Background()
	oci := &proxyStoppingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}}
	o := New(oci)
	if err := o.StopProxy(ctx, "wardyn-agent-x"); err != nil {
		t.Fatalf("StopProxy: %v", err)
	}
	if err := o.KillSandbox(ctx, "wardyn-agent-x"); err != nil {
		t.Fatalf("KillSandbox after StopProxy: %v", err)
	}
	if len(oci.proxyStops) != 1 || len(oci.kills) != 1 {
		t.Errorf("proxy stops %v kills %v; want the stop forwarded and the route kept", oci.proxyStops, oci.kills)
	}

	k8s := New(&fakeSubstrate{name: "k8s", classes: []types.ConfinementClass{types.CC1}})
	if err := k8s.StopProxy(ctx, "wardyn-agent-y"); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Errorf("StopProxy on a substrate that cannot keep a sandbox = %v, want ErrEndUnsupported", err)
	}
}

// revivingSubstrate is a fakeSubstrate that can replace a proxy in place.
type revivingSubstrate struct {
	*fakeSubstrate
	replaced []string
}

func (r *revivingSubstrate) ProxyConfig(context.Context, string) ([]byte, error) {
	return []byte(`{"run_token":"t"}`), nil
}

func (r *revivingSubstrate) ReplaceProxy(_ context.Context, ref string, _ []byte) error {
	r.rec(&r.replaced, ref)
	return nil
}

// TestOrchestrator_ProxyReviver: a proxy-only revive reaches a substrate that
// can replace a proxy in place; one that cannot (Kubernetes) answers
// ErrReviveUnsupported for both halves.
func TestOrchestrator_ProxyReviver(t *testing.T) {
	ctx := context.Background()
	oci := &revivingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}}
	o := New(oci)
	if cfg, err := o.ProxyConfig(ctx, "wardyn-agent-x"); err != nil || string(cfg) != `{"run_token":"t"}` {
		t.Fatalf("ProxyConfig = %s, %v", cfg, err)
	}
	if err := o.ReplaceProxy(ctx, "wardyn-agent-x", nil); err != nil || len(oci.replaced) != 1 {
		t.Fatalf("ReplaceProxy: %v, replaced %v", err, oci.replaced)
	}

	k8s := New(&fakeSubstrate{name: "k8s", classes: []types.ConfinementClass{types.CC1}})
	if _, err := k8s.ProxyConfig(ctx, "wardyn-agent-y"); !errors.Is(err, runner.ErrReviveUnsupported) {
		t.Errorf("ProxyConfig on a substrate that cannot = %v, want ErrReviveUnsupported", err)
	}
	if err := k8s.ReplaceProxy(ctx, "wardyn-agent-y", nil); !errors.Is(err, runner.ErrReviveUnsupported) {
		t.Errorf("ReplaceProxy on a substrate that cannot = %v, want ErrReviveUnsupported", err)
	}
}

// freezingSubstrate is a fakeSubstrate that can pause/resume the agent.
type freezingSubstrate struct {
	*fakeSubstrate
	freezes, thaws []string
}

func (f *freezingSubstrate) FreezeSandbox(_ context.Context, ref string) error {
	f.rec(&f.freezes, ref)
	return nil
}
func (f *freezingSubstrate) ThawSandbox(_ context.Context, ref string) error {
	f.rec(&f.thaws, ref)
	return nil
}

// TestOrchestrator_FreezeSandbox: Freeze/Thaw reach the substrate that owns
// the ref when it implements runner.Freezer, and the route survives (a later
// kill still finds it). Two substrates and no RefStore, so there is no
// sole-substrate fallback: a Freeze that dropped the route would fail the
// kill. A substrate that does not implement Freezer (Kubernetes) answers
// ErrFreezeUnsupported.
func TestOrchestrator_FreezeSandbox(t *testing.T) {
	ctx := context.Background()
	vmm := &fakeSubstrate{name: "smolvm", classes: []types.ConfinementClass{types.CC3}, refPrefix: "vm-"}
	oci := &freezingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}, refPrefix: "wardyn-agent-"}}
	o := New(vmm, oci)
	sb, err := o.CreateSandbox(ctx, specFor(types.CC1))
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := o.FreezeSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("FreezeSandbox: %v", err)
	}
	if err := o.ThawSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("ThawSandbox: %v", err)
	}
	if err := o.KillSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("KillSandbox after freeze/thaw: %v", err)
	}
	if len(oci.freezes) != 1 || len(oci.thaws) != 1 || len(oci.kills) != 1 || len(vmm.kills) != 0 {
		t.Errorf("docker freezes %v thaws %v kills %v, smolvm kills %v; want each forwarded once to docker and the route kept",
			oci.freezes, oci.thaws, oci.kills, vmm.kills)
	}

	k8s := New(&fakeSubstrate{name: "k8s", classes: []types.ConfinementClass{types.CC1}})
	if err := k8s.FreezeSandbox(ctx, "wardyn-agent-y"); !errors.Is(err, runner.ErrFreezeUnsupported) {
		t.Errorf("FreezeSandbox on a substrate that cannot pause = %v, want ErrFreezeUnsupported", err)
	}
	if err := k8s.ThawSandbox(ctx, "wardyn-agent-y"); !errors.Is(err, runner.ErrFreezeUnsupported) {
		t.Errorf("ThawSandbox on a substrate that cannot pause = %v, want ErrFreezeUnsupported", err)
	}
}

// TestCapabilities_FreezeAggregatesPerClass pins the per-class merge: each
// class's Freeze comes from the substrate routing picks for it (the first to
// list it), so a deployment whose docker substrate has verified only CC1
// never reports Freeze=true for a class it did not claim, and a later
// substrate's claim never vouches for runs routed elsewhere.
func TestCapabilities_FreezeAggregatesPerClass(t *testing.T) {
	oci := &fakeSubstrate{
		name:     "docker",
		classes:  []types.ConfinementClass{types.CC1, types.CC2},
		resolved: map[types.ConfinementClass]string{types.CC1: "oci/runc", types.CC2: "oci/runsc"},
		freeze:   map[types.ConfinementClass]bool{types.CC1: true, types.CC2: false},
	}
	caps, err := New(oci).Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !caps.Freeze[types.CC1] || caps.Freeze[types.CC2] {
		t.Errorf("Freeze = %v, want {CC1:true, CC2:false}", caps.Freeze)
	}

	// k8s lists CC1 first, with no Freeze entry: CC1 runs route there, so
	// docker's later Freeze[CC1]=true must not be reported for them.
	k8s := &fakeSubstrate{name: "k8s", classes: []types.ConfinementClass{types.CC1}}
	caps, err = New(k8s, oci).Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Freeze[types.CC1] {
		t.Errorf("Freeze[CC1] = true, but CC1 routes to k8s, which cannot freeze (Freeze = %v)", caps.Freeze)
	}
}
