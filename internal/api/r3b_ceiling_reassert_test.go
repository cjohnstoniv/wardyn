// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// r3bWalledRun dispatches one run under the ceiling the caller describes and
// returns the audit trail. It is deliberately NOT runWalledDispatch: that
// harness builds an assigned-profile ceiling only `if len(d.deny) > 0`, so the
// shape this pin is about — an ASSIGNED profile carrying no denies at all —
// cannot be expressed through it, and it lives in a file this lane must not
// edit. Everything else is the same wiring: dispatchRun driven directly with a
// resolved dispatchCeiling, which is how every lane delivers one.
func r3bWalledRun(t *testing.T, profile string, deny []string) ([]types.AuditEvent, uuid.UUID) {
	t.Helper()
	fr := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	srv.cfg.Store = ceilingDispatchStore{dispatchTestStore: st}
	run.Task = "" // no agent exec / completion watcher: this is about composition

	gc := governanceCeiling{Spec: types.RunPolicySpec{DeniedDomains: deny}}
	if profile != "" {
		gc.Profile = &types.GovernanceProfile{Name: profile}
	}
	srv.dispatchRun(context.Background(), run, ceilingForDispatch(gc), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
	})
	return audit.events, run.ID
}

// TestR3BCeilingReassertAuditsEveryAssignedProfile is F175's pin.
//
// reassertCeilingDenies documents itself as "ALWAYS audited when a profile
// applies, even with nothing to drop", and docs/AUDIT-ACTIONS.md says the same
// out loud: emitted on every walled run "including one with nothing to drop",
// so the row's ABSENCE means "no profile applies" rather than "this door
// skipped the ceiling". The function opened with `if len(c.deny) == 0 { return }`,
// which made that false for the commonest assigned shape there is — a profile
// that grants rather than denies. Nothing else in the dispatch says which
// ceiling the run stood inside: run.policy.effective records a policy, not
// whose walls they are.
func TestR3BCeilingReassertAuditsEveryAssignedProfile(t *testing.T) {
	t.Run("assigned profile with EMPTY denied_domains still records the row", func(t *testing.T) {
		events, runID := r3bWalledRun(t, "grants-only", nil)
		ev := findAudit(events, runID, "run.ceiling.reassert", "success")
		if ev == nil {
			t.Fatalf("no run.ceiling.reassert row for a run dispatched under an ASSIGNED profile with no "+
				"denies — its absence is supposed to mean \"no profile applies\"; events=%s", auditDump(events, runID))
		}
		var data struct {
			Profile string `json:"profile"`
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatalf("row data is not an object: %v (%s)", err, ev.Data)
		}
		// Naming the profile is the whole point: it is the one fact the
		// run.policy.effective envelope cannot carry.
		if data.Profile != "grants-only" {
			t.Errorf("row names profile %q, want %q", data.Profile, "grants-only")
		}
	})

	t.Run("assigned profile WITH denies still records the row", func(t *testing.T) {
		events, runID := r3bWalledRun(t, "walled", []string{"*.corp.example"})
		if findAudit(events, runID, "run.ceiling.reassert", "success") == nil {
			t.Fatalf("no run.ceiling.reassert row for a walled run; events=%s", auditDump(events, runID))
		}
	})

	// The control, and it is what stops this pin from being satisfied by
	// emitting the row unconditionally: with NO assigned profile there is no
	// ceiling to name, and the row must stay absent so its absence keeps
	// meaning something.
	t.Run("no assigned profile records NO row", func(t *testing.T) {
		events, runID := r3bWalledRun(t, "", nil)
		if ev := findAudit(events, runID, "run.ceiling.reassert", "success"); ev != nil {
			t.Errorf("run.ceiling.reassert recorded for an UNASSIGNED principal (%s) — the row's absence is "+
				"how an operator tells \"no profile applies\" from \"this door skipped the ceiling\"", ev.Data)
		}
	})
}
