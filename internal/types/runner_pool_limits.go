// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
	"math"
	"slices"
)

// Pool limits (0.9). A pool carries its own limits, per run type: how large a
// run may be, how long it may live and, for an Interactive run, how long it may
// sit unused. They never widen anything. The effective limit of every field is
// the strictest of the pool, the person's governance ceiling, the deployment
// and (self-hosted) the runner's capacity; internal/runnerpool computes it.

// RunnerPoolRunType is the kind of run a pool serves. The values are the run
// mode's wire values (a Background task, an Interactive environment).
type RunnerPoolRunType string

const (
	RunnerPoolRunBackground  RunnerPoolRunType = "background"
	RunnerPoolRunInteractive RunnerPoolRunType = "interactive"
)

// Valid reports whether t is one of the two run types.
func (t RunnerPoolRunType) Valid() bool {
	return t == RunnerPoolRunBackground || t == RunnerPoolRunInteractive
}

// Plural is how a sentence names runs of the type.
func (t RunnerPoolRunType) Plural() string {
	switch t {
	case RunnerPoolRunBackground:
		return "background tasks"
	case RunnerPoolRunInteractive:
		return "interactive environments"
	}
	return ""
}

// The bounds every pool amount and duration is validated against. They are the
// run request's own bounds (CPU and memory) and the run limits' (seconds), so a
// pool can never name a value the request contract refuses.
const (
	RunnerPoolCPUMillisMax = 1_000_000     // milli-CPU: a thousand CPUs
	RunnerPoolMemoryMiBMax = 16 << 20      // MiB: 16 TiB
	RunnerPoolDiskMiBMax   = 16 << 20      // MiB: 16 TiB, the request contract's disk bound
	RunnerPoolSecondsMax   = math.MaxInt32 // about 68 years
)

// RunnerPoolAmount is a CPU or memory limit: what an untouched run gets and the
// most a request may ask for. Both are positive and Default is at most Cap.
type RunnerPoolAmount struct {
	Default int `json:"default"`
	Cap     int `json:"cap"`
}

func (a RunnerPoolAmount) validate(field, unit string, bound int) error {
	switch {
	case a.Default < 1 || a.Cap < 1:
		return fmt.Errorf("%s: set a default and a cap of at least 1 %s", field, unit)
	case a.Cap > bound:
		return fmt.Errorf("%s: the cap is at most %d %s", field, bound, unit)
	case a.Default > a.Cap:
		return fmt.Errorf("%s: the default (%d) is past the cap (%d)", field, a.Default, a.Cap)
	}
	return nil
}

// validateDisk is the main-drive size's check: an amount like memory's, and a
// whole number of GiB, because the console sizes a drive in GiB. A nil size is a
// pool that sets no main-drive bound.
func validateDisk(runType string, a *RunnerPoolAmount) error {
	if a == nil {
		return nil
	}
	field := runType + ".disk_mib"
	if err := a.validate(field, "MiB", RunnerPoolDiskMiBMax); err != nil {
		return err
	}
	if a.Default%1024 != 0 || a.Cap%1024 != 0 {
		return fmt.Errorf("%s: set a default and a cap in whole GiB (multiples of 1024 MiB)", field)
	}
	return nil
}

// RunnerPoolDuration is a lifetime or an idle-stop limit, in seconds. It has
// exactly one of two forms: a finite MaxSec, with DefaultSec from 1 to MaxSec,
// or Unlimited, with MaxSec 0 and a DefaultSec of 0 (no end, or never stop
// for idleness, by default) up to RunnerPoolSecondsMax. Unlimited is never
// encoded by a zero MaxSec alone, so an omitted number cannot read as it. For an
// idle stop, Unlimited is "never". An Interactive lifetime that is Unlimited
// still needs a DefaultSec of at least 1; a Background lifetime may default to
// no end.
type RunnerPoolDuration struct {
	DefaultSec int  `json:"default_sec"`
	MaxSec     int  `json:"max_sec,omitempty"`
	Unlimited  bool `json:"unlimited,omitempty"`
}

func (d RunnerPoolDuration) validate(field string) error {
	switch {
	case d.Unlimited && d.MaxSec != 0:
		return fmt.Errorf("%s: set max_sec or unlimited, not both", field)
	case !d.Unlimited && d.MaxSec < 1:
		return fmt.Errorf("%s: set max_sec to a number of seconds, or unlimited", field)
	case d.MaxSec > RunnerPoolSecondsMax || d.DefaultSec > RunnerPoolSecondsMax:
		return fmt.Errorf("%s: seconds are at most %d", field, RunnerPoolSecondsMax)
	case d.DefaultSec < 0:
		return fmt.Errorf("%s: default_sec is 0 or more seconds", field)
	case !d.Unlimited && d.DefaultSec < 1:
		return fmt.Errorf("%s: a finite limit needs a default of at least 1 second", field)
	case !d.Unlimited && d.DefaultSec > d.MaxSec:
		return fmt.Errorf("%s: the default (%d) is past the maximum (%d)", field, d.DefaultSec, d.MaxSec)
	}
	return nil
}

// RunnerPoolBackgroundLimits are a pool's limits for Background tasks. It has no
// idle stop: a Background task ends when its command or agent exits, or at its
// maximum lifetime. Decoding a body that sends `idle` here fails.
//
// Every field is optional: one left out is set by the pool's runners (their per-run limits, and
// governance and the deployment for time), never "no limit".
type RunnerPoolBackgroundLimits struct {
	CPUMillis *RunnerPoolAmount `json:"cpu_millis,omitempty"`
	MemoryMiB *RunnerPoolAmount `json:"memory_mib,omitempty"`
	// DiskMiB bounds the main drive of this run type's runs: what an untouched run
	// gets and the most a request may ask for, in MiB, each a whole number of GiB.
	// Nil: the pool sets no main-drive bound.
	DiskMiB  *RunnerPoolAmount   `json:"disk_mib,omitempty"`
	Lifetime *RunnerPoolDuration `json:"lifetime,omitempty"`
}

// RunnerPoolInteractiveLimits are a pool's limits for Interactive environments,
// which alone have an idle stop. Every field is optional, as on RunnerPoolBackgroundLimits.
type RunnerPoolInteractiveLimits struct {
	CPUMillis *RunnerPoolAmount `json:"cpu_millis,omitempty"`
	MemoryMiB *RunnerPoolAmount `json:"memory_mib,omitempty"`
	// DiskMiB is the main drive's bound, as on RunnerPoolBackgroundLimits.
	DiskMiB  *RunnerPoolAmount   `json:"disk_mib,omitempty"`
	Lifetime *RunnerPoolDuration `json:"lifetime,omitempty"`
	Idle     *RunnerPoolDuration `json:"idle,omitempty"`
}

// RunnerPoolLimits is everything a pool limits. A run type with limits is a run
// type the pool allows, and one without is refused, so "allowed" and "limited"
// cannot disagree. A pool with no RunnerPoolLimits at all adds no bound: only
// governance, the deployment and the runner bind its runs.
type RunnerPoolLimits struct {
	Background  *RunnerPoolBackgroundLimits  `json:"background,omitempty"`
	Interactive *RunnerPoolInteractiveLimits `json:"interactive,omitempty"`
	// Barriers are the confinement classes (Fence CC1, Wall CC2, Vault CC3) the
	// pool allows. A run on any other is refused. None listed: every barrier its
	// runners offer.
	Barriers []ConfinementClass `json:"barriers"`
	// MaxConcurrentRuns caps the pool's non-terminal runs, across everyone. Unset
	// is no pool cap; a person's own cap is the governance profile's.
	MaxConcurrentRuns *int `json:"max_concurrent_runs,omitempty"`
}

// Validate refuses limits a pool cannot keep: no run type, no barrier, an
// unknown or repeated barrier, a default past its cap, a value out of range or a
// concurrency cap below 1.
func (l RunnerPoolLimits) Validate() error {
	if l.Background == nil && l.Interactive == nil {
		return errors.New("a pool allows at least one run type: set background, interactive or both")
	}
	if b := l.Background; b != nil {
		if err := errors.Join(
			b.CPUMillis.validateSet("background.cpu_millis", "milli-CPU", RunnerPoolCPUMillisMax),
			b.MemoryMiB.validateSet("background.memory_mib", "MiB", RunnerPoolMemoryMiBMax),
			validateDisk("background", b.DiskMiB),
			b.Lifetime.validateSet("background.lifetime"),
		); err != nil {
			return err
		}
	}
	if i := l.Interactive; i != nil {
		if err := errors.Join(
			i.CPUMillis.validateSet("interactive.cpu_millis", "milli-CPU", RunnerPoolCPUMillisMax),
			i.MemoryMiB.validateSet("interactive.memory_mib", "MiB", RunnerPoolMemoryMiBMax),
			validateDisk("interactive", i.DiskMiB),
			i.Lifetime.validateSet("interactive.lifetime"),
			i.Idle.validateSet("interactive.idle"),
		); err != nil {
			return err
		}
		// An Interactive environment is never left to run with no end by default:
		// No end is chosen per run, so an unlimited lifetime still names a finite
		// default. A Background task may default to No end.
		if i.Lifetime != nil && i.Lifetime.Unlimited && i.Lifetime.DefaultSec < 1 {
			return errors.New("interactive.lifetime: an unlimited lifetime still needs a finite default of at least 1 second; No end is chosen per run")
		}
	}
	for i, c := range l.Barriers {
		if c.Rank() == 0 {
			return fmt.Errorf("barriers: %q is not CC1, CC2 or CC3", c)
		}
		if slices.Contains(l.Barriers[:i], c) {
			return fmt.Errorf("barriers: %s is listed twice", c)
		}
	}
	if l.MaxConcurrentRuns != nil && (*l.MaxConcurrentRuns < 1 || *l.MaxConcurrentRuns > math.MaxInt32) {
		return errors.New("max_concurrent_runs: leave it unset for no cap, or set 1 or more")
	}
	return nil
}

// AllowsRunType reports whether the pool serves runs of type t.
func (l RunnerPoolLimits) AllowsRunType(t RunnerPoolRunType) bool {
	switch t {
	case RunnerPoolRunBackground:
		return l.Background != nil
	case RunnerPoolRunInteractive:
		return l.Interactive != nil
	}
	return false
}

// RunTypes lists the run types the pool allows, Background first.
func (l RunnerPoolLimits) RunTypes() []RunnerPoolRunType {
	var out []RunnerPoolRunType
	for _, t := range []RunnerPoolRunType{RunnerPoolRunBackground, RunnerPoolRunInteractive} {
		if l.AllowsRunType(t) {
			out = append(out, t)
		}
	}
	return out
}

// AllowsBarrier reports whether the pool allows runs on confinement class c.
func (l RunnerPoolLimits) AllowsBarrier(c ConfinementClass) bool {
	return len(l.Barriers) == 0 || slices.Contains(l.Barriers, c)
}

// validateSet is validate for an amount the pool sets; nil is set by its runners.
func (a *RunnerPoolAmount) validateSet(field, unit string, bound int) error {
	if a == nil {
		return nil
	}
	return a.validate(field, unit, bound)
}

// validateSet is validate for a duration the pool sets; nil is set elsewhere.
func (d *RunnerPoolDuration) validateSet(field string) error {
	if d == nil {
		return nil
	}
	return d.validate(field)
}

// BarrierLabel is the console's name for a confinement class.
func BarrierLabel(c ConfinementClass) string {
	switch c {
	case CC1:
		return "Fence"
	case CC2:
		return "Wall"
	case CC3:
		return "Vault"
	}
	return ""
}

// LimitSource names which bound an effective limit came from.
type LimitSource string

const (
	LimitSourcePool       LimitSource = "pool"
	LimitSourceGovernance LimitSource = "governance"
	LimitSourceDeployment LimitSource = "deployment"
	LimitSourceRunner     LimitSource = "runner"
)

// LimitAmount is an effective number. Unlimited is its own value, never a zero.
type LimitAmount struct {
	Value     int  `json:"value"`
	Unlimited bool `json:"unlimited,omitempty"`
}

// EffectiveLimit is one field's answer: what an untouched run gets, the most a
// request may ask for, and which source bound each. A source is absent when
// nothing bounded the value (an unlimited cap, or an unlimited default).
type EffectiveLimit struct {
	Default       LimitAmount `json:"default"`
	Cap           LimitAmount `json:"cap"`
	DefaultSource LimitSource `json:"default_source,omitempty"`
	CapSource     LimitSource `json:"cap_source,omitempty"`
}

// ConcurrencyBound is one source's cap on running runs. The caps are never
// folded into one number, because each counts a different set: the pool's counts
// the pool's runs across everyone, governance's one person's runs, the
// deployment's every run.
type ConcurrencyBound struct {
	Source LimitSource `json:"source"`
	Max    int         `json:"max"`
}

// EffectiveRunLimits is what a run of one type gets in one pool for one person,
// as the preview and the launch both read it. Idle is nil for a Background run:
// it has no idle stop.
//
// Lifetime caps a FINITE end and is the one number a create reads. It is folded
// from two bounds that an extension must read apart, because they are measured
// from different moments: LifetimeSpan (the pool, the deployment and the
// runner) counts from the run's start, and a run past it is KILLED; LifetimeLease
// (the person's governance) counts from the moment an end is asked for. A new
// run's end is bounded by the lesser of the two; an extension's by the lesser of
// start + LifetimeSpan.Cap and now + LifetimeLease.Cap. NoEndAllowed says
// whether the run may have no end at all, which is a separate yes: every source
// must allow it, and governance allows it only when its profile does, whatever
// finite maximum it also sets.
type EffectiveRunLimits struct {
	RunType   RunnerPoolRunType `json:"run_type"`
	CPUMillis EffectiveLimit    `json:"cpu_millis"`
	MemoryMiB EffectiveLimit    `json:"memory_mib"`
	// DiskMiB is the main drive's size in MiB. An unlimited Default means an untouched
	// run gets no size (its scratch is unbounded); with no default but a cap, an
	// untouched run gets the cap (foldField's rule).
	DiskMiB       EffectiveLimit     `json:"disk_mib"`
	Lifetime      EffectiveLimit     `json:"lifetime_sec"`
	LifetimeSpan  EffectiveLimit     `json:"lifetime_span_sec"`
	LifetimeLease EffectiveLimit     `json:"lifetime_lease_sec"`
	NoEndAllowed  bool               `json:"no_end_allowed"`
	Idle          *EffectiveLimit    `json:"idle_sec,omitempty"`
	Concurrency   []ConcurrencyBound `json:"concurrency"`
}

// RunEndReason says why the platform ended a run. It is the audit row's `reason`
// and the run detail's sentence, and it rides the run's status_detail as
// "lifetime: <reason>" when the run is KILLED at its end or its maximum.
type RunEndReason string

const (
	// RunEndMaxLifetimeReached: the run reached its effective maximum lifetime and was KILLED.
	RunEndMaxLifetimeReached RunEndReason = "max_lifetime_reached"
	// The five ways the pool queue ends a run that waited (docs/design/0.9/pool-queue.md section 9).
	// A run that waited too long, or whose profile, policy or portal changed under it, is FAILED; a
	// run whose pool was removed is KILLED. identity_deactivated is the existing token, FAILED here.
	RunEndQueueTimeout        RunEndReason = "queue_timeout"
	RunEndQueueProfileChanged RunEndReason = "queue_profile_changed"
	RunEndQueuePolicyChanged  RunEndReason = "queue_policy_changed"
	RunEndQueuePortalRevoked  RunEndReason = "queue_portal_revoked"
	RunEndRunnerPoolRemoved   RunEndReason = "runner_pool_removed"
	RunEndIdentityDeactivated RunEndReason = "identity_deactivated"
)

// runEndSentences is the one sentence per end reason.
var runEndSentences = map[RunEndReason]string{
	RunEndMaxLifetimeReached:  "This run reached its maximum lifetime and was ended.",
	RunEndEndReached:          "This run reached its end time and was ended.",
	RunEndQueueTimeout:        "This run waited too long for a free slot and was not started.",
	RunEndQueueProfileChanged: "Your governance profile changed while this run waited. Start it again.",
	RunEndQueuePolicyChanged:  "The policy this run used changed while it waited. Start it again to review it.",
	RunEndQueuePortalRevoked:  "The portal that launched this run was removed while it waited.",
	RunEndRunnerPoolRemoved:   "The pool this run was waiting for was removed.",
	RunEndIdentityDeactivated: "This account's access has been removed. Sign in again, or ask your Wardyn admin.",
}

// RunEndEndReached: a Background task reached its own end time, which came
// before its maximum lifetime, and was KILLED.
const RunEndEndReached RunEndReason = "end_reached"

// Valid reports whether r is a defined end reason.
func (r RunEndReason) Valid() bool { _, ok := runEndSentences[r]; return ok }

// Sentence is what the run's detail page and the audit view say.
func (r RunEndReason) Sentence() string { return runEndSentences[r] }

// Apply is a request's value against the limit: 0 asks for the default, a value
// within the cap is kept, and one past it is reduced to the cap with capped set.
// Reduced, never refused, as a request's CPU and memory are today.
func (l EffectiveLimit) Apply(asked int) (applied LimitAmount, capped bool) {
	switch {
	case asked <= 0:
		return l.Default, false
	case l.Cap.Unlimited || asked <= l.Cap.Value:
		return LimitAmount{Value: asked}, false
	}
	return l.Cap, true
}
