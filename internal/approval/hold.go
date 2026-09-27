// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package approval

import (
	"encoding/json"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Hold reports whether ap is currently parking its run's sandbox, and, for a
// BOUNDED hold, when that parking ends. It is a verbatim port of
// ui/src/app/lib/types/approvals.ts's now-retired isHeld: same arm order,
// same three windows (internal/types/holds.go), no behaviour change
// (#1197 decision brief, "the rule", row 1).
//
// A row that is not PENDING is never held — a decision (or the expiry
// sweeper) already answered the question a hold was asking — so every
// decided/EXPIRED/CANCELLED row returns (false, zero) regardless of kind or
// age. This is the SAME "only the server's own state ends the hold" posture
// #509 established for tool_call/credential_reauth, generalized to the
// function's one entry gate rather than repeated in every arm.
//
// until is the zero time.Time for an UNCONDITIONAL hold (tool_call outside
// the ADO-escalation shape, credential_reauth) — nothing bounds it but the
// row's own PENDING-ness — and for a row that is not held at all.
func Hold(ap types.ApprovalRequest, now time.Time) (held bool, until time.Time) {
	if ap.State != types.ApprovalPending {
		return false, time.Time{}
	}
	// An Azure DevOps capability escalation is a tool_call row, but the proxy
	// releases ITS hold after HoldWindowADOCapability
	// (credhold.go's maxCapabilityHoldTimeout) while the row stays PENDING.
	// TallyKey is the SAME structural test CancelForRun already uses to tell
	// this shape apart from a plain tool_call — reused rather than
	// re-derived, so the two can never disagree about which rows are ADO
	// escalations.
	if TallyKey(ap) == TallyToolCallADO {
		return boundedHold(ap.RequestedAt, now, types.HoldWindowADOCapability)
	}
	// #509 — PENDING alone is live for both of these, at any age: the row's
	// own server-side expiry (approval.ExpireStale) ends it, and only that
	// ends it. No client-side ceiling belongs here.
	if ap.Kind == types.ApprovalToolCall || ap.Kind == types.ApprovalCredentialReauth {
		return true, time.Time{}
	}
	// push_content is a SHORT proxy hold, unlike tool_call/credential_reauth's
	// unconditional PENDING-is-live: the proxy refuses with a timeout after
	// push_rules.hold_seconds (at most HoldWindowPush) and the row then stays
	// a PASSIVE pending — a retry of the same push rejoins the row and
	// re-enters the hold.
	if ap.Kind == types.ApprovalPushContent {
		return boundedHold(ap.RequestedAt, now, types.HoldWindowPush)
	}
	// Every other kind (egress_domain, plain credential) is held only while
	// its requested_scope carries the proxy's own wait_for_review mode — a
	// passive deny_with_review (or any other mode/no mode at all) is never
	// held, at any age.
	if approvalMode(ap) != "wait_for_review" {
		return false, time.Time{}
	}
	return boundedHold(ap.RequestedAt, now, types.HoldWindowEgress)
}

// approvalMode reads requested_scope.mode, or "" for a malformed/absent
// scope — the same default isHeld's TS twin fell back to
// ((a.requested_scope?.mode as string) ?? ""), which resolves to "not held"
// for a shape this arm cannot classify.
func approvalMode(ap types.ApprovalRequest) string {
	var sc struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(ap.RequestedScope, &sc)
	return sc.Mode
}

// boundedHold is TS's isStale, inverted and reworked: held is true strictly
// before requestedAt+window (the boundary itself is NOT held — a request
// raised exactly `window` ago has already timed out at the proxy), and until
// is always requestedAt+window regardless of whether it has already passed —
// callers project HeldUntil only when held is also true (see projectHolds).
func boundedHold(requestedAt, now time.Time, window time.Duration) (held bool, until time.Time) {
	until = requestedAt.Add(window)
	return now.Before(until), until
}
