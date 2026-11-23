// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestDispatcherChecksRunnerAndDurableGuardBeforeCreate(t *testing.T) {
	id := uuid.NewString()
	fake := runnertest.NewFake()
	guard := errors.New("offline lease unavailable")
	d, err := newTestDispatcher(Config{RunnerID: id, Substrate: fake, BeforeCreate: func(context.Context, runner.SandboxSpec) error { return guard },
		Created: func(context.Context, runner.SandboxSpec, string) error {
			t.Fatal("created after failed guard")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b := runnerwire.Loopback()
	defer b.Close()
	p := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	defer p.Close()
	for _, runnerID := range []string{id, uuid.NewString()} {
		data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: uuid.New(), RunnerID: runnerID}})
		if _, err := d.Call(context.Background(), p, 2, runnerwire.MethodCreateSandbox, data); err == nil {
			t.Fatal("unsafe create accepted")
		}
	}
	if len(fake.Created) != 0 {
		t.Fatal("created before identity/offline checks")
	}
	data, _ := runnerwire.Marshal(runnerwire.RefArgs{Ref: placement.RunnerRefPrefix(uuid.NewString()) + "sandbox"})
	if _, err := d.Call(context.Background(), p, 2, runnerwire.MethodKill, data); err == nil {
		t.Fatal("foreign runner ref accepted")
	}
	if len(fake.Calls) != 0 {
		t.Fatal("foreign ref reached substrate")
	}
}

func TestDispatcherOutputBindingFailureRollsBackSandbox(t *testing.T) {
	fake := runnertest.NewFake()
	id, run := uuid.NewString(), uuid.New()
	failure := errors.New("persist output binding failed")
	d, err := newTestDispatcher(Config{RunnerID: id, Substrate: fake, BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created: func(context.Context, runner.SandboxSpec, string) error { return nil },
		Output:  func(uuid.UUID) (io.Writer, error) { return io.Discard, nil }, BindOutput: func(uuid.UUID, string) error { return failure },
		ForgetOutput: func(uuid.UUID) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	a, b := runnerwire.Loopback()
	defer b.Close()
	p := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	defer p.Close()
	data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: run, RunnerID: id}, ExecOutput: true})
	if _, err := d.Call(context.Background(), p, 2, runnerwire.MethodCreateSandbox, data); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if !fake.Called("kill", "fake-"+run.String()) {
		t.Fatal("unreported sandbox remained running")
	}
}

type cancelAfterCreate struct {
	*runnertest.Fake
	cancel context.CancelFunc
}

func (f cancelAfterCreate) CreateSandbox(ctx context.Context, spec runner.SandboxSpec) (runner.Sandbox, error) {
	sandbox, err := f.Fake.CreateSandbox(ctx, spec)
	f.cancel()
	return sandbox, err
}

func TestDispatcherDurableBindingSurvivesCallCancellation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "persist", true: "rollback"}[fail], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := runnertest.NewFake()
			id, run := uuid.NewString(), uuid.New()
			failure := errors.New("local state could not be persisted")
			bound := false
			d, err := newTestDispatcher(Config{RunnerID: id, Substrate: cancelAfterCreate{fake, cancel},
				BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
				Created: func(persist context.Context, spec runner.SandboxSpec, ref string) error {
					if persist.Err() != nil || ctx.Err() == nil || spec.RunID != run || ref != "fake-"+run.String() {
						t.Fatal("lost local identity or inherited dropped call cancellation")
					}
					if _, bounded := persist.Deadline(); !bounded {
						t.Fatal("durable binding is unbounded")
					}
					bound = true
					if fail {
						return failure
					}
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			a, b := runnerwire.Loopback()
			defer b.Close()
			peer := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
			defer peer.Close()
			data, _ := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: runner.SandboxSpec{RunID: run, RunnerID: id}})
			_, err = d.Call(ctx, peer, 2, runnerwire.MethodCreateSandbox, data)
			if !bound || errors.Is(err, failure) != fail || (err != nil && !fail) {
				t.Fatalf("binding result: bound=%v error=%v", bound, err)
			}
			if fake.Called("kill", "fake-"+run.String()) != fail {
				t.Fatal("sandbox cleanup did not distinguish durable failure from link cancellation")
			}
		})
	}
}

// These tests isolate creation; lifecycle policy is exercised separately.
func newTestDispatcher(cfg Config) (*Dispatcher, error) {
	if cfg.CreateFailed == nil {
		cfg.CreateFailed = func(context.Context, runner.SandboxSpec) error { return nil }
	}
	if cfg.HoldsSandbox == nil {
		cfg.HoldsSandbox = func(string) bool { return true }
	}
	cfg.BeforeRefCall = func(context.Context, string, string, uuid.UUID) error { return nil }
	cfg.AfterRefCall = func(context.Context, string, string, string, uuid.UUID) error { return nil }
	if cfg.ExecutionResult == nil {
		cfg.ExecutionResult = func(context.Context, string, uuid.UUID) (runnerwire.ExecutionResult, error) {
			return runnerwire.ExecutionResult{}, runnerwire.Refuse("execution recovery not configured in this fixture")
		}
	}
	if cfg.MutationFinished == nil {
		cfg.MutationFinished = func(context.Context, string, string, uuid.UUID, uuid.UUID, error) error { return nil }
	}
	return New(cfg)
}

// sweepWithSeedError lists one orphan candidate and answers with an error, as a
// docker substrate does when a leftover drive seed would not be removed.
type sweepWithSeedError struct {
	*runnertest.Fake
	candidate uuid.UUID
}

func (s sweepWithSeedError) SweepOrphanedSandboxes(_ context.Context, _ time.Duration, isOrphan func(uuid.UUID) bool) (int, error) {
	isOrphan(s.candidate)
	return 0, errors.New("docker: remove leftover drive seed: in use")
}

// TestDispatcherSweepListsCandidatesDespiteASeedError: the listing call still
// hands the org its candidates when the same pass failed to remove a drive
// seed, so one stuck seed does not stop every orphan sandbox's removal.
func TestDispatcherSweepListsCandidatesDespiteASeedError(t *testing.T) {
	orphan := uuid.New()
	d, err := newTestDispatcher(Config{RunnerID: uuid.NewString(), Substrate: sweepWithSeedError{Fake: runnertest.NewFake(), candidate: orphan},
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := runnerwire.Marshal(runnerwire.SweepArgs{MinAge: time.Minute})
	res, err := d.sweep(context.Background(), data)
	if err != nil {
		t.Fatalf("listing sweep = %v, want the candidates", err)
	}
	if got := res.(runnerwire.SweepResult).Candidates; len(got) != 1 || got[0] != orphan {
		t.Fatalf("candidates = %v, want [%s]", got, orphan)
	}
}

// callRefArgs sends a ref verb to a dispatcher over a loopback peer.
func callRefArgs(t *testing.T, d *Dispatcher, method string, args any) (any, error) {
	t.Helper()
	a, b := runnerwire.Loopback()
	defer b.Close()
	peer := runnerwire.NewPeer(a, runnerwire.PeerConfig{})
	defer peer.Close()
	data, _ := runnerwire.Marshal(args)
	return d.Call(context.Background(), peer, 1, method, data)
}

func TestDispatch_FenceProxyRoutes(t *testing.T) {
	id := uuid.NewString()
	ref := placement.RunnerRefPrefix(id) + "fake-ref"
	fake := runnertest.NewFake()
	d, err := newTestDispatcher(Config{RunnerID: id, Substrate: fake,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callRefArgs(t, d, runnerwire.MethodFenceProxy, runnerwire.RefArgs{Ref: ref}); err != nil {
		t.Fatalf("fence_proxy = %v; want nil", err)
	}
	if !fake.Called("fence_proxy", "fake-ref") {
		t.Errorf("fence_proxy did not reach the substrate's FenceProxy: %v", fake.Calls)
	}

	// A substrate that cannot cut the agent off says so with the code a runner that cannot end answers.
	bare, err := newTestDispatcher(Config{RunnerID: id, Substrate: struct{ substrate.Substrate }{fake},
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callRefArgs(t, bare, runnerwire.MethodFenceProxy, runnerwire.RefArgs{Ref: ref}); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Errorf("fence_proxy on a substrate without a fence = %v; want runner.ErrEndUnsupported", err)
	}
}

func TestDispatch_GenerationRoutesToAt(t *testing.T) {
	id := uuid.NewString()
	ref := placement.RunnerRefPrefix(id) + "fake-ref"
	cfg := []byte(`{"run_id":"` + uuid.NewString() + `"}`)
	for _, tc := range []struct {
		name, method string
		args         func(gen int) any
		plain, at    string
	}{
		{"stop_proxy", runnerwire.MethodStopProxy, func(g int) any { return runnerwire.RefArgs{Ref: ref, PolicyGeneration: g} }, "stop_proxy", "stop_proxy_at"},
		{"end", runnerwire.MethodEnd, func(g int) any { return runnerwire.RefArgs{Ref: ref, PolicyGeneration: g} }, "end", "end_at"},
		{"replace_proxy", runnerwire.MethodReplaceProxy, func(g int) any {
			return runnerwire.ReplaceProxyArgs{Ref: ref, CfgJSON: cfg, PolicyGeneration: g}
		}, "replace_proxy " + string(cfg), "replace_proxy_at"},
	} {
		for _, gen := range []int{0, 7} {
			t.Run(fmt.Sprintf("%s/gen%d", tc.name, gen), func(t *testing.T) {
				fake := runnertest.NewFake()
				d, err := newTestDispatcher(Config{RunnerID: id, Substrate: fake,
					BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
					Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := callRefArgs(t, d, tc.method, tc.args(gen)); err != nil {
					t.Fatalf("%s at generation %d = %v", tc.method, gen, err)
				}
				wantAt := gen > 0
				if got := fake.Called(tc.at, "fake-ref"); got != wantAt {
					t.Errorf("at-method called = %v; want %v (calls %v)", got, wantAt, fake.Calls)
				}
				if got := fake.Called(tc.plain, "fake-ref"); got == wantAt {
					t.Errorf("plain method called = %v; want %v (calls %v)", got, !wantAt, fake.Calls)
				}
				if wantAt {
					if len(fake.PolicyGens) != 1 || fake.PolicyGens[0].Gen != gen {
						t.Errorf("the substrate saw generations %v; want one call at %d", fake.PolicyGens, gen)
					}
				}
			})
		}
	}
}

// narrowingFake is a substrate that narrows a proxy in place and records what it was asked.
type narrowingFake struct {
	*runnertest.Fake
	read, narrowed [][]string
	state          runner.ProxyNarrowState
	err            error
}

func (n *narrowingFake) ProxyNarrowState(context.Context, string) (runner.ProxyNarrowState, error) {
	n.read = append(n.read, nil)
	return n.state, n.err
}

func (n *narrowingFake) NarrowProxy(_ context.Context, _ string, denies []string) (runner.ProxyNarrowState, error) {
	n.narrowed = append(n.narrowed, denies)
	return n.state, n.err
}

func TestDispatch_NarrowProxyRoutes(t *testing.T) {
	id := uuid.NewString()
	ref := placement.RunnerRefPrefix(id) + "fake-ref"
	cfg := func(sub substrate.Substrate) Config {
		return Config{RunnerID: id, Substrate: sub,
			BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
			Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }}
	}

	// A substrate that cannot narrow answers the code the org reads as "not offered here".
	bare, err := newTestDispatcher(cfg(runnertest.NewFake()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callRefArgs(t, bare, runnerwire.MethodNarrowProxy, runnerwire.NarrowProxyArgs{Ref: ref, Deny: []string{"a.example"}}); !errors.Is(err, runner.ErrNarrowUnsupported) {
		t.Errorf("narrow_proxy on a substrate that cannot narrow = %v; want runner.ErrNarrowUnsupported", err)
	}

	want := runner.ProxyNarrowState{ConfigSHA256: "cfg", BaseSHA256: "base", LiveDenies: []string{"a.example"}, Changed: true, BaseChecked: true}
	n := &narrowingFake{Fake: runnertest.NewFake(), state: want}
	d, err := newTestDispatcher(cfg(n))
	if err != nil {
		t.Fatal(err)
	}
	// An empty Deny reads the list; a non-empty one narrows. BaseChecked survives the answer.
	res, err := callRefArgs(t, d, runnerwire.MethodNarrowProxy, runnerwire.NarrowProxyArgs{Ref: ref})
	if err != nil || len(n.read) != 1 || len(n.narrowed) != 0 {
		t.Fatalf("an empty Deny: %v; reads %d, narrows %d; want one read", err, len(n.read), len(n.narrowed))
	}
	if got := res.(runnerwire.NarrowProxyResult); got.ConfigSHA256 != "cfg" || got.BaseSHA256 != "base" || !got.Changed || !got.BaseChecked || len(got.LiveDenies) != 1 {
		t.Errorf("the read answered %+v; want the substrate's state with BaseChecked", got)
	}
	res, err = callRefArgs(t, d, runnerwire.MethodNarrowProxy, runnerwire.NarrowProxyArgs{Ref: ref, Deny: []string{"a.example", "b.example"}})
	if err != nil || len(n.read) != 1 || len(n.narrowed) != 1 || len(n.narrowed[0]) != 2 || n.narrowed[0][1] != "b.example" {
		t.Fatalf("a Deny list: %v; reads %d, narrows %v; want one narrow of both hosts", err, len(n.read), n.narrowed)
	}
	if !res.(runnerwire.NarrowProxyResult).BaseChecked {
		t.Error("BaseChecked did not survive the narrow's answer")
	}

	// The substrate's error comes back as is, for the wire to map to its code.
	n.err = runner.ErrNarrowNotSent
	if _, err := callRefArgs(t, d, runnerwire.MethodNarrowProxy, runnerwire.NarrowProxyArgs{Ref: ref, Deny: []string{"a.example"}}); !errors.Is(err, runner.ErrNarrowNotSent) {
		t.Errorf("a substrate's ErrNarrowNotSent came back as %v", err)
	}
}

// movingFake is runnertest's fake with a substrate's export and import, recording what reached them.
type movingFake struct {
	*runnertest.Fake
	mu       sync.Mutex
	exported []string
	imported []string
}

func (m *movingFake) ExportState(_ context.Context, ref string, _ runner.CarriedSet) (runner.StateExport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exported = append(m.exported, ref)
	return runner.StateExport{Stream: io.NopCloser(strings.NewReader("exported files")), SpoolDrained: true}, nil
}

func (m *movingFake) ImportSandbox(_ context.Context, spec runner.SandboxSpec, _ runner.CarriedSet, _ int64, carried io.Reader) (runner.Sandbox, runner.CarriedManifest, error) {
	b, err := io.ReadAll(carried)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.imported = append(m.imported, spec.RunID.String()+":"+string(b))
	return runner.Sandbox{Ref: "imported-ref", Driver: "fake"}, runner.CarriedManifest{Entries: 1, Digest: "d"}, err
}

// TestDispatch_MoveMethodsRoute: each of the three move methods reaches its handler through Call's move
// case (the substrate's export and import, and the unbound notice), end to end from the org's client. Without
// the case they fall to refCall and none does. A substrate without an export or import is refused, and so are
// a call the runner cannot serve (no run id, another runner, a VM, no byte bound, a foreign or unproven ref)
// and one the org's client must not send (no MoveState in the caps, a foreign ref, a stream of another kind).
func TestDispatch_MoveMethodsRoute(t *testing.T) {
	runnerID := uuid.NewString()
	sub := &movingFake{Fake: runnertest.NewFake()}
	var unbound []string
	var mu sync.Mutex
	d, err := newTestDispatcher(Config{RunnerID: runnerID, Substrate: sub,
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
		SpoolDrained: func(context.Context, string) (bool, error) { return true, nil },
		MoveUnbound: func(_ context.Context, id uuid.UUID, ref string) error {
			mu.Lock()
			defer mu.Unlock()
			unbound = append(unbound, id.String()+":"+ref)
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	org := movingOrg(t, runnerID, d)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	set := runner.CarriedSet{Roots: []string{"/home/agent/work"}}
	ref := placement.RunnerRefPrefix(runnerID) + "fake-ref"

	out, err := org.ExportState(ctx, ref, set)
	if err != nil {
		t.Fatalf("move_export = %v; want the substrate's export", err)
	}
	got, err := io.ReadAll(out.Stream)
	_ = out.Stream.Close()
	if err != nil || string(got) != "exported files" || !out.SpoolDrained {
		t.Errorf("export stream = %q, %v, drained %v; want the substrate's bytes, drained", got, err, out.SpoolDrained)
	}

	runID := uuid.New()
	sb, m, err := org.ImportSandbox(ctx, runner.SandboxSpec{RunID: runID, RunnerID: runnerID, Image: "agent:1"}, set, 1<<20, strings.NewReader("carried files"))
	if err != nil {
		t.Fatalf("move_import = %v; want the substrate's import", err)
	}
	if sb.Ref != placement.RunnerRefPrefix(runnerID)+"imported-ref" || m.Digest != "d" {
		t.Errorf("import answered %+v, %+v; want the runner's ref and the importer's manifest", sb, m)
	}

	if err := org.MoveUnbound(ctx, runner.MoveUnbound{RunID: runID, Ref: ref}); err != nil {
		t.Fatalf("move_unbound = %v; want nil", err)
	}

	sub.mu.Lock()
	exported, imported := sub.exported, sub.imported
	sub.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	if len(exported) != 1 || exported[0] != "fake-ref" {
		t.Errorf("exports reaching the substrate = %v; want [fake-ref]", exported)
	}
	if len(imported) != 1 || imported[0] != runID.String()+":carried files" {
		t.Errorf("imports reaching the substrate = %v; want the run's carried files", imported)
	}
	if len(unbound) != 1 || unbound[0] != runID.String()+":fake-ref" {
		t.Errorf("notices reaching the runner = %v; want the run's local ref", unbound)
	}

	// A substrate that can neither export nor import refuses both, and touches nothing.
	bare, err := newTestDispatcher(Config{RunnerID: runnerID, Substrate: struct{ substrate.Substrate }{runnertest.NewFake()},
		BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
		Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	borg := movingOrg(t, runnerID, bare)
	var refusal *runnerwire.Error
	if _, err := borg.ExportState(ctx, ref, set); !errors.As(err, &refusal) || refusal.Code != runnerwire.CodeRefused {
		t.Errorf("export on a substrate without one = %v; want a refusal", err)
	}
	if _, _, err := borg.ImportSandbox(ctx, runner.SandboxSpec{RunID: uuid.New(), RunnerID: runnerID}, set, 1<<20, strings.NewReader("x")); !errors.As(err, &refusal) || refusal.Code != runnerwire.CodeRefused {
		t.Errorf("import on a substrate without one = %v; want a refusal", err)
	}

	// The runner refuses a call it cannot serve before any substrate call or stream.
	refused := func(t *testing.T, d *Dispatcher, method string, args any) {
		t.Helper()
		var refusal *runnerwire.Error
		if _, err := callRefArgs(t, d, method, args); !errors.As(err, &refusal) || refusal.Code != runnerwire.CodeRefused {
			t.Fatalf("%s = %v; want a refusal", method, err)
		}
	}
	good := runner.SandboxSpec{RunID: uuid.New(), RunnerID: runnerID, Image: "agent:1"}
	for _, row := range []struct {
		name   string
		method string
		args   any
	}{
		{"an import with no run id", runnerwire.MethodMoveImport, runnerwire.MoveImportArgs{Spec: runner.SandboxSpec{RunnerID: runnerID}, Set: set, MaxBytes: 1 << 20}},
		{"an import for another runner", runnerwire.MethodMoveImport, runnerwire.MoveImportArgs{Spec: runner.SandboxSpec{RunID: uuid.New(), RunnerID: uuid.NewString()}, Set: set, MaxBytes: 1 << 20}},
		{"an import of a VM", runnerwire.MethodMoveImport, runnerwire.MoveImportArgs{Spec: runner.SandboxSpec{RunID: uuid.New(), RunnerID: runnerID, MachineKind: types.MachineLinuxVM}, Set: set, MaxBytes: 1 << 20}},
		{"an import with no byte bound", runnerwire.MethodMoveImport, runnerwire.MoveImportArgs{Spec: good, Set: set}},
		{"an export of another runner's ref", runnerwire.MethodMoveExport, runnerwire.MoveExportArgs{Ref: placement.RunnerRefPrefix(uuid.NewString()) + "fake-ref", Set: set}},
		{"a notice for another runner's ref", runnerwire.MethodMoveUnbound, runner.MoveUnbound{RunID: runID, Ref: placement.RunnerRefPrefix(uuid.NewString()) + "fake-ref"}},
	} {
		t.Run(row.name, func(t *testing.T) { refused(t, d, row.method, row.args) })
	}
	t.Run("an export of a sandbox this runner cannot prove its own", func(t *testing.T) {
		unowned, err := newTestDispatcher(Config{RunnerID: runnerID, Substrate: &movingFake{Fake: runnertest.NewFake()},
			HoldsSandbox: func(string) bool { return false },
			BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
			Created:      func(context.Context, runner.SandboxSpec, string) error { return nil }})
		if err != nil {
			t.Fatal(err)
		}
		refused(t, unowned, runnerwire.MethodMoveExport, runnerwire.MoveExportArgs{Ref: ref, Set: set})
	})

	// The organisation's client refuses before the wire: a runner whose caps lack MoveState, a ref of
	// another runner, and a runner that answers with a stream of another kind.
	t.Run("caps without MoveState", func(t *testing.T) {
		quiet := &movingFake{Fake: runnertest.NewFake()}
		var notices atomic.Int32
		qd, err := newTestDispatcher(Config{RunnerID: runnerID, Substrate: quiet,
			BeforeCreate: func(context.Context, runner.SandboxSpec) error { return nil },
			Created:      func(context.Context, runner.SandboxSpec, string) error { return nil },
			// The runner would record a notice, so only the org's client gate refuses one.
			MoveUnbound: func(context.Context, uuid.UUID, string) error { notices.Add(1); return nil }})
		if err != nil {
			t.Fatal(err)
		}
		org := movingOrg(t, runnerID, qd)
		caps, _ := json.Marshal(runnerwire.Caps{Support: substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1}}})
		org.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
		if _, err := org.ExportState(ctx, ref, set); err == nil {
			t.Error("export to a runner without MoveState was sent")
		}
		if _, _, err := org.ImportSandbox(ctx, good, set, 1<<20, strings.NewReader("x")); err == nil {
			t.Error("import to a runner without MoveState was sent")
		}
		if err := org.MoveUnbound(ctx, runner.MoveUnbound{RunID: runID, Ref: ref}); err == nil {
			t.Error("notice to a runner without MoveState was sent")
		}
		if len(quiet.exported)+len(quiet.imported) != 0 || notices.Load() != 0 {
			t.Errorf("calls reached the runner: exports %v, imports %v, %d notices", quiet.exported, quiet.imported, notices.Load())
		}
	})
	t.Run("a ref of another runner", func(t *testing.T) {
		foreign := placement.RunnerRefPrefix(uuid.NewString()) + "fake-ref"
		if _, err := org.ExportState(ctx, foreign, set); !errors.Is(err, remote.ErrForeignRef) {
			t.Errorf("export of a foreign ref = %v; want ErrForeignRef", err)
		}
		if err := org.MoveUnbound(ctx, runner.MoveUnbound{RunID: runID, Ref: foreign}); !errors.Is(err, remote.ErrForeignRef) {
			t.Errorf("notice for a foreign ref = %v; want ErrForeignRef", err)
		}
	})
	t.Run("a stream of another kind", func(t *testing.T) {
		wrong := wrongKindOrg(t, runnerID)
		if _, err := wrong.ExportState(ctx, ref, set); err == nil || !strings.Contains(err.Error(), "stream") {
			t.Errorf("export answered with an exec stream = %v; want it refused", err)
		}
		if _, _, err := wrong.ImportSandbox(ctx, good, set, 1<<20, strings.NewReader("x")); !errors.Is(err, runner.ErrMoveFiles) ||
			!strings.Contains(err.Error(), `with a "exec" stream`) {
			t.Errorf("import answered with an exec stream = %v; want it refused for the stream's kind", err)
		}
	})
}

// movingOrg is the org's client of d over a loopback link, with caps that say the runner serves moves.
func movingOrg(t *testing.T, runnerID string, d *Dispatcher) *remote.Substrate {
	t.Helper()
	link := &remote.Link{}
	org := remote.New(runnerID, link, remote.Options{})
	caps, _ := json.Marshal(runnerwire.Caps{MoveState: true, Support: substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1}}})
	org.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	orgEnd, runEnd := runnerwire.Loopback()
	op := link.Attach(t.Context(), orgEnd)
	var rp *runnerwire.Peer
	rp = runnerwire.NewPeer(runEnd, runnerwire.PeerConfig{OnCall: func(ctx context.Context, call uint32, method string, raw json.RawMessage) (any, error) {
		return d.Call(ctx, rp, call, method, raw)
	}})
	go rp.Run(t.Context())
	t.Cleanup(func() { rp.Close(); op.Close() })
	return org
}

// wrongKindOrg is the org's client of a runner that answers both move stream calls with an exec stream.
func wrongKindOrg(t *testing.T, runnerID string) *remote.Substrate {
	t.Helper()
	link := &remote.Link{}
	org := remote.New(runnerID, link, remote.Options{})
	caps, _ := json.Marshal(runnerwire.Caps{MoveState: true, Support: substrate.ClassSupport{Classes: []types.ConfinementClass{types.CC1}}})
	org.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	orgEnd, runEnd := runnerwire.Loopback()
	op := link.Attach(t.Context(), orgEnd)
	var rp *runnerwire.Peer
	rp = runnerwire.NewPeer(runEnd, runnerwire.PeerConfig{OnCall: func(ctx context.Context, call uint32, method string, _ json.RawMessage) (any, error) {
		st, err := rp.Open(ctx, runnerwire.Open{Kind: runnerwire.KindExec, Call: call})
		if err != nil {
			return nil, err
		}
		if method == runnerwire.MethodMoveExport {
			return runnerwire.MoveExportResult{Stream: st.ID()}, nil
		}
		return runnerwire.StreamResult{Stream: st.ID()}, nil
	}})
	go rp.Run(t.Context())
	t.Cleanup(func() { rp.Close(); op.Close() })
	return org
}
