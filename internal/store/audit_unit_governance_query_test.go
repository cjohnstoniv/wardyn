// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Without the governance arm (an actor-filtered feed) the query is the one the unit feed had before it read
// governance rows, byte for byte.
func TestUnitAuditQueryRunRowsUnchanged(t *testing.T) {
	units := []uuid.UUID{uuid.New()}
	q, args := unitAuditQuery(units, AuditFilter{}, false)
	want := "\n\t\tSELECT " + auditCols + "\n\t\tFROM audit_events\n\t\tWHERE run_id IN (SELECT id FROM agent_runs WHERE unit_id = ANY($1))\n\t\tORDER BY seq DESC"
	if q != want || len(args) != 1 {
		t.Errorf("run-row query = %q with %d args, want %q with 1", q, len(args), want)
	}
}

// The feed's query reads the listed governance rows beside the runs' rows, and the filter's arguments
// follow the arm's.
func TestUnitAuditQueryFeedReadsGovernanceArm(t *testing.T) {
	units := []uuid.UUID{uuid.New(), uuid.New()}
	q, args := unitAuditQuery(units, AuditFilter{Action: "governance.unit.write"}, true)
	if len(args) != 4 {
		t.Fatalf("feed query has %d args, want units, actions, unit ids and the action filter", len(args))
	}
	if got, ok := args[1].([]string); !ok || !slices.Equal(got, unitGovernanceActions) {
		t.Errorf("$2 = %v, want the closed action list", args[1])
	}
	if got, ok := args[2].([]string); !ok || len(got) != 2 || got[0] != units[0].String() {
		t.Errorf("$3 = %v, want the unit ids as text", args[2])
	}
	for _, frag := range []string{"run_id IN (SELECT id FROM agent_runs WHERE unit_id = ANY($1))", "run_id IS NULL AND action = ANY($2)",
		"target = ANY($3)", "data->>'unit_id' = ANY($3)", "jsonb_typeof(data->'units') = 'array' AND data->'units' ?| $3", "$4",
		"action = ANY($2) AND NOT " + federatedRowSQL} {
		if !strings.Contains(q, frag) {
			t.Errorf("feed query lacks %q:\n%s", frag, q)
		}
	}
}

// The closed list leaves the change rows out, has no duplicates, and names only governance actions.
func TestUnitGovernanceActionsClosed(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range unitGovernanceActions {
		if !strings.HasPrefix(a, "governance.") || strings.HasPrefix(a, "governance.change.") {
			t.Errorf("%q does not belong on the list", a)
		}
		if seen[a] {
			t.Errorf("%q is listed twice", a)
		}
		seen[a] = true
	}
	for _, a := range []string{"governance.unit.write", "governance.envelope.set", "governance.elevation.expire", "governance.image_request.reject"} {
		if !seen[a] {
			t.Errorf("%q is not listed", a)
		}
	}
}
