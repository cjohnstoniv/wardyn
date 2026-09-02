// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rcSeedDecided seeds an already-DECIDED always-scoped egress approval on the
// fixture's run with an explicit DecidedAt — what a live decide() leaves behind,
// and the only field the boot heal can order two verdicts by.
func rcSeedDecided(f *scopeFixture, host string, state types.ApprovalState, decidedAt time.Time) {
	id := uuid.New()
	at := decidedAt
	f.approval.mu.Lock()
	f.approval.byID[id] = types.ApprovalRequest{
		ID: id, RunID: f.runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":` + strconv.Quote(host) + `}`),
		State:          state, DecisionScope: types.ScopeAlways, DecidedAt: &at,
		RequestedAt: decidedAt.Add(-time.Minute),
	}
	f.approval.mu.Unlock()
}

// TestReconcileAppliesOnlyTheNewestDecisionPerHost pins the fold, not merely the
// order: two `always` verdicts on the SAME host collapse to one write, and the
// one that survives is the one the human made LAST. The reconciled COUNT is
// asserted because ordering alone would still write both — the older verdict
// would land, and for a moment (and in the audit stream) the workspace would
// carry a decision the operator had already replaced.
func TestReconcileAppliesOnlyTheNewestDecisionPerHost(t *testing.T) {
	const host = "registry.npmjs.org"
	older := time.Now().UTC().Add(-2 * time.Hour)
	newer := older.Add(time.Hour)

	for _, tc := range []struct {
		name         string
		olderState   types.ApprovalState
		newerState   types.ApprovalState
		wantApproved bool
	}{
		{"a newer approve beats an older deny", types.ApprovalDenied, types.ApprovalApproved, true},
		{"a newer deny beats an older approve", types.ApprovalApproved, types.ApprovalDenied, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopeFixture(t)
			ws := f.seedWorkspace(t, nil, nil)
			rcSeedDecided(f, host, tc.olderState, older)
			rcSeedDecided(f, host, tc.newerState, newer)

			n, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context())
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if n != 1 {
				t.Errorf("reconciled %d decisions, want 1: two verdicts on one host are ONE write, the newest", n)
			}
			approved, denied := f.egressLists(t, ws)
			if slices.Contains(approved, host) != tc.wantApproved || slices.Contains(denied, host) == tc.wantApproved {
				t.Fatalf("approved=%v denied=%v, want %q only on the %s list", approved, denied, host,
					map[bool]string{true: "approved", false: "denied"}[tc.wantApproved])
			}
		})
	}
}

// TestReconcileSkipsDecisionsOlderThanTheOperatorsListEdit is the resurrection
// guard, in BOTH directions the two PUTs undo. The approval row reads
// APPROVED/always (or DENIED/always) forever, so without the EgressEditedAt
// stamp the heal put the operator's removal back at every restart: fail-OPEN for
// the allowlist, and an availability override for the denylist.
//
// Note the stamp is per WORKSPACE, not per list: an operator who restates one
// list suppresses re-applies of decisions older than that moment in both
// directions. That is the rule the undo needs — the operator's most recent word
// about this workspace's egress wins — and the pending-heal it can cost is a
// dropped write-back the operator has since had the lists in front of them for.
func TestReconcileSkipsDecisionsOlderThanTheOperatorsListEdit(t *testing.T) {
	const host = "registry.npmjs.org"
	decided := time.Now().UTC().Add(-time.Hour)

	t.Run("approved-egress PUT removes the host", func(t *testing.T) {
		f := newScopeFixture(t)
		ws := f.seedWorkspace(t, []string{host}, nil)
		rcSeedDecided(f, host, types.ApprovalApproved, decided)
		if _, err := f.store.SetWorkspaceApprovedEgress(t.Context(), ws, nil); err != nil {
			t.Fatalf("undo PUT: %v", err)
		}
		if n, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context()); err != nil || n != 0 {
			t.Fatalf("reconcile = %d/%v, want 0 decisions re-applied", n, err)
		}
		if approved, _ := f.egressLists(t, ws); slices.Contains(approved, host) {
			t.Fatalf("the removed host came back: approved=%v", approved)
		}
	})

	t.Run("denied-egress PUT removes the host", func(t *testing.T) {
		f := newScopeFixture(t)
		ws := f.seedWorkspace(t, nil, []string{host})
		rcSeedDecided(f, host, types.ApprovalDenied, decided)
		if _, err := f.store.SetWorkspaceDeniedEgress(t.Context(), ws, nil); err != nil {
			t.Fatalf("undo PUT: %v", err)
		}
		if n, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context()); err != nil || n != 0 {
			t.Fatalf("reconcile = %d/%v, want 0 decisions re-applied", n, err)
		}
		if _, denied := f.egressLists(t, ws); slices.Contains(denied, host) {
			t.Fatalf("the lifted deny came back: denied=%v", denied)
		}
	})
}

// TestReconcileStillHealsDecisionsNewerThanTheListEdit is the other half, and
// the one that keeps D28 closed: the stamp is a floor, not an off switch. A
// verdict made AFTER the operator last touched the lists is exactly the dropped
// write-back the heal exists for, and it must still be recreated.
func TestReconcileStillHealsDecisionsNewerThanTheListEdit(t *testing.T) {
	const host = "registry.npmjs.org"
	f := newScopeFixture(t)
	ws := f.seedWorkspace(t, nil, nil)
	// The operator edits the lists FIRST (stamping "now"), then decides.
	if _, err := f.store.SetWorkspaceApprovedEgress(t.Context(), ws, nil); err != nil {
		t.Fatalf("list edit: %v", err)
	}
	rcSeedDecided(f, host, types.ApprovalApproved, time.Now().UTC().Add(time.Minute))

	n, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciled %d decisions, want 1: a verdict newer than the list edit is still healed", n)
	}
	if approved, _ := f.egressLists(t, ws); !slices.Contains(approved, host) {
		t.Fatalf("the dropped write-back was not healed: approved=%v", approved)
	}
}
