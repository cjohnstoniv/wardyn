// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package approval

import (
	"encoding/json"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Hold reports whether ap is currently parking its run's sandbox and, for a bounded hold,
// when parking ends. Ported from ui's now-retired isHeld (same arm order, same three
// windows in internal/types/holds.go, no behavior change).
//
// A row that is not PENDING is never held — a decision or the expiry sweeper already
// answered the question — so every decided/EXPIRED/CANCELLED row returns (false, zero)
// regardless of kind or age; only the server's own state ends a hold.
//
// until is the zero time.Time for an unconditional hold (tool_call outside the
// ADO-escalation shape, credential_reauth) and for a row not held at all.
func Hold(ap types.ApprovalRequest, now time.Time) (held bool, until time.Time) {
	if ap.State != types.ApprovalPending {
		return false, time.Time{}
	}
	// An ADO capability escalation is a tool_call row, but the proxy releases ITS hold after
	// HoldWindowADOCapability while the row stays PENDING. TallyKey is the same structural
	// test CancelForRun uses, reused so the two can never disagree about which rows are ADO escalations.
	if TallyKey(ap) == TallyToolCallADO {
		return boundedHold(ap.RequestedAt, now, types.HoldWindowADOCapability)
	}
	// PENDING alone is live for both of these, at any age: only the row's own server-side
	// expiry (approval.ExpireStale) ends it — no client-side ceiling belongs here.
	if ap.Kind == types.ApprovalToolCall || ap.Kind == types.ApprovalCredentialReauth {
		return true, time.Time{}
	}
	// push_content is a SHORT proxy hold, unlike tool_call/credential_reauth's unconditional
	// PENDING-is-live: the proxy refuses with a timeout after push_rules.hold_seconds (at
	// most HoldWindowPush), and a retry of the same push rejoins the row and re-enters the hold.
	if ap.Kind == types.ApprovalPushContent {
		return boundedHold(ap.RequestedAt, now, types.HoldWindowPush)
	}
	// Every other kind is held only while requested_scope carries wait_for_review — a
	// passive deny_with_review (or no mode) is never held, at any age.
	if approvalMode(ap) != "wait_for_review" {
		return false, time.Time{}
	}
	return boundedHold(ap.RequestedAt, now, types.HoldWindowEgress)
}

// approvalMode reads requested_scope.mode, or "" for a malformed/absent scope — the same
// default isHeld's TS twin used, which resolves to "not held" for an unclassifiable shape.
func approvalMode(ap types.ApprovalRequest) string {
	var sc struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(ap.RequestedScope, &sc)
	return sc.Mode
}

// boundedHold is TS's isStale, inverted: held is true strictly before requestedAt+window (a
// request raised exactly `window` ago has already timed out at the proxy); until is always
// requestedAt+window, and callers project HeldUntil only when held is also true.
func boundedHold(requestedAt, now time.Time, window time.Duration) (held bool, until time.Time) {
	until = requestedAt.Add(window)
	return now.Before(until), until
}
