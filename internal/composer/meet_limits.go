// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"cmp"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// meetLimits composes GovernanceLimits: every switch that refuses a request
// shape only turns on, every size or duration bound takes the smaller positive
// value (0 is no bound), and the autonomy rubric takes the lower level per
// field.
func (m *meeter) meetLimits(o types.LimitsOverlay) {
	l := &m.out.Limits
	// A denial is only ever added: one the overlay leaves off stays the base's.
	for _, f := range []struct {
		base *bool
		ov   *bool
	}{
		{&l.DenyTaskModeExec, o.DenyTaskModeExec},
		{&l.DenyInteractive, o.DenyInteractive},
		{&l.DenyUIApps, o.DenyUIApps},
		{&l.DenyUserDrive, o.DenyUserDrive},
	} {
		if f.ov != nil {
			*f.base = *f.base || *f.ov
		}
	}
	for _, f := range []struct {
		name string
		base *int
		ov   *int
	}{
		{"max_concurrent_runs", &l.MaxConcurrentRuns, o.MaxConcurrentRuns},
		{"max_cpu_millis", &l.MaxCPUMillis, o.MaxCPUMillis},
		{"max_memory_mib", &l.MaxMemoryMiB, o.MaxMemoryMiB},
		{"max_ephemeral_disk_mib", &l.MaxEphemeralDiskMiB, o.MaxEphemeralDiskMiB},
		{"max_drive_size_mib", &l.MaxDriveSizeMiB, o.MaxDriveSizeMiB},
	} {
		if f.ov == nil {
			continue
		}
		if looser(*f.ov, *f.base) {
			m.widen(f.name, "%d is a weaker bound than the base's %d", *f.ov, *f.base)
		}
		*f.base = tightest(*f.base, *f.ov)
	}
	if o.AutonomyRubric != nil {
		m.meetRubric(o.AutonomyRubric)
	}
	m.meetRunLimits(o)
}

// meetRunLimits composes the run-lifetime bounds. The bound fields go through
// the same TightenRunLimits the live-run reclamp uses, so a running child is cut
// to exactly what a new run gets. The two defaults add the one thing that
// function leaves alone: a default is the smaller of the two after 0 is read as
// "the maximum", then clamped to the composed maximum.
func (m *meeter) meetRunLimits(o types.LimitsOverlay) {
	if o.MaxEndAheadSec == nil && o.DefaultEndSec == nil && o.AllowNoEnd == nil && o.MaxWaitSec == nil &&
		o.DefaultWaitSec == nil && o.UserChangesLimits == nil && o.PauseIdleAfterSec == nil {
		return
	}
	l := &m.out.Limits
	base := l.RunLimits
	ov := types.RunLimits{
		MaxEndAheadSec:    derefInt(o.MaxEndAheadSec),
		MaxWaitSec:        derefInt(o.MaxWaitSec),
		PauseIdleAfterSec: derefInt(o.PauseIdleAfterSec),
		// The identity of AND, so a flag the overlay does not name is the base's.
		AllowNoEnd:        o.AllowNoEnd == nil || *o.AllowNoEnd,
		UserChangesLimits: o.UserChangesLimits == nil || *o.UserChangesLimits,
	}
	got := TightenRunLimits(base, ov)
	m.widenIf("max_end_ahead_sec", o.MaxEndAheadSec != nil && looser(*o.MaxEndAheadSec, base.MaxEndAheadSec))
	m.widenIf("max_wait_sec", o.MaxWaitSec != nil && looser(*o.MaxWaitSec, base.MaxWaitSec))
	m.widenIf("pause_idle_after_sec", o.PauseIdleAfterSec != nil && looser(*o.PauseIdleAfterSec, base.PauseIdleAfterSec))
	m.widenIf("allow_no_end", derefBool(o.AllowNoEnd) && !base.AllowNoEnd)
	m.widenIf("user_changes_limits", derefBool(o.UserChangesLimits) && !base.UserChangesLimits)
	got.DefaultEndSec = m.meetDefault("default_end_sec", base.DefaultEndSec, base.MaxEndAheadSec, o.DefaultEndSec, got.MaxEndAheadSec)
	got.DefaultWaitSec = m.meetDefault("default_wait_sec", base.DefaultWaitSec, base.MaxWaitSec, o.DefaultWaitSec, got.MaxWaitSec)
	l.RunLimits = got
}

func (m *meeter) widenIf(field string, widens bool) {
	if widens {
		m.widen(field, "is a weaker bound than the base's")
	}
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// meetDefault meets a default end or wait. 0 reads as the side's maximum (for
// the overlay, the composed maximum, since it names no other); the smaller of
// the two is then clamped to the composed maximum, so the pair always passes the
// writer's own "default past its max" refusal.
func (m *meeter) meetDefault(field string, baseDef, baseMax int, ov *int, resMax int) int {
	if ov == nil {
		return clampDefault(baseDef, resMax)
	}
	b, v := cmp.Or(baseDef, baseMax), cmp.Or(*ov, resMax)
	if looser(v, b) {
		m.widen(field, "%d is a weaker default than the base's %d", v, b)
	}
	return clampDefault(tightest(b, v), resMax)
}

// clampDefault cuts a default to the composed maximum and spells an unset one
// (0, which reads as "the maximum") out as that maximum, so the same composition
// spells the same however its steps were ordered.
func clampDefault(def, resMax int) int {
	if resMax > 0 && (def == 0 || def > resMax) {
		return resMax
	}
	return def
}

// meetRubric takes the lower autonomy level per field; a field set on one side
// only takes that side, and a rubric that caps nothing stays nil.
func (m *meeter) meetRubric(o *types.AutonomyRubric) {
	l := &m.out.Limits
	var b types.AutonomyRubric
	if l.AutonomyRubric != nil {
		b = *l.AutonomyRubric
	}
	for _, f := range []struct {
		name string
		base *types.AutonomyLevel
		ov   types.AutonomyLevel
	}{
		{"egress_open", &b.EgressOpen, o.EgressOpen},
		{"egress_reviewed", &b.EgressReviewed, o.EgressReviewed},
		{"egress_sealed", &b.EgressSealed, o.EgressSealed},
		{"secrets_powerful", &b.SecretsPowerful, o.SecretsPowerful},
		{"secrets_baseline", &b.SecretsBaseline, o.SecretsBaseline},
		{"secrets_none", &b.SecretsNone, o.SecretsNone},
		{"confinement_cc1", &b.ConfinementCC1, o.ConfinementCC1},
		{"confinement_cc2", &b.ConfinementCC2, o.ConfinementCC2},
		{"confinement_cc3", &b.ConfinementCC3, o.ConfinementCC3},
	} {
		switch {
		case f.ov == "":
		case *f.base == "" || f.ov.Rank() < f.base.Rank():
			*f.base = f.ov
		case f.ov.Rank() > f.base.Rank():
			m.widen("autonomy_rubric."+f.name, "%s allows a higher level than the base's %s", f.ov, *f.base)
		}
	}
	if b == (types.AutonomyRubric{}) {
		l.AutonomyRubric = nil
		return
	}
	l.AutonomyRubric = &b
}
