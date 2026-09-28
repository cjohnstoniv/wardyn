// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"fmt"
	"math"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxRunLimitSec bounds every run-limit duration at math.MaxInt32 seconds
// (about 68 years): past any lease anyone means, well inside time.Duration, so
// created_at + limit never wraps into the past, and within the 32-bit
// agent_runs.wait_budget_sec a profile wait is captured into.
const maxRunLimitSec = math.MaxInt32

// runLimitsRefusal is the write boundary on a profile's run limits (long-holds
// design rev 4, §2.2), or "" when they are writable. A default past its own
// max is refused rather than clamped: it is one admin's form contradicting
// itself, and saving it would leave the editor showing a default no run gets.
func runLimitsRefusal(l types.RunLimits) string {
	for _, f := range []struct {
		field string
		sec   int
	}{
		{"max_end_ahead_sec", l.MaxEndAheadSec},
		{"default_end_sec", l.DefaultEndSec},
		{"max_wait_sec", l.MaxWaitSec},
		{"default_wait_sec", l.DefaultWaitSec},
		{"pause_idle_after_sec", l.PauseIdleAfterSec},
	} {
		if f.sec < 0 || f.sec > maxRunLimitSec {
			return fmt.Sprintf("limits.%s: %d is not a number of seconds from 0 to %d — use 0 for unset",
				f.field, f.sec, maxRunLimitSec)
		}
	}
	if l.MaxEndAheadSec > 0 && l.DefaultEndSec > l.MaxEndAheadSec {
		return fmt.Sprintf("limits.default_end_sec: %d is past limits.max_end_ahead_sec (%d)",
			l.DefaultEndSec, l.MaxEndAheadSec)
	}
	if l.MaxWaitSec > 0 && l.DefaultWaitSec > l.MaxWaitSec {
		return fmt.Sprintf("limits.default_wait_sec: %d is past limits.max_wait_sec (%d)",
			l.DefaultWaitSec, l.MaxWaitSec)
	}
	return ""
}

// captureRunLimits stamps a new run with its owner's run limits, the profile
// they came from, its end and its wait. ceiling is the OWNER's, resolved for
// this create: an operator's carries no profile, so a super admin's run is
// bounded by the deployment alone, while a security admin's resolves a profile
// like any member's. Captured once and never re-resolved, so a later change to
// the end or the wait clamps against the bounds the run launched under.
func (s *Server) captureRunLimits(run *types.AgentRun, ceiling governanceCeiling) {
	l := ceiling.Limits.RunLimits
	run.RunLimits = l
	if ceiling.Profile != nil {
		id := ceiling.Profile.ID
		run.GovernanceProfileID = &id
	}
	if end := cmp.Or(l.DefaultEndSec, l.MaxEndAheadSec); end > 0 {
		endsAt := run.CreatedAt.Add(time.Duration(end) * time.Second)
		run.EndsAt = &endsAt
	}
	run.WaitBudgetSec = runWaitSec(l, s.cfg.ApprovalExpiryAfter)
}

// runWaitSec is a new run's wait for a decision: the profile's default, else
// its max, else the deployment's approval expiry, and never past the
// deployment's approval expiry, whose sweep binds every request anyway. 0 means
// no bound is known here (no profile wait and no deployment expiry).
func runWaitSec(l types.RunLimits, deployment time.Duration) int {
	ceiling := waitCeilingSec(l, deployment)
	wait := cmp.Or(l.DefaultWaitSec, ceiling)
	if ceiling > 0 {
		wait = min(wait, ceiling)
	}
	return wait
}

// waitCeilingSec is the longest wait a run may have: the tighter of the
// profile's max and the deployment's approval expiry, or 0 when neither is set.
func waitCeilingSec(l types.RunLimits, deployment time.Duration) int {
	ceiling := int(deployment / time.Second)
	if l.MaxWaitSec > 0 && (ceiling <= 0 || l.MaxWaitSec < ceiling) {
		ceiling = l.MaxWaitSec
	}
	return ceiling
}
