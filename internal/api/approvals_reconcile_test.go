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

// rcRequireEgress marks a host REQUIRED in the workspace's requirements
// contract — the M1 condition the live deny-always path refuses on, reached by a
// requirements PUT, which deliberately does NOT stamp EgressEditedAt.
func rcRequireEgress(f *scopeFixture, wsID uuid.UUID, host string) {
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	ws := f.store.workspaces[wsID]
	ws.Requirements = map[string]types.WorkspaceRequirement{"egress:" + host: {Level: "required"}}
	f.store.workspaces[wsID] = ws
}

// TestReconcileSkipsAVerdictTheLivePathWouldRefuse pins the parity
// alwaysEgressDecision's doc comment used to CLAIM: the heal must not write
// something the live API answers 400 for.
//
// The heal replays a verdict recorded at t0 against the workspace as it is at
// BOOT, and the two diverge: mark `egress:<host>` required after an older
// deny-always on that host and every restart re-wrote the deny. Its only
// newer-action guard is EgressEditedAt, which the requirements PUT does not
// stamp — so nothing caught it, and the M1 guard exists because a deny on a
// required host makes "the workspace declare a need it can never satisfy" in
// every confined replay. Re-broken on each boot, with nothing saying why.
//
// Both halves are asserted on ONE run: the durable list is untouched AND the
// skip is audited. Auditing is not decoration here — the live twin audits even
// its give-up paths precisely so "how did this host get onto this workspace's
// list" has one answer, and a host that appeared only because a boot replayed a
// months-old verdict was the case that query could not answer.
func TestReconcileSkipsAVerdictTheLivePathWouldRefuse(t *testing.T) {
	const host = "registry.npmjs.org"
	f := newScopeFixture(t)
	wsID := f.seedWorkspace(t, nil, nil)
	rcRequireEgress(f, wsID, host)
	rcSeedDecided(f, host, types.ApprovalDenied, time.Now().UTC())

	before := len(f.audit())
	n, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 0 {
		t.Errorf("reconciled = %d, want 0 — the live decide path answers 400 for this exact write", n)
	}
	if _, denied := f.egressLists(t, wsID); len(denied) != 0 {
		t.Errorf("denied_egress = %v, want empty — the heal wrote a deny that contradicts the workspace's own "+
			"requirements contract, and would do it again on every boot", denied)
	}
	if got := len(f.audit()) - before; got == 0 {
		t.Error("the heal skipped silently — 'why is this host NOT on the list' needs an answer too")
	}
}

// TestReconcileAuditsTheWritesItMakes is the other half: the heal's DURABLE
// writes were recorded nowhere at all, while its live twin audits even the
// paths where it gives up. The source distinguishes the two, because "an
// operator clicked this" and "a restart replayed this" are different facts
// about the same row.
func TestReconcileAuditsTheWritesItMakes(t *testing.T) {
	const host = "registry.npmjs.org"
	f := newScopeFixture(t)
	wsID := f.seedWorkspace(t, nil, nil)
	rcSeedDecided(f, host, types.ApprovalApproved, time.Now().UTC())

	before := len(f.audit())
	if n, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context()); err != nil || n != 1 {
		t.Fatalf("reconcile = %d, %v; want 1, nil", n, err)
	}
	if approved, _ := f.egressLists(t, wsID); !slices.Contains(approved, host) {
		t.Fatalf("approved_egress = %v, want it to carry %q — the fixture stopped exercising a real write", approved, host)
	}
	var found bool
	for _, ev := range f.audit()[before:] {
		if ev.Action != "workspace.egress.approve" {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatalf("decode audit data: %v", err)
		}
		if d["source"] == "boot-heal" {
			found = true
		}
	}
	if !found {
		t.Errorf("no workspace.egress.approve row with source=boot-heal; the heal wrote to the workspace and left "+
			"no trace. events=%+v", f.audit()[before:])
	}
}
