// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestEffectiveFoldsDisk: the main drive's size is folded like CPU and memory,
// from the pool, governance and the deployment's ephemeral block, for both run types.
func TestEffectiveFoldsDisk(t *testing.T) {
	const gib = 1024
	src := func(s types.LimitSource, max, def int) SourceLimits {
		out := SourceLimits{Source: s}
		if max > 0 {
			out.DiskMiB.Max = Int(max)
		}
		if def > 0 {
			out.DiskMiB.Default = Int(def)
		}
		return out
	}
	type want struct {
		def, cap       int // 0 with unlimited set
		defUnl, capUnl bool
		defSrc, capSrc types.LimitSource
	}
	for _, tc := range []struct {
		name string
		ss   []SourceLimits
		want want
	}{
		{"a pool cap under the org max is the pool's",
			[]SourceLimits{src(types.LimitSourcePool, 50*gib, 10*gib), src(types.LimitSourceDeployment, 100*gib, 0)},
			want{def: 10 * gib, cap: 50 * gib, defSrc: types.LimitSourcePool, capSrc: types.LimitSourcePool}},
		{"a profile or ceiling under the pool is governance's",
			[]SourceLimits{src(types.LimitSourcePool, 50*gib, 10*gib), src(types.LimitSourceGovernance, 20*gib, 0), src(types.LimitSourceDeployment, 100*gib, 0)},
			want{def: 10 * gib, cap: 20 * gib, defSrc: types.LimitSourcePool, capSrc: types.LimitSourceGovernance}},
		{"the org only is the deployment's",
			[]SourceLimits{src(types.LimitSourceDeployment, 100*gib, 8*gib)},
			want{def: 8 * gib, cap: 100 * gib, defSrc: types.LimitSourceDeployment, capSrc: types.LimitSourceDeployment}},
		{"nothing bounds it: unlimited and unlimited",
			[]SourceLimits{src(types.LimitSourcePool, 0, 0), src(types.LimitSourceDeployment, 0, 0)},
			want{defUnl: true, capUnl: true}},
		{"a pool default beats the deployment fallback",
			[]SourceLimits{src(types.LimitSourcePool, 50*gib, 10*gib), src(types.LimitSourceDeployment, 100*gib, 5*gib)},
			want{def: 10 * gib, cap: 50 * gib, defSrc: types.LimitSourcePool, capSrc: types.LimitSourcePool}},
		{"no default: an untouched run gets the cap",
			[]SourceLimits{src(types.LimitSourceGovernance, 20*gib, 0), src(types.LimitSourceDeployment, 0, 0)},
			want{def: 20 * gib, cap: 20 * gib, defSrc: types.LimitSourceGovernance, capSrc: types.LimitSourceGovernance}},
	} {
		for _, rt := range []types.RunnerPoolRunType{bg, ia} {
			got, err := Effective(rt, tc.ss...)
			if err != nil {
				t.Fatalf("%s/%s: %v", tc.name, rt, err)
			}
			d, w := got.DiskMiB, tc.want
			if d.Default.Value != w.def || d.Default.Unlimited != w.defUnl || d.Cap.Value != w.cap || d.Cap.Unlimited != w.capUnl ||
				d.DefaultSource != w.defSrc || d.CapSource != w.capSrc {
				t.Errorf("%s/%s: disk = %+v, want %+v", tc.name, rt, d, w)
			}
		}
	}
}

// TestPoolSourceCarriesDisk: a run type's disk bound reaches the pool source, and
// a block without one leaves the field unbounded.
func TestPoolSourceCarriesDisk(t *testing.T) {
	l := appServerPool()
	l.Background.DiskMiB = &types.RunnerPoolAmount{Default: 10240, Cap: 51200}
	p := testPool(&l)
	if s, _ := PoolSource(p, bg); s.DiskMiB.Max == nil || *s.DiskMiB.Max != 51200 || *s.DiskMiB.Default != 10240 {
		t.Fatalf("background disk = %+v", s.DiskMiB)
	}
	if s, _ := PoolSource(p, ia); s.DiskMiB != (Bound{}) {
		t.Fatalf("interactive sets no disk bound, got %+v", s.DiskMiB)
	}
	l.Interactive.DiskMiB = &types.RunnerPoolAmount{Default: 2048, Cap: 4096}
	if s, _ := PoolSource(p, ia); *s.DiskMiB.Max != 4096 || *s.DiskMiB.Default != 2048 {
		t.Fatalf("interactive disk = %+v", s.DiskMiB)
	}
}

// TestGovernanceSourceDiskIsAMaximumOnly: the lower of the profile's and the
// ceiling's positive sizes caps it, and governance never names a default.
func TestGovernanceSourceDiskIsAMaximumOnly(t *testing.T) {
	for _, tc := range []struct {
		profile, ceiling int
		want             int // 0: no cap
	}{
		{0, 0, 0}, {8192, 0, 8192}, {0, 4096, 4096}, {8192, 4096, 4096}, {2048, 4096, 2048},
	} {
		s := mustGov(t, types.GovernanceLimits{MaxEphemeralDiskMiB: tc.profile}, types.ResourceLimits{DiskMiB: tc.ceiling}, types.ResourceLimits{})
		if tc.want == 0 && s.DiskMiB.Max != nil || tc.want != 0 && (s.DiskMiB.Max == nil || *s.DiskMiB.Max != tc.want) {
			t.Errorf("profile %d, ceiling %d: max = %v, want %d", tc.profile, tc.ceiling, s.DiskMiB.Max, tc.want)
		}
		if s.DiskMiB.Default != nil {
			t.Errorf("profile %d, ceiling %d: governance named a disk default", tc.profile, tc.ceiling)
		}
	}
}

// TestDeploymentSourceDisk: the org's ephemeral default and maximum are the
// deployment's disk bound; unset (0) is no bound and a negative value is refused.
func TestDeploymentSourceDisk(t *testing.T) {
	d, err := DeploymentSource(depDefault, types.EphemeralProvider{DefaultDiskMiB: 5120, MaxDiskMiB: 102400}, 0, 0)
	if err != nil || *d.DiskMiB.Max != 102400 || *d.DiskMiB.Default != 5120 {
		t.Fatalf("disk = %+v, %v", d.DiskMiB, err)
	}
	if d, err = DeploymentSource(depDefault, types.EphemeralProvider{}, 0, 0); err != nil || d.DiskMiB != (Bound{}) {
		t.Fatalf("an unset org block bounds nothing: %+v, %v", d.DiskMiB, err)
	}
	for _, eph := range []types.EphemeralProvider{{DefaultDiskMiB: -1}, {MaxDiskMiB: -1}} {
		if _, err := DeploymentSource(depDefault, eph, 0, 0); err == nil {
			t.Errorf("%+v was accepted", eph)
		}
	}
}
