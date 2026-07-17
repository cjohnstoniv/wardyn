// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// These are the counterfactuals for the crown's confirmed policy-aliasing
// cluster: resolvePolicy used to hand out a SHALLOW copy of the process-global
// cfg.DefaultPolicy, so every caller's slice header pointed at the global's
// backing array. Two concurrent create-runs appending run-specific egress then
// raced the same spare-capacity element (one run's domain replacing another's
// in the allowlist handed to its proxy), and any in-place mutation leaked into
// every later run. resolvePolicy now Clones.

// defaultPolicyWithSpareCapacity mirrors the shipped policies' real shape: a
// decoded AllowedDomains slice with spare capacity (default.json decodes to
// len=15 cap=16), which is what made the aliased append silently land in the
// global rather than reallocate.
func defaultPolicyWithSpareCapacity() types.RunPolicySpec {
	domains := make([]string, 1, 4) // len 1, cap 4 => 3 spare slots
	domains[0] = "api.anthropic.com"
	return types.RunPolicySpec{
		AllowedDomains:      domains,
		MinConfinementClass: types.CC1,
		EligibleGrants:      []types.GrantSpec{{Kind: "github_token"}},
	}
}

// TestResolvePolicy_DoesNotAliasDefaultPolicy pins the root cause: the returned
// spec must share no backing array with the global, so a per-run append can
// never be visible to another run.
func TestResolvePolicy_DoesNotAliasDefaultPolicy(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	cfg.DefaultPolicy = defaultPolicyWithSpareCapacity()
	srv := New(cfg)

	specA, _, err := srv.resolvePolicy(context.Background(), nil)
	if err != nil {
		t.Fatalf("resolvePolicy: %v", err)
	}
	specB, _, err := srv.resolvePolicy(context.Background(), nil)
	if err != nil {
		t.Fatalf("resolvePolicy: %v", err)
	}

	// Two resolutions must not share storage (pre-fix: &a[0] == &b[0]).
	if &specA.AllowedDomains[0] == &specB.AllowedDomains[0] {
		t.Fatal("resolvePolicy returned two specs sharing one AllowedDomains backing array — a per-run append will corrupt a sibling run")
	}

	// Run A's append must be invisible to run B AND to the global.
	unionAllowedDomains(&specA, []string{"a-only.example"})
	for _, d := range specB.AllowedDomains {
		if d == "a-only.example" {
			t.Error("run A's egress domain leaked into run B's allowlist")
		}
	}
	for _, d := range srv.cfg.DefaultPolicy.AllowedDomains {
		if d == "a-only.example" {
			t.Error("run A's egress domain leaked into the process-global default policy")
		}
	}

	// In-place mutation of one spec's grants must not strip the global's (the
	// preflight dry-run "persists nothing" promise).
	specA.EligibleGrants = specA.EligibleGrants[:0]
	if len(srv.cfg.DefaultPolicy.EligibleGrants) != 1 {
		t.Errorf("mutating a resolved spec's grants changed the global default policy: %v", srv.cfg.DefaultPolicy.EligibleGrants)
	}
}

// TestResolvePolicy_ConcurrentUnionNoRace is the -race counterfactual: N
// goroutines resolving the default policy and appending their own egress domain
// concurrently. Pre-fix this fired "DATA RACE ... unionAllowedDomains (write)"
// and one run's domain would replace another's. Each run must end up with
// exactly its own domain and no sibling's.
func TestResolvePolicy_ConcurrentUnionNoRace(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, nil)
	cfg.DefaultPolicy = defaultPolicyWithSpareCapacity()
	srv := New(cfg)

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	got := make([][]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			spec, _, err := srv.resolvePolicy(context.Background(), nil)
			if err != nil {
				return
			}
			mine := string(rune('a'+i)) + ".example"
			unionAllowedDomains(&spec, []string{mine})
			got[i] = spec.AllowedDomains
		}(i)
	}
	close(start)
	wg.Wait()

	for i, doms := range got {
		mine := string(rune('a'+i)) + ".example"
		var haveMine bool
		for _, d := range doms {
			if d == mine {
				haveMine = true
				continue
			}
			// Any OTHER run's domain showing up here is the cross-run swap.
			for j := 0; j < n; j++ {
				if j != i && d == string(rune('a'+j))+".example" {
					t.Errorf("run %d's allowlist contains run %d's domain %q — concurrent runs shared a backing array", i, j, d)
				}
			}
		}
		if !haveMine {
			t.Errorf("run %d lost its own egress domain %q (overwritten by a sibling)", i, mine)
		}
	}
}
