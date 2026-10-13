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
	// CPUMillis, MemoryMiB, DiskMiB, LifetimeSec and IdleSec are the fields. IdleSec
	// is read for an Interactive run only.
	CPUMillis, MemoryMiB, DiskMiB, LifetimeSec, IdleSec Bound
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
	types.LimitSourcePool:       0,
	types.LimitSourceGovernance: 1,
	types.LimitSourceRunner:     2,
	types.LimitSourceDeployment: 3,
}

// Effective is the one function that decides a run's CPU, memory, main-drive
// size, lifetime and idle stop: for each field, the strictest of every source it is given. The
// caller passes the sources that apply to this run:
//
//   - the pool: PoolSource (a pool with no limits passes an empty one, or none);
//   - the person's governance ceiling: GovernanceSource (an operator has none);
//   - the deployment: DeploymentSource, ALWAYS, exactly once (its default sandbox
//     size, the request contract's bounds, WARDYN_RUN_MAX_AGE, its concurrency
//     cap);
//   - self-hosted pools only: RunnerCapacitySource over the run's candidates.
//
// A value is unlimited only when no source bounds it, and the deployment source
// is always there to bound it: Effective refuses to compute without exactly one,
// and without more than one pool source, because a dropped source would silently
// widen every field. An untouched run gets the
// lowest default any source names, never above the cap. The deployment's
// default is its fallback sandbox size: it counts only when no other source
// names a default, so it never lowers a pool's own. With no default at all a
// run gets the cap, which is today's rule for a run's end. Each answer names the source that
// bound it, for the preview and for explaining a reduction. A source with a
// non-positive bound, an unknown source or an unknown run type is an error:
// the caller refuses, it never guesses.
func Effective(runType types.RunnerPoolRunType, sources ...SourceLimits) (types.EffectiveRunLimits, error) {
	if !runType.Valid() {
		return types.EffectiveRunLimits{}, fmt.Errorf("run type %q is not background or interactive", runType)
	}
	ordered := slices.Clone(sources)
	count := map[types.LimitSource]int{}
	for _, s := range ordered {
		if _, ok := sourceRank[s.Source]; !ok {
			return types.EffectiveRunLimits{}, fmt.Errorf("limit source %q is unknown", s.Source)
		}
		count[s.Source]++
	}
	if count[types.LimitSourceDeployment] != 1 {
		return types.EffectiveRunLimits{}, fmt.Errorf("exactly one deployment source is needed, got %d", count[types.LimitSourceDeployment])
	}
	for src, n := range count {
		if n > 1 {
			return types.EffectiveRunLimits{}, fmt.Errorf("limit source %q is given %d times", src, n)
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
	fold(&out.DiskMiB, func(s SourceLimits) Bound { return s.DiskMiB })
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
		// The deployment sorts last, so its fallback default is read only
		// when nothing more specific named one.
		if b.Default != nil && (defV == nil || (s.Source != types.LimitSourceDeployment && *b.Default < *defV)) {
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

// DeploymentSource is the deployment's side of Effective, the one every call
// carries: defaults is its default sandbox size (sizing.EffectiveLimits), CPU and
// memory are bounded by the request contract's own bounds, eph is the
// organisation's ephemeral-disk default and maximum (0 = unset), maxAgeSec is
// WARDYN_RUN_MAX_AGE (0 = none) and concurrentRuns the deployment's cap on
// running runs (0 = none).
func DeploymentSource(defaults types.ResourceLimits, eph types.EphemeralProvider, maxAgeSec, concurrentRuns int) (SourceLimits, error) {
	if defaults.CPUMillis < 1 || defaults.MemoryMiB < 1 || eph.DefaultDiskMiB < 0 || eph.MaxDiskMiB < 0 || maxAgeSec < 0 || concurrentRuns < 0 {
		return SourceLimits{}, fmt.Errorf("the deployment's default size is 1 or more and its disk, age and run caps 0 or more, got %+v, %+v, %d, %d", defaults, eph, maxAgeSec, concurrentRuns)
	}
	out := SourceLimits{Source: types.LimitSourceDeployment,
		CPUMillis: Bound{Max: Int(types.RunnerPoolCPUMillisMax), Default: Int(defaults.CPUMillis)},
		MemoryMiB: Bound{Max: Int(types.RunnerPoolMemoryMiBMax), Default: Int(defaults.MemoryMiB)},
		DiskMiB:   Bound{Max: positive(eph.MaxDiskMiB), Default: positive(eph.DefaultDiskMiB)},
	}
	if maxAgeSec > 0 {
		out.LifetimeSec.Max = Int(maxAgeSec)
	}
	if concurrentRuns > 0 {
		out.ConcurrentRuns = Int(concurrentRuns)
	}
	return out, nil
}

// positive is a pointer to n, or nil when n is not positive: a source that means
// "nothing" leaves a Bound nil.
func positive(n int) *int {
	if n > 0 {
		return Int(n)
	}
	return nil
}

// InteractiveDefaultLifetimeSec is the end an untouched Interactive run gets when
// no source (pool, governance, deployment) names a lifetime default: 24 hours
// (owner ruling, #1319). An Interactive run never defaults to No end; that is an
// explicit choice. Idle stop still ends an abandoned run sooner.
const InteractiveDefaultLifetimeSec = 24 * 60 * 60

// WithInteractiveLifetimeDefault adds the Interactive fallback to the deployment
// source for runType. It rides the deployment's default slot, which Effective
// reads only when nothing more specific names a default and never above a cap,
// so a pool's or a profile's own default and every maximum win over it, and it
// changes no source's No end permission. Any other run type is returned as given.
func WithInteractiveLifetimeDefault(deployment SourceLimits, runType types.RunnerPoolRunType) SourceLimits {
	if runType == types.RunnerPoolRunInteractive && deployment.LifetimeSec.Default == nil {
		deployment.LifetimeSec.Default = Int(InteractiveDefaultLifetimeSec)
	}
	return deployment
}

// PoolSource is the pool's side of Effective for one run type. A pool with no
// limits adds no bound. A run type the pool does not serve is a refusal, so a
// pool never answers limits for a run it would not take.
//
// runType is the run's STORED experience (background or interactive), never one
// inferred from the request: an empty or unknown value (the older-client shape)
// never satisfies a pool's run-type list and is refused.
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
		if b.DiskMiB != nil {
			out.DiskMiB = amount(*b.DiskMiB)
		}
	case types.RunnerPoolRunInteractive:
		i := l.Interactive
		out.CPUMillis, out.MemoryMiB, out.LifetimeSec = amount(i.CPUMillis), amount(i.MemoryMiB), duration(i.Lifetime)
		if i.DiskMiB != nil {
			out.DiskMiB = amount(*i.DiskMiB)
		}
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

// AtCapacity reports that the pool is running its cap, so a new run waits for a
// slot: it returns a Wait, not a Refusal, so it cannot be answered as a refusal.
// nil means a slot is free. running counts the pool's runs that HOLD a slot
// (dispatched or running), never queued ones; a queue of max waiting runs must
// not read the pool as full for ever. It is counted in the same transaction that
// inserts the new run, under the pool's row lock; a count read outside it lets
// two launches both pass. The queue itself is not here.
func AtCapacity(p types.RunnerPool, running int) *Wait {
	if p.Limits == nil || p.Limits.MaxConcurrentRuns == nil || running < *p.Limits.MaxConcurrentRuns {
		return nil
	}
	return &Wait{Pool: p.Name, Max: *p.Limits.MaxConcurrentRuns}
}

// GovernanceSource is the person's governance ceiling's side of Effective:
// CPU, memory, the main drive's size, how long a run may live and how many a
// person may run at once.
//
// CPU and memory are the member cap (#1949): the most a governed request may
// ask for is max(untouched, size), where untouched is what the run already gets
// (the admin-authored resolved policy's size after the policy step, never
// derived from the request) and size is the ceiling policy's own size. A
// profile's maximum counts as the ceiling's size: with no ceiling size set, the
// maximum IS the size (a maximum above the deployment's default raises the cap
// to it); with both set, the lower of the two. A governed member is never left
// unbounded: a field where untouched is 0 (unset) starts from deploymentDefault,
// per field, so a ceiling that sets only memory still bounds CPU at the default.
// A deploymentDefault with a field below 1 is an error.
//
// l must be the limits the run is judged under: for an extension, the limits the
// run captured at create, not the live profile.
//
// The lifetime is a lease: a finite end may be at most MaxEndAheadSec ahead of
// now, whether or not No end is also on offer (AllowNoEnd with
// UserChangesLimits), and NoEnd says whether it is. With no maximum, a profile
// that does not offer No end still bounds a finite end, at the run-limit ceiling,
// exactly as an end change does; only one that offers it leaves the lease
// unbounded. The default is the profile's default end, else its maximum, as a
// new run's end is today. Governance has no idle-stop ceiling (PauseIdleAfterSec
// pauses, it does not stop), so IdleSec is never set.
func GovernanceSource(l types.GovernanceLimits, ceiling, untouched, deploymentDefault types.ResourceLimits) (SourceLimits, error) {
	out := SourceLimits{Source: types.LimitSourceGovernance}
	if deploymentDefault.CPUMillis < 1 || deploymentDefault.MemoryMiB < 1 {
		return out, errors.New("governance needs the deployment's default size to bound a member")
	}
	out.CPUMillis.Max = Int(MemberCap(ceiling.CPUMillis, l.MaxCPUMillis, cmp.Or(untouched.CPUMillis, deploymentDefault.CPUMillis)))
	out.MemoryMiB.Max = Int(MemberCap(ceiling.MemoryMiB, l.MaxMemoryMiB, cmp.Or(untouched.MemoryMiB, deploymentDefault.MemoryMiB)))
	// The main drive: the lower of the profile's and the ceiling's size is a maximum
	// only. Governance sizes nothing, so it never names a default.
	out.DiskMiB.Max = lowerPositive(l.MaxEphemeralDiskMiB, ceiling.DiskMiB)
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
	return out, nil
}

// lowerPositive is the lower of the positive values of a and b, nil when neither is.
func lowerPositive(a, b int) *int {
	switch {
	case a > 0 && b > 0:
		return Int(min(a, b))
	case a > 0:
		return Int(a)
	}
	return positive(b)
}

// MemberCap is one field of the member cap (#1949), the one rule every bound on a
// governed member's CPU and memory uses: max(untouched, size), where the profile's
// maximum is the size when the ceiling sets none and lowers it when it does. A
// caller with no baseline passes the deployment's default as untouched, so the
// cap is at least 1 whenever untouched is.
func MemberCap(ceiling, profileMax, untouched int) int {
	size := ceiling
	switch {
	case size < 1:
		size = profileMax
	case profileMax > 0:
		size = min(size, profileMax)
	}
	return max(size, untouched)
}

// RunnerCapacity is what one runner advertises for a single run.
type RunnerCapacity struct{ CPUMillis, MemoryMiB int64 }

// RunnerCapacitySource is a self-hosted run's runner-capacity side of Effective:
// the weakest of its candidates, because the run may land on any of them and a
// pool must not offer more than its smallest eligible runner can give. No
// candidate, or one that reports no capacity, is an error: a missing fact refuses
// the run, it is never read as zero or as unlimited.
func RunnerCapacitySource(candidates ...RunnerCapacity) (SourceLimits, error) {
	out := SourceLimits{Source: types.LimitSourceRunner}
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
