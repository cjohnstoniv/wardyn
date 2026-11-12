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
	fenced []string
	gens   []int
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
