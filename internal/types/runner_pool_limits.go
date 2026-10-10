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

// RunnerPoolDuration is a lifetime or an idle-stop limit, in seconds. It has
// exactly one of two forms: a finite MaxSec, with DefaultSec from 1 to MaxSec,
// or Unlimited, with MaxSec 0 and a DefaultSec of 0 (no end, or never stop
// for idleness, by default) up to RunnerPoolSecondsMax. Unlimited is never
// encoded by a zero MaxSec alone, so an omitted number cannot read as it. For an
// idle stop, Unlimited is "never".
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
type RunnerPoolBackgroundLimits struct {
	CPUMillis RunnerPoolAmount   `json:"cpu_millis"`
	MemoryMiB RunnerPoolAmount   `json:"memory_mib"`
	Lifetime  RunnerPoolDuration `json:"lifetime"`
}

// RunnerPoolInteractiveLimits are a pool's limits for Interactive environments,
// which alone have an idle stop.
type RunnerPoolInteractiveLimits struct {
	CPUMillis RunnerPoolAmount   `json:"cpu_millis"`
	MemoryMiB RunnerPoolAmount   `json:"memory_mib"`
	Lifetime  RunnerPoolDuration `json:"lifetime"`
	Idle      RunnerPoolDuration `json:"idle"`
}

// RunnerPoolLimits is everything a pool limits. A run type with limits is a run
// type the pool allows, and one without is refused, so "allowed" and "limited"
// cannot disagree. A pool with no RunnerPoolLimits at all adds no bound: only
// governance, the deployment and the runner bind its runs.
type RunnerPoolLimits struct {
	Background  *RunnerPoolBackgroundLimits  `json:"background,omitempty"`
	Interactive *RunnerPoolInteractiveLimits `json:"interactive,omitempty"`
	// Barriers are the confinement classes (Fence CC1, Wall CC2, Vault CC3) the
	// pool allows. A run on any other is refused.
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
			b.CPUMillis.validate("background.cpu_millis", "milli-CPU", RunnerPoolCPUMillisMax),
			b.MemoryMiB.validate("background.memory_mib", "MiB", RunnerPoolMemoryMiBMax),
			b.Lifetime.validate("background.lifetime"),
		); err != nil {
			return err
		}
	}
	if i := l.Interactive; i != nil {
		if err := errors.Join(
			i.CPUMillis.validate("interactive.cpu_millis", "milli-CPU", RunnerPoolCPUMillisMax),
			i.MemoryMiB.validate("interactive.memory_mib", "MiB", RunnerPoolMemoryMiBMax),
			i.Lifetime.validate("interactive.lifetime"),
			i.Idle.validate("interactive.idle"),
		); err != nil {
			return err
		}
	}
	if len(l.Barriers) == 0 {
		return errors.New("barriers: a pool allows at least one barrier")
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
	return slices.Contains(l.Barriers, c)
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
	LimitSourcePool           LimitSource = "pool"
	LimitSourceGovernance     LimitSource = "governance"
	LimitSourceDeployment     LimitSource = "deployment"
	LimitSourceRunnerCapacity LimitSource = "runner_capacity"
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
	RunType       RunnerPoolRunType  `json:"run_type"`
	CPUMillis     EffectiveLimit     `json:"cpu_millis"`
	MemoryMiB     EffectiveLimit     `json:"memory_mib"`
	Lifetime      EffectiveLimit     `json:"lifetime_sec"`
	LifetimeSpan  EffectiveLimit     `json:"lifetime_span_sec"`
	LifetimeLease EffectiveLimit     `json:"lifetime_lease_sec"`
	NoEndAllowed  bool               `json:"no_end_allowed"`
	Idle          *EffectiveLimit    `json:"idle_sec,omitempty"`
	Concurrency   []ConcurrencyBound `json:"concurrency"`
}

// RunEndReason says why the platform ended a run. It is the audit row's `reason`
// and the run detail's sentence; how a run carries it is the lifetime lane's.
type RunEndReason string

// RunEndMaxLifetimeReached: the run reached its effective maximum lifetime and
// was KILLED.
const RunEndMaxLifetimeReached RunEndReason = "max_lifetime_reached"

// Valid reports whether r is a defined end reason.
func (r RunEndReason) Valid() bool { return r == RunEndMaxLifetimeReached }

// Sentence is what the run's detail page and the audit view say.
func (r RunEndReason) Sentence() string {
	if r == RunEndMaxLifetimeReached {
		return "This run reached its maximum lifetime and was ended."
	}
	return ""
}

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
