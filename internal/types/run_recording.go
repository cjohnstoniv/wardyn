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
