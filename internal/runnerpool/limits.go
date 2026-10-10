// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Bound is one source's limit on one field. A nil Max is "this source sets no
// maximum"; a nil Default is "this source suggests no default". A bound that is
// present is positive: a source that means "nothing" leaves it nil, so a zero
// can never be read as a limit of nothing or as no limit.
type Bound struct{ Max, Default *int }

// SourceLimits is what one source says about one run. Every source is folded by
// Effective the same way: the lowest maximum wins, per field.
type SourceLimits struct {
	Source types.LimitSource
	// CPUMillis, MemoryMiB, LifetimeSec and IdleSec are the fields. IdleSec is read
	// for an Interactive run only.
	CPUMillis, MemoryMiB, LifetimeSec, IdleSec Bound
	// ConcurrentRuns caps running runs; see types.ConcurrencyBound for what it counts.
	ConcurrentRuns *int
	// NoEnd says whether the source lets a run have no end. Nil means it does
	// exactly when it sets no lifetime maximum; governance sets it, because a
	// profile can offer No end and also bound a finite end.
	NoEnd *bool
}

// Int is a pointer to n, for building a Bound.
func Int(n int) *int { return &n }

// sourceRank orders sources for a tie: the most specific source is named as the
// one that bound a value two sources both bound, whatever order they came in.
var sourceRank = map[types.LimitSource]int{
	types.LimitSourcePool:           0,
	types.LimitSourceGovernance:     1,
	types.LimitSourceRunnerCapacity: 2,
	types.LimitSourceDeployment:     3,
}

// Effective is the one function that decides a run's CPU, memory, lifetime and
// idle stop: for each field, the strictest of every source it is given. The
// caller passes the sources that apply to this run:
//
//   - the pool: PoolSource (a pool with no limits passes an empty one, or none);
//   - the person's governance ceiling: GovernanceSource (an operator has none);
//   - the deployment: its default sandbox size as Defaults, the request
//     contract's bound and WARDYN_RUN_MAX_AGE as maxima, and its concurrency cap;
//   - self-hosted pools only: RunnerCapacitySource over the run's candidates.
//
// A value is unlimited only when no source bounds it. An untouched run gets the
// lowest default any source names, never above the cap; with none it gets the
// cap, which is today's rule for a run's end. Each answer names the source that
// bound it, for the preview and for explaining a reduction. A source with a
// non-positive bound, an unknown source or an unknown run type is an error:
// the caller refuses, it never guesses.
func Effective(runType types.RunnerPoolRunType, sources ...SourceLimits) (types.EffectiveRunLimits, error) {
	if !runType.Valid() {
		return types.EffectiveRunLimits{}, fmt.Errorf("run type %q is not background or interactive", runType)
	}
	ordered := slices.Clone(sources)
	for _, s := range ordered {
		if _, ok := sourceRank[s.Source]; !ok {
			return types.EffectiveRunLimits{}, fmt.Errorf("limit source %q is unknown", s.Source)
		}
	}
	slices.SortStableFunc(ordered, func(a, b SourceLimits) int { return cmp.Compare(sourceRank[a.Source], sourceRank[b.Source]) })

	out := types.EffectiveRunLimits{RunType: runType, Concurrency: []types.ConcurrencyBound{}}
	var err error
	fold := func(dst *types.EffectiveLimit, pick func(SourceLimits) Bound) {
		var ferr error
		*dst, ferr = foldField(ordered, pick)
		err = errors.Join(err, ferr)
	}
	fold(&out.CPUMillis, func(s SourceLimits) Bound { return s.CPUMillis })
	fold(&out.MemoryMiB, func(s SourceLimits) Bound { return s.MemoryMiB })
	fold(&out.Lifetime, func(s SourceLimits) Bound { return s.LifetimeSec })
	fold(&out.LifetimeSpan, func(s SourceLimits) Bound { return lifetimeOf(s, false) })
	fold(&out.LifetimeLease, func(s SourceLimits) Bound { return lifetimeOf(s, true) })
	out.NoEndAllowed = true
	for _, s := range ordered {
		out.NoEndAllowed = out.NoEndAllowed && allowsNoEnd(s)
	}
	if runType == types.RunnerPoolRunInteractive {
		out.Idle = new(types.EffectiveLimit)
		fold(out.Idle, func(s SourceLimits) Bound { return s.IdleSec })
	}
	for _, s := range ordered {
		if s.ConcurrentRuns == nil {
			continue
		}
		if *s.ConcurrentRuns < 1 {
			err = errors.Join(err, fmt.Errorf("%s: a concurrency cap is 1 or more", s.Source))
			continue
		}
		out.Concurrency = append(out.Concurrency, types.ConcurrencyBound{Source: s.Source, Max: *s.ConcurrentRuns})
	}
	if err != nil {
		return types.EffectiveRunLimits{}, err
	}
	return out, nil
}

// lifetimeOf is the source's lifetime bound when it is of the asked kind: governance's is a
// lease, counted from the moment an end is asked for; every other source's is a
// span, counted from the run's start.
func lifetimeOf(s SourceLimits, lease bool) Bound {
	if (s.Source == types.LimitSourceGovernance) != lease {
		return Bound{}
	}
	return s.LifetimeSec
}

// allowsNoEnd: whether the source lets a run have no end.
func allowsNoEnd(s SourceLimits) bool {
	if s.NoEnd != nil {
		return *s.NoEnd
	}
	return s.LifetimeSec.Max == nil
}

// foldField folds one field over sources already in rank order. The comparisons
// are strict, so a tie keeps the earlier, more specific source.
func foldField(ordered []SourceLimits, pick func(SourceLimits) Bound) (types.EffectiveLimit, error) {
	var out types.EffectiveLimit
	var capV, defV *int
	var defSource types.LimitSource
	for _, s := range ordered {
		b := pick(s)
		if (b.Max != nil && *b.Max < 1) || (b.Default != nil && *b.Default < 1) {
			return types.EffectiveLimit{}, fmt.Errorf("%s: a limit is 1 or more, or unset", s.Source)
		}
		if b.Max != nil && (capV == nil || *b.Max < *capV) {
			capV, out.CapSource = b.Max, s.Source
		}
		if b.Default != nil && (defV == nil || *b.Default < *defV) {
			defV, defSource = b.Default, s.Source
		}
	}
	switch {
	case capV == nil:
		out.Cap = types.LimitAmount{Unlimited: true}
	default:
		out.Cap = types.LimitAmount{Value: *capV}
	}
	switch {
	case defV != nil && (capV == nil || *defV <= *capV):
		out.Default, out.DefaultSource = types.LimitAmount{Value: *defV}, defSource
	case capV != nil:
		out.Default, out.DefaultSource = types.LimitAmount{Value: *capV}, out.CapSource
	default:
		out.Default = types.LimitAmount{Unlimited: true}
	}
	return out, nil
}

// PoolSource is the pool's side of Effective for one run type. A pool with no
// limits adds no bound. A run type the pool does not serve is a refusal, so a
// pool never answers limits for a run it would not take.
func PoolSource(p types.RunnerPool, runType types.RunnerPoolRunType) (SourceLimits, *Refusal) {
	out := SourceLimits{Source: types.LimitSourcePool}
	l := p.Limits
	if l == nil {
		return out, nil
	}
	if !l.AllowsRunType(runType) {
		return out, &Refusal{Reason: ReasonRunTypeNotAllowed, Name: p.Name, RunType: runType}
	}
	amount := func(a types.RunnerPoolAmount) Bound { return Bound{Max: Int(a.Cap), Default: Int(a.Default)} }
	duration := func(d types.RunnerPoolDuration) Bound {
		var b Bound
		if !d.Unlimited {
			b.Max = Int(d.MaxSec)
		}
		if d.DefaultSec > 0 {
			b.Default = Int(d.DefaultSec)
		}
		return b
	}
	switch runType {
	case types.RunnerPoolRunBackground:
		b := l.Background
		out.CPUMillis, out.MemoryMiB, out.LifetimeSec = amount(b.CPUMillis), amount(b.MemoryMiB), duration(b.Lifetime)
	case types.RunnerPoolRunInteractive:
		i := l.Interactive
		out.CPUMillis, out.MemoryMiB, out.LifetimeSec = amount(i.CPUMillis), amount(i.MemoryMiB), duration(i.Lifetime)
		out.IdleSec = duration(i.Idle)
	}
	out.ConcurrentRuns = l.MaxConcurrentRuns
	return out, nil
}

// CheckBarrier refuses a barrier the pool does not allow. A pool with no limits
// allows every barrier.
func CheckBarrier(p types.RunnerPool, barrier types.ConfinementClass) *Refusal {
	if p.Limits == nil || p.Limits.AllowsBarrier(barrier) {
		return nil
	}
	return &Refusal{Reason: ReasonBarrierNotAllowed, Name: p.Name, Barrier: barrier, Allowed: p.Limits.Barriers}
}

// CheckCapacity refuses a launch into a pool already running its cap. running is
// the pool's non-terminal runs counted in the same transaction that inserts the
// new run, under the pool's row lock; a count read outside it lets two launches
// both pass.
func CheckCapacity(p types.RunnerPool, running int) *Refusal {
	if p.Limits == nil || p.Limits.MaxConcurrentRuns == nil || running < *p.Limits.MaxConcurrentRuns {
		return nil
	}
	return &Refusal{Reason: ReasonAtCapacity, Name: p.Name, Max: *p.Limits.MaxConcurrentRuns}
}

// GovernanceSource is the person's governance ceiling's side of Effective:
// CPU, memory, how long a run may live and how many a person may run at once.
//
// size is the CPU and memory a governed request may reach, as the caller
// computes it for the member cap: max(what an untouched run already gets, the
// ceiling's size), where the ceiling's size is composer.CeilingResources (the
// ceiling policy's own size, an unset field at the deployment's default, each
// lowered, never raised, to the profile's maximum). The profile's MaxCPUMillis
// and MaxMemoryMiB are folded into it and are not read again here, so a maximum
// above the deployment's default does not lift the cap above it.
// A zero field is unrestricted and leaves the bound nil; so does a zero limit.
//
// The lifetime is a lease: a finite end may be at most MaxEndAheadSec ahead of
// now, whether or not No end is also on offer (AllowNoEnd with
// UserChangesLimits), and NoEnd says whether it is. With no maximum, a profile
// that does not offer No end still bounds a finite end, at the run-limit ceiling,
// exactly as an end change does; only one that offers it leaves the lease
// unbounded. The default is the profile's default end, else its maximum, as a
// new run's end is today. Governance has no idle-stop ceiling (PauseIdleAfterSec
// pauses, it does not stop), so IdleSec is never set.
func GovernanceSource(l types.GovernanceLimits, size types.ResourceLimits) SourceLimits {
	out := SourceLimits{Source: types.LimitSourceGovernance}
	positive := func(n int) *int {
		if n > 0 {
			return Int(n)
		}
		return nil
	}
	out.CPUMillis.Max, out.MemoryMiB.Max = positive(size.CPUMillis), positive(size.MemoryMiB)
	noEnd := l.AllowNoEnd && l.UserChangesLimits
	out.NoEnd = &noEnd
	switch {
	case l.MaxEndAheadSec > 0:
		out.LifetimeSec.Max = Int(l.MaxEndAheadSec)
	case !noEnd:
		// No maximum is set but No end is not on offer: a finite end is
		// bounded only by the run-limit ceiling, as planEnd bounds it.
		out.LifetimeSec.Max = Int(types.RunnerPoolSecondsMax)
	}
	out.LifetimeSec.Default = positive(cmp.Or(l.DefaultEndSec, l.MaxEndAheadSec))
	out.ConcurrentRuns = positive(l.MaxConcurrentRuns)
	return out
}

// RunnerCapacity is what one runner advertises for a single run.
type RunnerCapacity struct{ CPUMillis, MemoryMiB int64 }

// RunnerCapacitySource is a self-hosted run's runner-capacity side of Effective:
// the weakest of its candidates, because the run may land on any of them and a
// pool must not offer more than its smallest eligible runner can give. No
// candidate, or one that reports no capacity, is an error: a missing fact refuses
// the run, it is never read as zero or as unlimited.
func RunnerCapacitySource(candidates ...RunnerCapacity) (SourceLimits, error) {
	out := SourceLimits{Source: types.LimitSourceRunnerCapacity}
	if len(candidates) == 0 {
		return out, errors.New("no candidate runner to take a capacity from")
	}
	cpu, mem := int64(math.MaxInt32), int64(math.MaxInt32)
	for _, c := range candidates {
		if c.CPUMillis < 1 || c.MemoryMiB < 1 {
			return out, errors.New("a candidate runner reports no capacity")
		}
		cpu, mem = min(cpu, c.CPUMillis), min(mem, c.MemoryMiB)
	}
	out.CPUMillis.Max, out.MemoryMiB.Max = Int(int(cpu)), Int(int(mem))
	return out, nil
}
