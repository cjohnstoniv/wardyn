// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// This file is the governance-profile meet: ApplyOverlay narrows a resolved
// ceiling by an overlay, field by field, and can never widen it.
//
// It is deliberately NOT Clamp. Clamp bounds an untrusted proposal against an
// operator ceiling: it drops workspace mounts, replaces llm_inspection with the
// operator's mode and defaults unnamed tools to hold. A meet of two
// operator-authored ceilings is a different operation with its own rule per
// field (the table in docs/design/0.8/0.8.6-comp.md), so neither borrows from
// the other.
//
// Every rule works on values the RUNTIME would read, not on raw ones: a field
// whose zero means a finite default (a 32 MiB pack cap, a 120 s hold, a 30 s
// first-use hold, a 16-hold cap, the deployment's sandbox size) is normalised to
// that default before it meets, and the result is written back explicitly. A
// "smaller positive" over raw values widens wherever the runtime gives zero a
// finite meaning.

// Authority is one resolved governance ceiling: the policy a run is bounded by
// and the limits that bound the request.
type Authority struct {
	Ceiling types.RunPolicySpec
	Limits  types.GovernanceLimits
}

// Overlay is what a composed profile says on top of its base. Presence is
// typed, so a field the overlay does not name is inherited unchanged.
type Overlay struct {
	Ceiling types.CeilingOverlay
	Limits  types.LimitsOverlay
}

// The wire reasons an overlay refusal carries. governance_overlay_invalid is a
// 400 at write; governance_overlay_unsatisfiable is the explicit deny-all state
// (a base that narrowed under an overlay until nothing satisfies both).
const (
	ReasonOverlayInvalid       = "governance_overlay_invalid"
	ReasonOverlayUnsatisfiable = "governance_overlay_unsatisfiable"
)

// OverlayError is a refusal to compose. Field is the wire name of the field (or
// field.sub) that caused it.
type OverlayError struct {
	Reason string
	Field  string
	Detail string
}

func (e *OverlayError) Error() string { return fmt.Sprintf("%s: %s: %s", e.Reason, e.Field, e.Detail) }

// ApplyOverlay returns base narrowed by ov, plus a warning for each thing the
// overlay named that the base does not permit (an entry the base no longer
// covers, a bound looser than the base's): the meet drops those rather than
// widen. This is the resolve-time reading, where the base may have narrowed
// since the overlay was written. It returns an error, and a zero Authority,
// when no representable result exists: a disjoint allowed_methods or
// azure_devops_capabilities (never [], which both read as "all"), two different
// llm_inspection values, or limits the writer would refuse.
func ApplyOverlay(base Authority, ov Overlay) (Authority, []string, error) {
	m := meeter{out: cloneAuthority(base)}
	m.run(ov)
	if m.err != nil {
		return Authority{}, nil, m.err
	}
	return m.out, m.warns, nil
}

// ValidateOverlay is the write-time reading of the same meet: it refuses an
// overlay that names anything its base does not permit (a domain outside the
// base, a method the base excludes, allow_all_egress on a base without it, a
// looser bound), where ApplyOverlay would silently narrow it. A write is the
// author's own act and can be sent back; a base edit arriving between two runs
// is not, which is why resolve takes the lenient path.
func ValidateOverlay(base Authority, ov Overlay) error {
	m := meeter{out: cloneAuthority(base), strict: true}
	m.run(ov)
	if m.err != nil {
		return m.err
	}
	return nil
}

// meeter carries one composition. out starts as a copy of the base and each
// rule overwrites the fields the overlay names.
type meeter struct {
	out    Authority
	strict bool
	warns  []string
	err    *OverlayError
}

func (m *meeter) run(ov Overlay) {
	m.validatePresence(ov)
	if m.err != nil {
		return
	}
	m.meetEgress(ov.Ceiling)
	m.meetPosture(ov.Ceiling)
	m.meetPush(ov.Ceiling.PushRules)
	m.meetLimits(ov.Limits)
	m.checkResult()
}

// fail records the first hard refusal; later rules still run on a value the
// caller will never see.
func (m *meeter) fail(reason, field, format string, args ...any) {
	if m.err == nil {
		m.err = &OverlayError{Reason: reason, Field: field, Detail: fmt.Sprintf(format, args...)}
	}
}

// widen records that the overlay named something looser than the base permits:
// refused at write, narrowed (with a warning) at resolve.
func (m *meeter) widen(field, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	if m.strict {
		m.fail(ReasonOverlayInvalid, field, "%s", detail)
		return
	}
	m.warns = append(m.warns, field+": "+detail)
}

func cloneAuthority(a Authority) Authority {
	out := Authority{Ceiling: cloneProposal(a.Ceiling), Limits: a.Limits}
	if a.Limits.AutonomyRubric != nil {
		r := *a.Limits.AutonomyRubric
		out.Limits.AutonomyRubric = &r
	}
	return out
}

// validatePresence refuses the overlay shapes that have no meaning whatever the
// base is: an empty list for a field where empty reads as "everything", a
// negative number, an unknown enum value, a resolved secret value.
func (m *meeter) validatePresence(ov Overlay) {
	c := ov.Ceiling
	if c.AllowedMethods != nil && len(normMethods(*c.AllowedMethods)) == 0 {
		m.fail(ReasonOverlayInvalid, "allowed_methods", "an empty list would allow every method; omit the field to inherit the base's")
	}
	if c.AzureDevOpsCapabilities != nil && len(*c.AzureDevOpsCapabilities) == 0 {
		m.fail(ReasonOverlayInvalid, "azure_devops_capabilities", "an empty list would mean the provider row's default profile; omit the field to inherit the base's")
	}
	if c.LLMInspection != nil && len(c.LLMInspection.WorkspaceSecretValues) > 0 {
		m.fail(ReasonOverlayInvalid, "llm_inspection.workspace_secret_values", "resolved secret values are never authored")
	}
	if c.ToolRules != nil {
		for _, r := range *c.ToolRules {
			if !types.ValidToolEffect(r.Effect) {
				m.fail(ReasonOverlayInvalid, "tool_rules", "tool %q has unknown effect %q", r.Tool, r.Effect)
			}
		}
	}
	m.validateNonNegative(ov)
	if r := ov.Limits.AutonomyRubric; r != nil {
		if err := r.Validate(); err != nil {
			m.fail(ReasonOverlayInvalid, "autonomy_rubric", "%v", err)
		}
	}
}

// validateNonNegative refuses a negative count or duration. auto_stop_after_sec
// is the one field where a negative is a stated intent (never reap).
func (m *meeter) validateNonNegative(ov Overlay) {
	c, l := ov.Ceiling, ov.Limits
	fields := []struct {
		name string
		v    *int
	}{
		{"first_use_hold_seconds", c.FirstUseHoldSeconds}, {"max_holds", c.MaxHolds},
		{"max_concurrent_runs", l.MaxConcurrentRuns}, {"max_cpu_millis", l.MaxCPUMillis},
		{"max_memory_mib", l.MaxMemoryMiB}, {"max_ephemeral_disk_mib", l.MaxEphemeralDiskMiB},
		{"max_drive_size_mib", l.MaxDriveSizeMiB}, {"max_end_ahead_sec", l.MaxEndAheadSec},
		{"default_end_sec", l.DefaultEndSec}, {"max_wait_sec", l.MaxWaitSec},
		{"default_wait_sec", l.DefaultWaitSec}, {"pause_idle_after_sec", l.PauseIdleAfterSec},
	}
	if r := c.Resources; r != nil {
		fields = append(fields, []struct {
			name string
			v    *int
		}{{"resources.cpu_millis", r.CPUMillis}, {"resources.memory_mib", r.MemoryMiB},
			{"resources.pids_limit", r.PidsLimit}, {"resources.disk_mib", r.DiskMiB}}...)
	}
	if p := c.PushRules; p != nil {
		fields = append(fields, []struct {
			name string
			v    *int
		}{{"push_rules.max_inspect_pack_mib", p.MaxInspectPackMiB}, {"push_rules.hold_seconds", p.HoldSeconds},
			{"push_rules.max_file_size_mib", p.MaxFileSizeMiB}}...)
	}
	for _, f := range fields {
		if f.v != nil && *f.v < 0 {
			m.fail(ReasonOverlayInvalid, f.name, "%d is not a count; use 0 for the default or unlimited", *f.v)
		}
	}
}

// checkResult is the last door: the composed limits must be ones the profile
// writer would accept, and a default may not sit past its own maximum. A result
// the writer would refuse counts as unsatisfiable, never as a clamp.
func (m *meeter) checkResult() {
	l := m.out.Limits
	if l.DefaultEndSec < 0 || l.DefaultWaitSec < 0 || l.MaxEndAheadSec < 0 || l.MaxWaitSec < 0 || l.PauseIdleAfterSec < 0 ||
		l.MaxConcurrentRuns < 0 || l.MaxCPUMillis < 0 || l.MaxMemoryMiB < 0 || l.MaxEphemeralDiskMiB < 0 || l.MaxDriveSizeMiB < 0 {
		m.fail(ReasonOverlayUnsatisfiable, "limits", "the composed limits carry a negative value")
	}
	if l.MaxEndAheadSec > 0 && l.DefaultEndSec > l.MaxEndAheadSec {
		m.fail(ReasonOverlayUnsatisfiable, "limits.default_end_sec", "the composed default is past the composed maximum")
	}
	if l.MaxWaitSec > 0 && l.DefaultWaitSec > l.MaxWaitSec {
		m.fail(ReasonOverlayUnsatisfiable, "limits.default_wait_sec", "the composed default is past the composed maximum")
	}
	if l.AutonomyRubric != nil {
		if err := l.AutonomyRubric.Validate(); err != nil {
			m.fail(ReasonOverlayUnsatisfiable, "limits.autonomy_rubric", "%v", err)
		}
	}
}

// tightest is the smaller of two bounds where a value <= 0 is no bound; two
// unbounded sides stay unbounded (0).
func tightest(a, b int) int {
	switch {
	case a <= 0:
		return max(b, 0)
	case b <= 0:
		return a
	}
	return min(a, b)
}

// looser reports whether an overlay bound o is weaker than the base's b, where
// a value <= 0 is no bound.
func looser(o, b int) bool {
	if b <= 0 {
		return false
	}
	return o <= 0 || o > b
}

func derefBool(p *bool) bool { return p != nil && *p }

// trimJoin names a list of entries in a message.
func trimJoin(xs []string) string { return strings.Join(xs, ",") }
