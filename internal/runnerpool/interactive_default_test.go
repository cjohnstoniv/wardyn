// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func interactiveEffective(t *testing.T, rt types.RunnerPoolRunType, maxAgeSec int, others ...SourceLimits) types.EffectiveRunLimits {
	t.Helper()
	dep, err := DeploymentSource(depDefault, types.EphemeralProvider{}, maxAgeSec, 0)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Effective(rt, append(others, WithInteractiveLifetimeDefault(dep, rt))...)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	return e
}

// TestInteractiveLifetimeDefault: an Interactive run no source names a lifetime for
// gets 24 hours, never an unlimited default (O7-B, probe P1 of the pool-limits
// review: a pool with no Limits block under a profile with no default end). A
// Background run keeps an unlimited default, and nothing about No end changes.
func TestInteractiveLifetimeDefault(t *testing.T) {
	const day = 24 * 60 * 60
	if InteractiveDefaultLifetimeSec != day {
		t.Fatalf("the Interactive fallback is %d s, want 24 hours", InteractiveDefaultLifetimeSec)
	}
	noEnd := true
	govOffersNoEnd := SourceLimits{Source: types.LimitSourceGovernance, NoEnd: &noEnd}
	unlimitedPool := SourceLimits{Source: types.LimitSourcePool}

	for _, c := range []struct {
		name       string
		rt         types.RunnerPoolRunType
		maxAge     int
		others     []SourceLimits
		want       int // 0 is an unlimited default
		wantSource types.LimitSource
	}{
		{"interactive, deployment alone", ia, 0, nil, day, types.LimitSourceDeployment},
		{"interactive, a pool with no limits block and a profile with no default", ia, 0, []SourceLimits{unlimitedPool, govOffersNoEnd}, day, types.LimitSourceDeployment},
		{"background keeps an unlimited default", bg, 0, []SourceLimits{unlimitedPool, govOffersNoEnd}, 0, ""},
		{"a pool default beats the fallback", ia, 0, []SourceLimits{{Source: types.LimitSourcePool, LifetimeSec: Bound{Default: Int(7200)}}}, 7200, types.LimitSourcePool},
		{"a profile default beats the fallback", ia, 0, []SourceLimits{{Source: types.LimitSourceGovernance, LifetimeSec: Bound{Default: Int(3 * day)}}}, 3 * day, types.LimitSourceGovernance},
		{"a maximum below it wins", ia, 0, []SourceLimits{{Source: types.LimitSourcePool, LifetimeSec: Bound{Max: Int(3600)}}}, 3600, types.LimitSourcePool},
		{"the deployment's age cap below it wins", ia, 1800, nil, 1800, types.LimitSourceDeployment},
	} {
		e := interactiveEffective(t, c.rt, c.maxAge, c.others...)
		if c.want == 0 {
			if !e.Lifetime.Default.Unlimited {
				t.Errorf("%s: default = %+v, want unlimited", c.name, e.Lifetime.Default)
			}
			continue
		}
		if e.Lifetime.Default.Unlimited || e.Lifetime.Default.Value != c.want || e.Lifetime.DefaultSource != c.wantSource {
			t.Errorf("%s: default = %+v from %q, want %d from %q", c.name, e.Lifetime.Default, e.Lifetime.DefaultSource, c.want, c.wantSource)
		}
	}

	// The default is not a maximum and not a No end ruling.
	e := interactiveEffective(t, ia, 0, unlimitedPool, govOffersNoEnd)
	if !e.Lifetime.Cap.Unlimited || !e.NoEndAllowed {
		t.Errorf("the fallback changed the cap or No end: cap %+v, no end allowed %v", e.Lifetime.Cap, e.NoEndAllowed)
	}
}
