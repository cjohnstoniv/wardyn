// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	bg = types.RunnerPoolRunBackground
	ia = types.RunnerPoolRunInteractive
)

var allSources = []types.LimitSource{
	types.LimitSourcePool, types.LimitSourceGovernance, types.LimitSourceDeployment, types.LimitSourceRunnerCapacity,
}

// field is one of the four per-run fields, with how to set a bound on it and read its answer.
type field struct {
	name string
	set  func(*SourceLimits) *Bound
	get  func(types.EffectiveRunLimits) types.EffectiveLimit
}

var fields = []field{
	{"cpu_millis", func(s *SourceLimits) *Bound { return &s.CPUMillis }, func(e types.EffectiveRunLimits) types.EffectiveLimit { return e.CPUMillis }},
	{"memory_mib", func(s *SourceLimits) *Bound { return &s.MemoryMiB }, func(e types.EffectiveRunLimits) types.EffectiveLimit { return e.MemoryMiB }},
	{"lifetime_sec", func(s *SourceLimits) *Bound { return &s.LifetimeSec }, func(e types.EffectiveRunLimits) types.EffectiveLimit { return e.Lifetime }},
	{"idle_sec", func(s *SourceLimits) *Bound { return &s.IdleSec }, func(e types.EffectiveRunLimits) types.EffectiveLimit { return *e.Idle }},
}

func mustEffective(t *testing.T, rt types.RunnerPoolRunType, ss ...SourceLimits) types.EffectiveRunLimits {
	t.Helper()
	e, err := Effective(rt, ss...)
	if err != nil {
		t.Fatalf("Effective: %v", err)
	}
	return e
}

func TestEffectiveEveryFieldTakesTheStrictestSource(t *testing.T) {
	for _, f := range fields {
		for _, winner := range allSources {
			var ss []SourceLimits
			for i, src := range allSources {
				s := SourceLimits{Source: src}
				max := 20 + 10*i
				if src == winner {
					max = 10
				}
				f.set(&s).Max = Int(max)
				ss = append(ss, s)
			}
			for _, order := range [][]SourceLimits{ss, reversed(ss)} {
				got := f.get(mustEffective(t, ia, order...))
				if got.Cap.Value != 10 || got.Cap.Unlimited || got.CapSource != winner {
					t.Errorf("%s: %s is the lowest, cap = %+v from %q", f.name, winner, got.Cap, got.CapSource)
				}
				if got.Default.Value != 10 || got.DefaultSource != winner {
					t.Errorf("%s: with no default named, the untouched run gets the cap: %+v from %q", f.name, got.Default, got.DefaultSource)
				}
			}
		}
	}
}

func reversed(ss []SourceLimits) []SourceLimits {
	out := slices.Clone(ss)
	slices.Reverse(out)
	return out
}

// TestEffectiveTieNamesTheMostSpecificSource: equal bounds name the pool, then
// governance, whatever order the sources arrive in.
func TestEffectiveTieNamesTheMostSpecificSource(t *testing.T) {
	for _, f := range fields {
		var ss []SourceLimits
		for _, src := range allSources {
			s := SourceLimits{Source: src}
			f.set(&s).Max = Int(50)
			ss = append(ss, s)
		}
		if got := f.get(mustEffective(t, ia, reversed(ss)...)); got.CapSource != types.LimitSourcePool {
			t.Errorf("%s: a four-way tie is bound by %q, want the pool", f.name, got.CapSource)
		}
		if got := f.get(mustEffective(t, ia, reversed(ss[1:])...)); got.CapSource != types.LimitSourceGovernance {
			t.Errorf("%s: a tie without the pool is bound by %q, want governance", f.name, got.CapSource)
		}
	}
}

// TestEffectiveUnlimitedOnlyWhenEverySourceAllowsIt covers each field: nothing
// bounding is the only way to unlimited, and one finite bound makes it finite.
func TestEffectiveUnlimitedOnlyWhenEverySourceAllowsIt(t *testing.T) {
	for _, f := range fields {
		none := f.get(mustEffective(t, ia))
		if !none.Cap.Unlimited || !none.Default.Unlimited || none.CapSource != "" || none.DefaultSource != "" {
			t.Errorf("%s: no source is unlimited with no provenance, got %+v", f.name, none)
		}
		open := f.get(mustEffective(t, ia, SourceLimits{Source: types.LimitSourcePool}, SourceLimits{Source: types.LimitSourceGovernance}))
		if !open.Cap.Unlimited {
			t.Errorf("%s: sources that set no maximum leave it unlimited, got %+v", f.name, open)
		}
		for _, bounder := range allSources {
			var ss []SourceLimits
			for _, src := range allSources {
				s := SourceLimits{Source: src}
				if src == bounder {
					f.set(&s).Max = Int(900)
				}
				ss = append(ss, s)
			}
			got := f.get(mustEffective(t, ia, ss...))
			if got.Cap.Unlimited || got.Cap.Value != 900 || got.CapSource != bounder {
				t.Errorf("%s: only %s bounds it, got %+v from %q", f.name, bounder, got.Cap, got.CapSource)
			}
		}
	}
}

func TestEffectiveDefaults(t *testing.T) {
	for _, f := range fields {
		pool := SourceLimits{Source: types.LimitSourcePool}
		*f.set(&pool) = Bound{Max: Int(100), Default: Int(60)}
		gov := SourceLimits{Source: types.LimitSourceGovernance}
		*f.set(&gov) = Bound{Max: Int(80), Default: Int(40)}
		// the lowest default wins, from whichever source named it
		if got := f.get(mustEffective(t, ia, pool, gov)); got.Default.Value != 40 || got.DefaultSource != types.LimitSourceGovernance || got.Cap.Value != 80 {
			t.Errorf("%s: lowest default = %+v from %q, cap %+v", f.name, got.Default, got.DefaultSource, got.Cap)
		}
		// a default past the effective cap is cut to it, and the cap's source is named
		gov2 := SourceLimits{Source: types.LimitSourceGovernance}
		*f.set(&gov2) = Bound{Max: Int(30)}
		got := f.get(mustEffective(t, ia, pool, gov2))
		if got.Default.Value != 30 || got.DefaultSource != types.LimitSourceGovernance || got.Cap.Value != 30 {
			t.Errorf("%s: a default past the cap = %+v from %q", f.name, got.Default, got.DefaultSource)
		}
		// a default with no maximum anywhere stands, unlimited cap
		open := SourceLimits{Source: types.LimitSourcePool}
		f.set(&open).Default = Int(7)
		if got := f.get(mustEffective(t, ia, open)); got.Default.Value != 7 || got.Default.Unlimited || !got.Cap.Unlimited || got.DefaultSource != types.LimitSourcePool {
			t.Errorf("%s: a default under an unlimited cap = %+v", f.name, got)
		}
	}
}

func TestEffectiveIdleIsInteractiveOnly(t *testing.T) {
	idle := SourceLimits{Source: types.LimitSourcePool, IdleSec: Bound{Max: Int(600)}}
	if e := mustEffective(t, bg, idle); e.Idle != nil {
		t.Fatalf("a Background run has no idle stop, got %+v", e.Idle)
	}
	if e := mustEffective(t, ia, idle); e.Idle == nil || e.Idle.Cap.Value != 600 {
		t.Fatalf("an Interactive run's idle stop = %+v", e.Idle)
	}
}

func TestEffectiveRefusesWhatItCannotRead(t *testing.T) {
	zero := SourceLimits{Source: types.LimitSourcePool, CPUMillis: Bound{Max: Int(0)}}
	for name, run := range map[string]func() error{
		"a zero maximum is not unlimited": func() error { _, err := Effective(bg, zero); return err },
		"a negative default": func() error {
			_, err := Effective(bg, SourceLimits{Source: types.LimitSourcePool, MemoryMiB: Bound{Default: Int(-1)}})
			return err
		},
		"a zero concurrency cap": func() error {
			_, err := Effective(bg, SourceLimits{Source: types.LimitSourcePool, ConcurrentRuns: Int(0)})
			return err
		},
		"an unknown source": func() error { _, err := Effective(bg, SourceLimits{Source: "operator"}); return err },
		"an unset run type": func() error { _, err := Effective(""); return err },
		"another run type":  func() error { _, err := Effective("batch"); return err },
	} {
		if run() == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestEffectiveConcurrencyIsListedNeverFolded: each source counts a different set of
// runs, so the answer keeps them apart.
func TestEffectiveConcurrencyIsListedNeverFolded(t *testing.T) {
	e := mustEffective(t, bg,
		SourceLimits{Source: types.LimitSourceDeployment, ConcurrentRuns: Int(100)},
		SourceLimits{Source: types.LimitSourceGovernance, ConcurrentRuns: Int(3)},
		SourceLimits{Source: types.LimitSourcePool, ConcurrentRuns: Int(20)},
		SourceLimits{Source: types.LimitSourceRunnerCapacity},
	)
	want := []types.ConcurrencyBound{
		{Source: types.LimitSourcePool, Max: 20}, {Source: types.LimitSourceGovernance, Max: 3}, {Source: types.LimitSourceDeployment, Max: 100},
	}
	if !slices.Equal(e.Concurrency, want) {
		t.Fatalf("concurrency = %+v, want %+v", e.Concurrency, want)
	}
	if none := mustEffective(t, bg); none.Concurrency == nil || len(none.Concurrency) != 0 {
		t.Fatalf("no cap is an empty list, not null: %#v", none.Concurrency)
	}
}

func testPool(l *types.RunnerPoolLimits) types.RunnerPool {
	return types.RunnerPool{ID: uuid.New(), Name: "App servers", HostingType: types.RunnerPoolRemoteProvided, State: types.RunnerPoolActive, Revision: 1, Limits: l}
}

func appServerPool() types.RunnerPoolLimits {
	return types.RunnerPoolLimits{
		Background: &types.RunnerPoolBackgroundLimits{
			CPUMillis: types.RunnerPoolAmount{Default: 2000, Cap: 8000},
			MemoryMiB: types.RunnerPoolAmount{Default: 4096, Cap: 16384},
			Lifetime:  types.RunnerPoolDuration{Unlimited: true},
		},
		Interactive: &types.RunnerPoolInteractiveLimits{
			CPUMillis: types.RunnerPoolAmount{Default: 1000, Cap: 4000},
			MemoryMiB: types.RunnerPoolAmount{Default: 2048, Cap: 8192},
			Lifetime:  types.RunnerPoolDuration{DefaultSec: 28800, MaxSec: 86400},
			Idle:      types.RunnerPoolDuration{DefaultSec: 1800, MaxSec: 7200},
		},
		Barriers: []types.ConfinementClass{types.CC2, types.CC3},
	}
}

func TestPoolSource(t *testing.T) {
	l := appServerPool()
	p := testPool(&l)

	s, ref := PoolSource(p, bg)
	if ref != nil || s.Source != types.LimitSourcePool || *s.CPUMillis.Max != 8000 || *s.CPUMillis.Default != 2000 {
		t.Fatalf("background source = %+v, %v", s, ref)
	}
	if s.LifetimeSec.Max != nil || s.LifetimeSec.Default != nil {
		t.Fatalf("an unlimited lifetime with default 0 names no maximum and no default: %+v", s.LifetimeSec)
	}
	if s.IdleSec != (Bound{}) {
		t.Fatalf("a background source carries no idle: %+v", s.IdleSec)
	}

	s, _ = PoolSource(p, ia)
	if *s.IdleSec.Max != 7200 || *s.IdleSec.Default != 1800 || *s.LifetimeSec.Max != 86400 {
		t.Fatalf("interactive source = %+v", s)
	}

	// "never" is Unlimited: no maximum, and a default of 0 is no default
	l.Interactive.Idle = types.RunnerPoolDuration{Unlimited: true}
	if s, _ = PoolSource(p, ia); s.IdleSec != (Bound{}) {
		t.Fatalf("never-idle names no bound: %+v", s.IdleSec)
	}

	if s, ref = PoolSource(testPool(nil), bg); ref != nil || s.Source != types.LimitSourcePool || s.CPUMillis != (Bound{}) {
		t.Fatalf("a pool with no limits adds nothing: %+v, %v", s, ref)
	}
	n := 4
	l.MaxConcurrentRuns = &n
	if s, _ = PoolSource(p, bg); *s.ConcurrentRuns != 4 {
		t.Fatal("the pool's concurrency cap reaches its source")
	}
}

func TestPoolSourceRefusesARunTypeThePoolDoesNotTake(t *testing.T) {
	l := appServerPool()
	l.Interactive = nil
	_, ref := PoolSource(testPool(&l), ia)
	if ref == nil || ref.Reason != ReasonRunTypeNotAllowed || ref.Message() != "App servers doesn't take interactive environments. Choose another pool or run type." {
		t.Fatalf("refusal = %+v", ref)
	}
	l = appServerPool()
	l.Background = nil
	if _, ref = PoolSource(testPool(&l), bg); ref == nil || !strings.Contains(ref.Message(), "background tasks") {
		t.Fatalf("refusal = %+v", ref)
	}
	if _, ref = PoolSource(testPool(&l), "batch"); ref == nil {
		t.Fatal("a run type outside the set is never allowed")
	}
}

func TestCheckBarrier(t *testing.T) {
	l := appServerPool()
	p := testPool(&l)
	if CheckBarrier(p, types.CC2) != nil || CheckBarrier(p, types.CC3) != nil {
		t.Fatal("an allowed barrier is not refused")
	}
	ref := CheckBarrier(p, types.CC1)
	if ref == nil || ref.Reason != ReasonBarrierNotAllowed || ref.Message() != "App servers doesn't allow the Fence barrier. It allows Wall and Vault." {
		t.Fatalf("refusal = %+v", ref)
	}
	if CheckBarrier(p, "") == nil {
		t.Fatal("an unset barrier is not an allowed one")
	}
	if CheckBarrier(testPool(nil), types.CC1) != nil {
		t.Fatal("a pool with no limits allows every barrier")
	}
	l.Barriers = []types.ConfinementClass{types.CC3}
	if got := CheckBarrier(p, types.CC1).Message(); !strings.HasSuffix(got, "It allows Vault.") {
		t.Fatalf("one allowed barrier: %q", got)
	}
}

func TestAtCapacity(t *testing.T) {
	l := appServerPool()
	p := testPool(&l)
	if AtCapacity(p, 1_000_000) != nil {
		t.Fatal("no cap, no queueing")
	}
	n := 3
	l.MaxConcurrentRuns = &n
	for running, refused := range map[int]bool{0: false, 2: false, 3: true, 4: true} {
		ref := AtCapacity(p, running)
		if (ref != nil) != refused {
			t.Errorf("%d running against a cap of 3: %+v", running, ref)
		}
		if ref != nil && (ref.Reason != ReasonAtCapacity || ref.Message() != "App servers is running its limit of 3. This run starts when one finishes.") {
			t.Errorf("queue decision = %+v", ref)
		}
	}
	if AtCapacity(testPool(nil), 99) != nil {
		t.Fatal("a pool with no limits has no cap")
	}
}

func TestGovernanceSource(t *testing.T) {
	var zero types.GovernanceLimits
	if s := GovernanceSource(zero, types.ResourceLimits{}, types.ResourceLimits{}); s.Source != types.LimitSourceGovernance || s.CPUMillis != (Bound{}) || s.ConcurrentRuns != nil {
		t.Fatalf("an unrestricted profile sets no size or count bound: %+v", s)
	}
	capped := types.GovernanceLimits{MaxCPUMillis: 4000, MaxMemoryMiB: 8192, MaxConcurrentRuns: 3}
	capped.MaxEndAheadSec, capped.DefaultEndSec = 7200, 3600
	size := types.ResourceLimits{CPUMillis: 4000, MemoryMiB: 8192}
	none := types.ResourceLimits{}
	s := GovernanceSource(capped, size, none)
	if *s.CPUMillis.Max != 4000 || *s.MemoryMiB.Max != 8192 || *s.LifetimeSec.Max != 7200 || *s.LifetimeSec.Default != 3600 || *s.ConcurrentRuns != 3 {
		t.Fatalf("source = %+v", s)
	}
	capped.DefaultEndSec = 0
	if s = GovernanceSource(capped, size, none); *s.LifetimeSec.Default != 7200 {
		t.Fatal("with no default end, a new run's end is the maximum")
	}
	if s.IdleSec != (Bound{}) {
		t.Fatal("governance has no idle-stop ceiling")
	}
	// offering No end does not lift a finite maximum; the allowance without the gate is no offer
	capped.AllowNoEnd, capped.UserChangesLimits = true, true
	if s = GovernanceSource(capped, size, none); s.LifetimeSec.Max == nil || *s.LifetimeSec.Max != 7200 || !*s.NoEnd {
		t.Fatalf("a finite maximum survives an offer of No end: %+v", s.LifetimeSec)
	}
	capped.UserChangesLimits = false
	if s = GovernanceSource(capped, size, none); *s.NoEnd {
		t.Fatal("No end without the change-limits gate is not an offer")
	}
}

func TestRunnerCapacitySource(t *testing.T) {
	s, err := RunnerCapacitySource(RunnerCapacity{8000, 16384}, RunnerCapacity{4000, 32768}, RunnerCapacity{6000, 8192})
	if err != nil || s.Source != types.LimitSourceRunnerCapacity || *s.CPUMillis.Max != 4000 || *s.MemoryMiB.Max != 8192 {
		t.Fatalf("the weakest candidate bounds each field: %+v, %v", s, err)
	}
	if s.LifetimeSec != (Bound{}) || s.IdleSec != (Bound{}) {
		t.Fatal("a runner's capacity says nothing about time")
	}
	for name, c := range map[string][]RunnerCapacity{
		"no candidate":           nil,
		"zero cpu":               {{8000, 16384}, {0, 16384}},
		"zero memory":            {{8000, 0}},
		"a negative report":      {{-1, 100}},
		"an unreported memory":   {{4000, 0}, {8000, 8192}},
		"one unknown among many": {{8000, 8192}, {8000, 8192}, {0, 0}},
	} {
		if _, err := RunnerCapacitySource(c...); err == nil {
			t.Errorf("%s was read as a capacity", name)
		}
	}
}

// TestEffectiveOwnerScenarios walks the owner's examples through every source.
func TestEffectiveOwnerScenarios(t *testing.T) {
	app := appServerPool()
	pool := testPool(&app)
	poolSrc := func(rt types.RunnerPoolRunType) SourceLimits {
		s, ref := PoolSource(pool, rt)
		if ref != nil {
			t.Fatal(ref)
		}
		return s
	}

	// An indefinite pool needs unlimited lifetime in the person's governance too.
	var open types.GovernanceLimits
	open.AllowNoEnd, open.UserChangesLimits = true, true
	if e := mustEffective(t, bg, poolSrc(bg), GovernanceSource(open, types.ResourceLimits{}, types.ResourceLimits{})); !e.Lifetime.Cap.Unlimited || !e.NoEndAllowed {
		t.Fatalf("unlimited pool + governance that offers No end = %+v, no end %v", e.Lifetime, e.NoEndAllowed)
	}
	if e := mustEffective(t, bg, poolSrc(bg), GovernanceSource(types.GovernanceLimits{}, types.ResourceLimits{}, types.ResourceLimits{})); e.NoEndAllowed || e.Lifetime.Cap.Unlimited {
		t.Fatalf("unlimited pool + governance that does not offer No end must not give it: %+v, no end %v", e.Lifetime, e.NoEndAllowed)
	}
	bounded := types.GovernanceLimits{}
	bounded.MaxEndAheadSec = 43200
	e := mustEffective(t, bg, poolSrc(bg), GovernanceSource(bounded, types.ResourceLimits{}, types.ResourceLimits{}))
	if e.Lifetime.Cap.Unlimited || e.Lifetime.Cap.Value != 43200 || e.Lifetime.CapSource != types.LimitSourceGovernance {
		t.Fatalf("a pool never widens governance: %+v", e.Lifetime)
	}
	if e.Lifetime.Default.Value != 43200 || e.Lifetime.DefaultSource != types.LimitSourceGovernance {
		t.Fatalf("an unlimited pool's no-end default is cut to governance's end: %+v", e.Lifetime)
	}
	// ...and to the deployment's maximum age.
	e = mustEffective(t, bg, poolSrc(bg), GovernanceSource(open, types.ResourceLimits{}, types.ResourceLimits{}),
		SourceLimits{Source: types.LimitSourceDeployment, LifetimeSec: Bound{Max: Int(604800)}})
	if e.Lifetime.Cap.Value != 604800 || e.Lifetime.CapSource != types.LimitSourceDeployment {
		t.Fatalf("the deployment's cap binds an unlimited pool: %+v", e.Lifetime)
	}

	// Governance's smaller CPU wins over the pool's; the pool's smaller memory wins over governance's.
	gov := GovernanceSource(types.GovernanceLimits{MaxCPUMillis: 4000, MaxMemoryMiB: 32768}, types.ResourceLimits{}, types.ResourceLimits{})
	e = mustEffective(t, bg, poolSrc(bg), gov)
	if e.CPUMillis.Cap.Value != 4000 || e.CPUMillis.CapSource != types.LimitSourceGovernance ||
		e.MemoryMiB.Cap.Value != 16384 || e.MemoryMiB.CapSource != types.LimitSourcePool {
		t.Fatalf("strictest wins per field: cpu %+v memory %+v", e.CPUMillis, e.MemoryMiB)
	}
	if e.CPUMillis.Default.Value != 2000 || e.CPUMillis.DefaultSource != types.LimitSourcePool {
		t.Fatalf("the pool's default stands under governance's higher cap: %+v", e.CPUMillis)
	}

	// A self-hosted pool is additionally capped by its runner's reported capacity.
	cap4, err := RunnerCapacitySource(RunnerCapacity{4000, 4096}, RunnerCapacity{16000, 65536})
	if err != nil {
		t.Fatal(err)
	}
	e = mustEffective(t, bg, poolSrc(bg), cap4)
	if e.CPUMillis.Cap.Value != 4000 || e.CPUMillis.CapSource != types.LimitSourceRunnerCapacity ||
		e.MemoryMiB.Cap.Value != 4096 || e.MemoryMiB.CapSource != types.LimitSourceRunnerCapacity ||
		e.MemoryMiB.Default.Value != 4096 || e.MemoryMiB.DefaultSource != types.LimitSourcePool {
		t.Fatalf("runner capacity: cpu %+v memory %+v", e.CPUMillis, e.MemoryMiB)
	}

	// An Interactive run: the pool's idle stop, never governance's pause.
	e = mustEffective(t, ia, poolSrc(ia), GovernanceSource(types.GovernanceLimits{RunLimits: types.RunLimits{PauseIdleAfterSec: 60}}, types.ResourceLimits{}, types.ResourceLimits{}))
	if e.Idle == nil || e.Idle.Cap.Value != 7200 || e.Idle.Default.Value != 1800 || e.Idle.CapSource != types.LimitSourcePool {
		t.Fatalf("idle = %+v", e.Idle)
	}
}

// TestEffectiveMemberCapIsTheProfileMaximumWhenTheCeilingSetsNoSize pins the
// #1949 member cap: a profile's CPU or memory maximum counts as the policy
// ceiling's size. With no ceiling size set, the maximum IS the size, so a maximum
// above the deployment's default (2000 CPU, 4096 MiB here) raises the cap to it;
// with both set, the lower of the two; and the cap never falls below what an
// untouched run already gets.
func TestEffectiveMemberCapIsTheProfileMaximumWhenTheCeilingSetsNoSize(t *testing.T) {
	untouched := types.ResourceLimits{CPUMillis: 2000, MemoryMiB: 4096}
	deployment := SourceLimits{Source: types.LimitSourceDeployment,
		CPUMillis: Bound{Max: Int(types.RunnerPoolCPUMillisMax), Default: Int(2000)},
		MemoryMiB: Bound{Max: Int(types.RunnerPoolMemoryMiBMax), Default: Int(4096)}}
	cases := []struct {
		name             string
		ceiling          types.ResourceLimits
		maxCPU, maxMem   int
		wantCPU, wantMem int
	}{
		{"only a maximum above the default", types.ResourceLimits{}, 8000, 16384, 8000, 16384},
		{"a ceiling below the maximum", types.ResourceLimits{CPUMillis: 6000, MemoryMiB: 12288}, 8000, 16384, 6000, 12288},
		{"a maximum below the ceiling", types.ResourceLimits{CPUMillis: 6000, MemoryMiB: 12288}, 3000, 8192, 3000, 8192},
		{"a ceiling and no maximum", types.ResourceLimits{CPUMillis: 6000, MemoryMiB: 12288}, 0, 0, 6000, 12288},
		{"neither: the untouched size", types.ResourceLimits{}, 0, 0, 2000, 4096},
		{"a maximum under the untouched size never cuts below it", types.ResourceLimits{}, 1000, 2048, 2000, 4096},
	}
	for _, c := range cases {
		limits := types.GovernanceLimits{MaxCPUMillis: c.maxCPU, MaxMemoryMiB: c.maxMem}
		e := mustEffective(t, bg, GovernanceSource(limits, c.ceiling, untouched), deployment)
		if e.CPUMillis.Cap.Value != c.wantCPU || e.MemoryMiB.Cap.Value != c.wantMem ||
			e.CPUMillis.CapSource != types.LimitSourceGovernance || e.MemoryMiB.CapSource != types.LimitSourceGovernance {
			t.Errorf("%s: cap cpu %+v memory %+v, want %d and %d from governance", c.name, e.CPUMillis, e.MemoryMiB, c.wantCPU, c.wantMem)
		}
	}
}

// TestGovernanceLifetimeAgreesWithEndChange: the lifetime Effective reads from
// governance is the one an end change enforces (planEnd in the run-end route). A
// finite maximum bounds a finite end whether or not No end is on offer, and No end
// is on offer only with AllowNoEnd and the change-limits gate together. With no
// maximum, a profile that does not offer No end still bounds a finite end, at the
// run-limit ceiling; it is never read as unlimited.
func TestGovernanceLifetimeAgreesWithEndChange(t *testing.T) {
	ceiling := types.RunnerPoolSecondsMax
	for _, c := range []struct {
		name        string
		max         int
		allow, gate bool
		wantMax     int
		wantNoEnd   bool
	}{
		{"no maximum, no offer", 0, false, false, ceiling, false},
		{"no maximum, allowed without the gate", 0, true, false, ceiling, false},
		{"no maximum, gate without the allowance", 0, false, true, ceiling, false},
		{"no maximum, offered", 0, true, true, 0, true},
		{"a maximum, no offer", 604800, false, false, 604800, false},
		{"a maximum survives the offer", 604800, true, true, 604800, true},
		{"a maximum survives an allowance without the gate", 604800, true, false, 604800, false},
	} {
		g := GovernanceSource(types.GovernanceLimits{RunLimits: types.RunLimits{MaxEndAheadSec: c.max, AllowNoEnd: c.allow, UserChangesLimits: c.gate}}, types.ResourceLimits{}, types.ResourceLimits{})
		if (c.wantMax == 0) != (g.LifetimeSec.Max == nil) || (g.LifetimeSec.Max != nil && *g.LifetimeSec.Max != c.wantMax) {
			t.Errorf("%s: lifetime maximum = %v, want %d", c.name, g.LifetimeSec.Max, c.wantMax)
		}
		e := mustEffective(t, bg, SourceLimits{Source: types.LimitSourcePool}, g)
		if e.NoEndAllowed != c.wantNoEnd {
			t.Errorf("%s: No end allowed = %v, want %v", c.name, e.NoEndAllowed, c.wantNoEnd)
		}
		if (c.wantMax == 0) != e.Lifetime.Cap.Unlimited || (c.wantMax != 0 && e.Lifetime.Cap.Value != c.wantMax) {
			t.Errorf("%s: finite-end cap = %+v, want %d", c.name, e.Lifetime.Cap, c.wantMax)
		}
	}
}

// TestEffectiveNoEndNeedsEverySource: one source that bounds the life, or does
// not allow No end, takes it away; none that do leaves it.
func TestEffectiveNoEndNeedsEverySource(t *testing.T) {
	yes, no := true, false
	if e := mustEffective(t, bg); !e.NoEndAllowed {
		t.Fatal("no source objects, so No end is allowed")
	}
	if e := mustEffective(t, bg, SourceLimits{Source: types.LimitSourceDeployment, LifetimeSec: Bound{Max: Int(604800)}}); e.NoEndAllowed {
		t.Fatal("the deployment's maximum age ends every run")
	}
	if e := mustEffective(t, bg, SourceLimits{Source: types.LimitSourcePool, LifetimeSec: Bound{Max: Int(3600)}, NoEnd: &yes}); !e.NoEndAllowed {
		t.Fatal("a source that says it allows No end is believed")
	}
	if e := mustEffective(t, bg, SourceLimits{Source: types.LimitSourceGovernance, NoEnd: &no}); e.NoEndAllowed {
		t.Fatal("a source that does not allow No end takes it away")
	}
}

// TestEffectiveLifetimeSpanAndLeaseAreSeparate: the pool's and deployment's
// lifetime counts from the run's start and governance's from now, so an extension
// needs both bounds; the folded Lifetime is only the lesser of the two.
func TestEffectiveLifetimeSpanAndLeaseAreSeparate(t *testing.T) {
	pool := SourceLimits{Source: types.LimitSourcePool, LifetimeSec: Bound{Max: Int(2592000)}}
	dep := SourceLimits{Source: types.LimitSourceDeployment, LifetimeSec: Bound{Max: Int(7776000)}}
	gov := SourceLimits{Source: types.LimitSourceGovernance, LifetimeSec: Bound{Max: Int(604800)}}
	e := mustEffective(t, bg, gov, dep, pool)
	if e.LifetimeSpan.Cap.Value != 2592000 || e.LifetimeSpan.CapSource != types.LimitSourcePool {
		t.Errorf("span = %+v", e.LifetimeSpan)
	}
	if e.LifetimeLease.Cap.Value != 604800 || e.LifetimeLease.CapSource != types.LimitSourceGovernance {
		t.Errorf("lease = %+v", e.LifetimeLease)
	}
	if e.Lifetime.Cap.Value != 604800 || e.Lifetime.CapSource != types.LimitSourceGovernance {
		t.Errorf("folded = %+v", e.Lifetime)
	}
	// a bound that is absent on one side is unlimited on that side only
	e = mustEffective(t, bg, gov)
	if !e.LifetimeSpan.Cap.Unlimited || e.LifetimeLease.Cap.Unlimited {
		t.Errorf("span %+v lease %+v", e.LifetimeSpan, e.LifetimeLease)
	}
	e = mustEffective(t, bg, pool)
	if e.LifetimeSpan.Cap.Unlimited || !e.LifetimeLease.Cap.Unlimited {
		t.Errorf("span %+v lease %+v", e.LifetimeSpan, e.LifetimeLease)
	}
}
