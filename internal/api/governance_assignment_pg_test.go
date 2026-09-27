// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_GovernanceAssignmentWritesAreAudited: an assignment changes a
// person's ceiling and autonomy, so every write that lands must leave its own
// audit row, and one that does not land (an unknown profile, refused by the
// foreign key) must leave neither a row nor an assignment. Against Postgres,
// because the 201/200 split and the 404 are the store's upsert and FK answers.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestPG_GovernanceAssignmentWritesAreAudited(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	profile, err := pg.UpsertGovernanceProfile(ctx, types.GovernanceProfile{
		Name:      "contractors",
		Ceiling:   types.RunPolicySpec{AllowedDomains: []string{"pypi.org"}, MinConfinementClass: types.CC2},
		CreatedBy: "seed",
	})
	if err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	h := newHarness(t)
	srv := New(baseTestConfig(h, pg))

	rows := func(action string) []types.AuditEvent {
		var out []types.AuditEvent
		for _, ev := range h.audit.snapshot() {
			if ev.Action == action {
				out = append(out, ev)
			}
		}
		return out
	}
	assign := func(profileID uuid.UUID, subject string, priority int) *httptest.ResponseRecorder {
		body, _ := json.Marshal(governanceAssignmentRequest{
			SubjectType: types.CapabilitySubjectUser, Subject: subject, ProfileID: profileID, Priority: priority,
		})
		return do(t, srv, http.MethodPost, "/api/v1/governance/assignments", adminToken, string(body))
	}

	w := assign(profile.ID, "alice@corp.example", 3)
	if w.Code != http.StatusCreated {
		t.Fatalf("first upsert: %d %s, want 201", w.Code, w.Body.String())
	}
	var saved types.GovernanceAssignment
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode: %v", err)
	}
	writes := rows("governance.assignment.write")
	if len(writes) != 1 {
		t.Fatalf("first upsert wrote %d governance.assignment.write rows, want 1", len(writes))
	}
	var data map[string]any
	if err := json.Unmarshal(writes[0].Data, &data); err != nil {
		t.Fatalf("decode audit data: %v", err)
	}
	if writes[0].Target != saved.ID.String() || writes[0].Outcome != "success" ||
		data["subject"] != "alice@corp.example" || data["subject_type"] != "user" ||
		data["profile_id"] != profile.ID.String() || data["priority"] != float64(3) {
		t.Fatalf("audit row = %+v data=%v; want target %s naming subject, profile_id and priority", writes[0], data, saved.ID)
	}

	// Same subject again: an update of the one row, not a second assignment.
	if w := assign(profile.ID, "alice@corp.example", 5); w.Code != http.StatusOK {
		t.Fatalf("re-upsert: %d %s, want 200", w.Code, w.Body.String())
	}
	if n := len(rows("governance.assignment.write")); n != 2 {
		t.Fatalf("re-upsert left %d write rows, want 2", n)
	}

	if w := assign(uuid.New(), "bob@corp.example", 1); w.Code != http.StatusNotFound {
		t.Fatalf("unknown profile: %d %s, want 404", w.Code, w.Body.String())
	}
	if n := len(rows("governance.assignment.write")); n != 2 {
		t.Fatalf("a refused upsert wrote an audit row (%d rows, want 2)", n)
	}
	all, err := pg.ListGovernanceAssignments(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("%d assignments stored, want only alice's", len(all))
	}

	del := "/api/v1/governance/assignments/" + saved.ID.String()
	if w := do(t, srv, http.MethodDelete, del, adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s, want 204", w.Code, w.Body.String())
	}
	deletes := rows("governance.assignment.delete")
	if len(deletes) != 1 || deletes[0].Target != saved.ID.String() || deletes[0].Outcome != "success" {
		t.Fatalf("delete rows = %+v, want one naming %s", deletes, saved.ID)
	}
	if w := do(t, srv, http.MethodDelete, del, adminToken, ""); w.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d, want 404", w.Code)
	}
	if n := len(rows("governance.assignment.delete")); n != 1 {
		t.Fatalf("a delete of nothing wrote an audit row (%d rows, want 1)", n)
	}
}
