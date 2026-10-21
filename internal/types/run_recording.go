// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// RecordingRequirement is whether an organisation or a runner pool requires its runs recorded. Empty
// reads as preferred, which is how every organisation and pool behaved before the setting existed.
type RecordingRequirement string

const (
	// RecordingPreferred records a run whenever its organisation and runner can, and says on the run
	// when it is not recorded.
	RecordingPreferred RecordingRequirement = "preferred"
	// RecordingRequired refuses a run on a runner that does not record (runner_recording_off).
	RecordingRequired RecordingRequirement = "required"
)

// Valid reports whether r is empty, preferred or required.
func (r RecordingRequirement) Valid() bool {
	return r == "" || r == RecordingPreferred || r == RecordingRequired
}

// RunNotRecorded says why a run has no session recording. Empty: the run is recorded.
type RunNotRecorded string

const (
	// NotRecordedOrgOff: the organisation keeps no recordings (no recording store).
	NotRecordedOrgOff RunNotRecorded = "org_off"
	// NotRecordedRunnerDeclined: the run's runner does not record, by its own setting
	// (--recording-store off) or because it predates the per-run recording flag.
	NotRecordedRunnerDeclined RunNotRecorded = "runner_declined"
	// NotRecordedPolicyOff: the run's recording level resolved to off (RecordingOff).
	NotRecordedPolicyOff RunNotRecorded = "policy_off"
)

// NotRecordedFor is a run's NotRecorded when its organisation keeps recordings (orgOn) and its target
// records a run its organisation asks it to (targetRecords).
func NotRecordedFor(orgOn, targetRecords bool) RunNotRecorded {
	switch {
	case !orgOn:
		return NotRecordedOrgOff
	case !targetRecords:
		return NotRecordedRunnerDeclined
	}
	return ""
}

// RecordingLevel is how much of a run is recorded. Keystrokes are never recorded at any level: no
// value names them, and the desktop relay never reads the browser's side of the stream.
type RecordingLevel string

const (
	// RecordingOff records no terminal or screen. The audit log still shows what happened.
	RecordingOff RecordingLevel = "off"
	// RecordingTerminal records terminal sessions only: what the organisation recorded before the level
	// existed, and the level when nothing chooses one.
	RecordingTerminal RecordingLevel = "terminal"
	// RecordingScreenActions also records a desktop's screen and an action log of what was opened,
	// copied and transferred.
	RecordingScreenActions RecordingLevel = "screen_actions"
)

// Valid reports whether l is empty (no choice) or a level.
func (l RecordingLevel) Valid() bool {
	return l == "" || l == RecordingOff || l == RecordingTerminal || l == RecordingScreenActions
}

// Rank orders the levels, off lowest; empty ranks below off, so any choice is at least no choice.
func (l RecordingLevel) Rank() int {
	switch l {
	case RecordingOff:
		return 1
	case RecordingTerminal:
		return 2
	case RecordingScreenActions:
		return 3
	}
	return 0
}

// AtLeast is the higher of l and floor: a lower layer may record more, never less.
func (l RecordingLevel) AtLeast(floor RecordingLevel) RecordingLevel {
	if floor.Rank() > l.Rank() {
		return floor
	}
	return l
}

// SiteRecording is the organisation's recording level. Level is the level a run gets when its policy
// chooses none (empty: terminal). Locked makes Level a minimum: no policy, profile or run goes under
// it, an operator's included.
type SiteRecording struct {
	Level  RecordingLevel `json:"level,omitempty"`
	Locked bool           `json:"locked,omitempty"`
}

// OrgLevel is the organisation's level: terminal when nothing chose one.
func (r *SiteRecording) OrgLevel() RecordingLevel {
	if r == nil || r.Level == "" {
		return RecordingTerminal
	}
	return r.Level
}

// RecordingChoice is what a run's recording level is resolved from.
type RecordingChoice struct {
	// Policy is the run's own policy's level, already raised to its ceiling's (composer.Clamp).
	Policy RecordingLevel
	// MemberAuthored is a member's own inline policy, and Ceiling is the level of the ceiling it was
	// clamped to. A member's policy under a ceiling that chooses no level may not go under the
	// organisation's level: a member could not opt out of recording before levels, and still cannot.
	MemberAuthored bool
	Ceiling        RecordingLevel
	Org            *SiteRecording
	// Required is the organisation's or the run's pool's required setting: at least terminal.
	Required bool
}

// Resolve is the run's recording level. Whether the organisation keeps recordings at all
// (WARDYN_RECORDING_STORE) is not part of it: off there is off for every level.
func (c RecordingChoice) Resolve() RecordingLevel {
	org := c.Org.OrgLevel()
	l := c.Policy
	if l == "" || (c.MemberAuthored && c.Ceiling == "") || (c.Org != nil && c.Org.Locked) {
		l = l.AtLeast(org)
	}
	if c.Required {
		l = l.AtLeast(RecordingTerminal)
	}
	return l
}
