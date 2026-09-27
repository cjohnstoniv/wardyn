// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package approval

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mustScope marshals v (a struct literal or map) into RequestedScope.
func mustScope(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal scope: %v", err)
	}
	return b
}

// adoScope builds the requested_scope of a control-plane-raised Azure DevOps
// capability escalation — the ONE shape TallyToolCallADO (and therefore
// Hold's ADO arm) recognises: lane=azure_devops AND requested_scope.grant_id
// equal to the row's own GrantID column.
func adoScope(t *testing.T, grantID uuid.UUID) json.RawMessage {
	return mustScope(t, map[string]any{"lane": "azure_devops", "grant_id": grantID.String(), "capability": "pr_create"})
}

// Every case here is ported from ui/src/app/lib/types/approvals.test.ts's
// now-retired isHeld suite (#1197 L1b): same fixtures, same expectations —
// this table is what proves the Go port is behaviour-identical to the TS
// rule it replaces.
func TestHold(t *testing.T) {
	now := time.Now().UTC()
	grantID := uuid.New()

	cases := []struct {
		name       string
		ap         types.ApprovalRequest
		wantHeld   bool
		wantUntil  time.Time // zero means "no until expected"
		checkUntil bool
	}{
		{
			name:     "a fresh tool_call is held",
			ap:       types.ApprovalRequest{Kind: types.ApprovalToolCall, State: types.ApprovalPending, RequestedAt: now},
			wantHeld: true,
		},
		{
			name:     "a fresh credential_reauth is held",
			ap:       types.ApprovalRequest{Kind: types.ApprovalCredentialReauth, State: types.ApprovalPending, RequestedAt: now},
			wantHeld: true,
		},
		{
			name:     "a PENDING tool_call 2 hours old is still held — no client ceiling",
			ap:       types.ApprovalRequest{Kind: types.ApprovalToolCall, State: types.ApprovalPending, RequestedAt: now.Add(-2 * time.Hour)},
			wantHeld: true,
		},
		{
			name:     "a PENDING credential_reauth 25 hours old is still held — only the server's own state ends it",
			ap:       types.ApprovalRequest{Kind: types.ApprovalCredentialReauth, State: types.ApprovalPending, RequestedAt: now.Add(-25 * time.Hour)},
			wantHeld: true,
		},
		{
			name:     "an EXPIRED tool_call is not held, regardless of age",
			ap:       types.ApprovalRequest{Kind: types.ApprovalToolCall, State: types.ApprovalExpired, RequestedAt: now},
			wantHeld: false,
		},
		{
			name:     "a DENIED credential_reauth is not held",
			ap:       types.ApprovalRequest{Kind: types.ApprovalCredentialReauth, State: types.ApprovalDenied, RequestedAt: now},
			wantHeld: false,
		},
		{
			name:     "a CANCELLED tool_call is not held",
			ap:       types.ApprovalRequest{Kind: types.ApprovalToolCall, State: types.ApprovalCancelled, RequestedAt: now},
			wantHeld: false,
		},
		{
			name: "egress wait_for_review is held while fresh, bounded at 30s",
			ap: types.ApprovalRequest{
				Kind: types.ApprovalEgressDomain, State: types.ApprovalPending, RequestedAt: now,
				RequestedScope: mustScope(t, map[string]any{"host": "h", "mode": "wait_for_review"}),
			},
			wantHeld: true, wantUntil: now.Add(types.HoldWindowEgress), checkUntil: true,
		},
		{
			name: "egress wait_for_review 60s old is past its 30s window: not held",
			ap: types.ApprovalRequest{
				Kind: types.ApprovalEgressDomain, State: types.ApprovalPending, RequestedAt: now.Add(-60 * time.Second),
				RequestedScope: mustScope(t, map[string]any{"host": "h", "mode": "wait_for_review"}),
			},
			wantHeld: false,
		},
		{
			name: "a plain credential with wait_for_review is held, same 30s window",
			ap: types.ApprovalRequest{
				Kind: types.ApprovalCredential, State: types.ApprovalPending, RequestedAt: now,
				RequestedScope: mustScope(t, map[string]any{"mode": "wait_for_review"}),
			},
			wantHeld: true, wantUntil: now.Add(types.HoldWindowEgress), checkUntil: true,
		},
		{
			name: "a passive deny_with_review pending is never held",
			ap: types.ApprovalRequest{
				Kind: types.ApprovalEgressDomain, State: types.ApprovalPending, RequestedAt: now,
				RequestedScope: mustScope(t, map[string]any{"host": "h", "mode": "deny_with_review"}),
			},
			wantHeld: false,
		},
		{
			name: "a fresh ADO capability escalation is held, bounded at 240s",
			ap: types.ApprovalRequest{
				Kind: types.ApprovalToolCall, State: types.ApprovalPending, RequestedAt: now, GrantID: &grantID,
				RequestedScope: adoScope(t, grantID),
			},
			wantHeld: true, wantUntil: now.Add(types.HoldWindowADOCapability), checkUntil: true,
		},
		{
			name: "an ADO capability escalation 5 minutes old is past its 240s window: not held",
			ap: types.ApprovalRequest{
				Kind: types.ApprovalToolCall, State: types.ApprovalPending, RequestedAt: now.Add(-5 * time.Minute), GrantID: &grantID,
				RequestedScope: adoScope(t, grantID),
			},
			wantHeld: false,
		},
		{
			name: "a plain tool_call of the same age (no grant_id) stays held — not an ADO escalation",
			ap: types.ApprovalRequest{
				Kind: types.ApprovalToolCall, State: types.ApprovalPending, RequestedAt: now.Add(-5 * time.Minute),
			},
			wantHeld: true,
		},
		{
			name: "a decided ADO capability escalation is not held",
			ap: types.ApprovalRequest{
				Kind: types.ApprovalToolCall, State: types.ApprovalApproved, RequestedAt: now, GrantID: &grantID,
				RequestedScope: adoScope(t, grantID),
			},
			wantHeld: false,
		},
		{
			name:     "a fresh push_content is held, bounded at 600s",
			ap:       types.ApprovalRequest{Kind: types.ApprovalPushContent, State: types.ApprovalPending, RequestedAt: now},
			wantHeld: true, wantUntil: now.Add(types.HoldWindowPush), checkUntil: true,
		},
		{
			name:     "a push_content past its 600s ceiling is a passive pending, not held",
			ap:       types.ApprovalRequest{Kind: types.ApprovalPushContent, State: types.ApprovalPending, RequestedAt: now.Add(-601 * time.Second)},
			wantHeld: false,
		},
		{
			name:     "a push_content well inside the 600s ceiling is still held",
			ap:       types.ApprovalRequest{Kind: types.ApprovalPushContent, State: types.ApprovalPending, RequestedAt: now.Add(-300 * time.Second)},
			wantHeld: true, wantUntil: now.Add(types.HoldWindowPush - 300*time.Second), checkUntil: true,
		},
		{
			name:     "an EXPIRED push_content is never held",
			ap:       types.ApprovalRequest{Kind: types.ApprovalPushContent, State: types.ApprovalExpired, RequestedAt: now},
			wantHeld: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			held, until := Hold(c.ap, now)
			if held != c.wantHeld {
				t.Fatalf("held = %v, want %v", held, c.wantHeld)
			}
			if c.checkUntil && !until.Equal(c.wantUntil) {
				t.Fatalf("until = %v, want %v", until, c.wantUntil)
			}
		})
	}
}

// TestHoldBoundary pins the exact instant each bounded window flips: held at
// until-1ms, not held AT until (a request raised exactly window ago has
// already timed out at the proxy — the boundary itself is not held).
func TestHoldBoundary(t *testing.T) {
	now := time.Now().UTC()
	grantID := uuid.New()

	for _, w := range []struct {
		name string
		ap   func(requestedAt time.Time) types.ApprovalRequest
		win  time.Duration
	}{
		{
			name: "egress wait_for_review, 30s",
			ap: func(at time.Time) types.ApprovalRequest {
				return types.ApprovalRequest{
					Kind: types.ApprovalEgressDomain, State: types.ApprovalPending, RequestedAt: at,
					RequestedScope: mustScope(t, map[string]any{"mode": "wait_for_review"}),
				}
			},
			win: types.HoldWindowEgress,
		},
		{
			name: "ADO capability escalation, 240s",
			ap: func(at time.Time) types.ApprovalRequest {
				return types.ApprovalRequest{
					Kind: types.ApprovalToolCall, State: types.ApprovalPending, RequestedAt: at, GrantID: &grantID,
					RequestedScope: adoScope(t, grantID),
				}
			},
			win: types.HoldWindowADOCapability,
		},
		{
			name: "push_content, 600s",
			ap: func(at time.Time) types.ApprovalRequest {
				return types.ApprovalRequest{Kind: types.ApprovalPushContent, State: types.ApprovalPending, RequestedAt: at}
			},
			win: types.HoldWindowPush,
		},
	} {
		t.Run(w.name, func(t *testing.T) {
			justInside := now.Add(-w.win + time.Millisecond)
			if held, _ := Hold(w.ap(justInside), now); !held {
				t.Fatalf("held at window-1ms = false, want true")
			}
			atBoundary := now.Add(-w.win)
			if held, _ := Hold(w.ap(atBoundary), now); held {
				t.Fatalf("held exactly at the window = true, want false")
			}
		})
	}
}
