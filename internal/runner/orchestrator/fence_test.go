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

// fencingSubstrate is a reviving substrate that fences and acts for a policy generation, and records both.
type fencingSubstrate struct {
	*revivingSubstrate
	fenced  []string
	started []string
	gens    []int
}

func (f *fencingSubstrate) FenceProxy(_ context.Context, ref string) error {
	f.rec(&f.fenced, ref)
	return nil
}

func (f *fencingSubstrate) StopProxyAt(_ context.Context, _ string, gen int) error {
	f.gens = append(f.gens, gen)
	return nil
}

func (f *fencingSubstrate) EndSandboxAt(_ context.Context, _ string, gen int) error {
	f.gens = append(f.gens, gen)
	return nil
}

func (f *fencingSubstrate) ReplaceProxyHashedAt(_ context.Context, _ string, _ []byte, gen int) (string, error) {
	f.gens = append(f.gens, gen)
	return "at", nil
}

// A substrate with generations is sent them; one without is sent the plain verb, and the fence is refused
// where it cannot be served.
func TestOrchestrator_AtMethodsFallBack(t *testing.T) {
	ctx := context.Background()
	const ref = "wardyn-agent-x"

	at := &fencingSubstrate{revivingSubstrate: &revivingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}}}
	o := New(at)
	if err := o.FenceProxy(ctx, ref); err != nil || len(at.fenced) != 1 {
		t.Fatalf("FenceProxy = %v, fenced %v; want it forwarded", err, at.fenced)
	}
	if err := o.StopProxyAt(ctx, ref, 3); err != nil {
		t.Fatal(err)
	}
	if err := o.EndSandboxAt(ctx, ref, 4); err != nil {
		t.Fatal(err)
	}
	if sum, err := o.ReplaceProxyHashedAt(ctx, ref, nil, 5); err != nil || sum != "at" {
		t.Fatalf("ReplaceProxyHashedAt = %q, %v", sum, err)
	}
	if len(at.gens) != 3 || at.gens[0] != 3 || at.gens[1] != 4 || at.gens[2] != 5 {
		t.Errorf("generations forwarded = %v; want [3 4 5]", at.gens)
	}

	both := &plainVerbs{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}}
	po := New(both)
	if err := po.StopProxyAt(ctx, ref, 3); err != nil || both.calls["stop_proxy"] != 1 {
		t.Errorf("StopProxyAt on a plain substrate = %v, calls %v; want the plain StopProxy", err, both.calls)
	}
	if err := po.EndSandboxAt(ctx, ref, 3); err != nil || both.calls["end"] != 1 {
		t.Errorf("EndSandboxAt on a plain substrate = %v, calls %v; want the plain EndSandbox", err, both.calls)
	}
	if sum, err := po.ReplaceProxyHashedAt(ctx, ref, []byte("c"), 3); err != nil || sum != "plain" || both.calls["replace"] != 1 {
		t.Errorf("ReplaceProxyHashedAt on a plain substrate = %q, %v, calls %v; want the plain ReplaceProxyHashed", sum, err, both.calls)
	}
	if err := po.FenceProxy(ctx, ref); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Errorf("FenceProxy on a substrate that cannot fence = %v; want runner.ErrEndUnsupported", err)
	}
}

// plainVerbs has StopProxy, EndSandbox and ReplaceProxyHashed and no generation or fence.
type plainVerbs struct {
	*fakeSubstrate
	calls map[string]int
}

func (p *plainVerbs) count(verb string) {
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[verb]++
}

func (p *plainVerbs) StopProxy(context.Context, string) error  { p.count("stop_proxy"); return nil }
func (p *plainVerbs) EndSandbox(context.Context, string) error { p.count("end"); return nil }
func (p *plainVerbs) CanReplaceProxy(context.Context, string) error {
	return nil
}
func (p *plainVerbs) EnsureProxyImage(context.Context) error { return nil }
func (p *plainVerbs) ReplaceProxy(context.Context, string, []byte) error {
	return nil
}
func (p *plainVerbs) ReplaceProxyHashed(context.Context, string, []byte) (string, error) {
	p.count("replace")
	return "plain", nil
}

func (f *fencingSubstrate) StartFenced(_ context.Context, ref string) error {
	f.rec(&f.started, ref)
	return nil
}

// A fenced start is forwarded to a substrate that has it, and refused where it cannot be served.
func TestOrchestrator_StartFencedForwards(t *testing.T) {
	ctx := context.Background()
	const ref = "wardyn-agent-x"
	at := &fencingSubstrate{revivingSubstrate: &revivingSubstrate{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}}}
	if err := New(at).StartFenced(ctx, ref); err != nil || len(at.started) != 1 {
		t.Fatalf("StartFenced = %v, started %v; want it forwarded", err, at.started)
	}
	plain := &plainVerbs{fakeSubstrate: &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}}
	if err := New(plain).StartFenced(ctx, ref); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Errorf("StartFenced on a substrate without it = %v; want runner.ErrEndUnsupported", err)
	}
}

// lostStopper is a fencing substrate that also keeps a lost run's agent, and records the generation it is sent.
type lostStopper struct {
	*fencingSubstrate
	lost []int
}

func (l *lostStopper) StopLostProxyAt(_ context.Context, _ string, gen int) error {
	l.lost = append(l.lost, gen)
	return nil
}

// A lost-run stop reaches a substrate that keeps a lost run's agent with its generation. Any other substrate is sent
// what the lost-run path sent before: StopProxy for no generation, StopProxyAt otherwise.
func TestOrchestratorStopLostProxyAt_FallsBack(t *testing.T) {
	ctx := context.Background()
	const ref = "wardyn-agent-x"
	sub := func() *fakeSubstrate {
		return &fakeSubstrate{name: "docker", classes: []types.ConfinementClass{types.CC1}}
	}

	lp := &lostStopper{fencingSubstrate: &fencingSubstrate{revivingSubstrate: &revivingSubstrate{fakeSubstrate: sub()}}}
	for _, gen := range []int{0, 6} {
		if err := New(lp).StopLostProxyAt(ctx, ref, gen); err != nil {
			t.Fatalf("StopLostProxyAt(%d) = %v", gen, err)
		}
	}
	if len(lp.lost) != 2 || lp.lost[0] != 0 || lp.lost[1] != 6 || len(lp.gens) != 0 {
		t.Errorf("lost stops = %v, StopProxyAt gens = %v; want [0 6] and none", lp.lost, lp.gens)
	}

	at := &fencingSubstrate{revivingSubstrate: &revivingSubstrate{fakeSubstrate: sub()}}
	if err := New(at).StopLostProxyAt(ctx, ref, 7); err != nil || len(at.gens) != 1 || at.gens[0] != 7 {
		t.Errorf("StopLostProxyAt(7) on a substrate with generations = %v, gens %v; want its StopProxyAt(7)", err, at.gens)
	}

	plain := &plainVerbs{fakeSubstrate: sub()}
	po := New(plain)
	if err := po.StopLostProxyAt(ctx, ref, 0); err != nil || plain.calls["stop_proxy"] != 1 {
		t.Errorf("StopLostProxyAt(0) on a plain substrate = %v, calls %v; want its StopProxy", err, plain.calls)
	}
	if err := po.StopLostProxyAt(ctx, ref, 3); err != nil || plain.calls["stop_proxy"] != 2 {
		t.Errorf("StopLostProxyAt(3) on a plain substrate = %v, calls %v; want its StopProxy", err, plain.calls)
	}

	none := sub()
	if err := New(none).StopLostProxyAt(ctx, ref, 0); !errors.Is(err, runner.ErrEndUnsupported) {
		t.Errorf("StopLostProxyAt on a substrate that cannot stop a proxy = %v; want runner.ErrEndUnsupported", err)
	}
}
