// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The reasons and sentences of the repeating schedule routes (/run-schedules, run_schedule_repeat.go), kept
// beside them. TestReasonDocsMatchReasonsGo and TestNoAdHocErrorBodyOrReasonLiteral read this file through
// reasonsGoFiles.
const (
	reasonRunScheduleInvalid          = "run_schedule_invalid"           // 400: every, time, time_zone or a start field in request
	reasonRunScheduleOwnerUnsupported = "run_schedule_owner_unsupported" // 422: only a person signed in through SSO keeps a schedule
	reasonRunScheduleLimit            = "run_schedule_limit"             // 409: the owner has the most schedules
	reasonRunScheduleNotFound         = "run_schedule_not_found"         // 404
)

const (
	runScheduleInvalidMsg          = "A schedule needs every (daily, weekdays or weekly), time (HH:MM) and time_zone (an IANA zone such as Europe/London), and its request carries no start_at."
	runScheduleOwnerUnsupportedMsg = "Only a person signed in with single sign-on can keep a repeating schedule."
	runScheduleLimitMsg            = "You have 20 schedules, the most allowed. Delete one first."
	runScheduleNotFoundMsg         = "No such schedule."
	// runScheduleResumeOffMsg answers a resume while WARDYN_SCHEDULED_RUNS is off, with reasonScheduledRunsOff.
	runScheduleResumeOffMsg = "Scheduled runs are off on this server, so this schedule can't resume."
	// runScheduleResumeDeniedMsg answers a resume the governance profile's deny_scheduled_runs refuses, with
	// reasonScheduledRunsDenied.
	runScheduleResumeDeniedMsg = "Your team can't schedule runs now, so this schedule can't resume. Ask your admin."
)
