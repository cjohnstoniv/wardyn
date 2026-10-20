// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_DirectDeletesRecordWhatTheyRemoved: a direct delete of an assignment, a grant or a profile
// widens someone, so its audit row names the subject, capability and profile, not only a row id.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestPG_DirectDeletesRecordWhatTheyRemoved(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(throwawayPGPool(t))
	h := newHarness(t)
	srv := New(baseTestConfig(h, pg))

	loose, err := pg.UpsertGovernanceProfile(ctx, types.GovernanceProfile{Name: "loose", CreatedBy: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	asg, err := pg.UpsertGovernanceAssignment(ctx, types.GovernanceAssignment{
		SubjectType: types.CapabilitySubjectUser, Subject: "alice@corp.example", ProfileID: loose.ID, Priority: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := pg.UpsertCapabilityGrant(ctx, types.CapabilityGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: "bob@corp.example", Capability: capAgent, Value: govAgentVal,
		Effect: types.CapabilityDeny, CreatedBy: "seed",
	})
	if err != nil {
		t.Fatal(err)
	}

	delAudit := func(path, action string) map[string]any {
		t.Helper()
		if w := do(t, srv, http.MethodDelete, path, adminToken, ""); w.Code != http.StatusNoContent {
			t.Fatalf("DELETE %s: %d %s, want 204", path, w.Code, w.Body.String())
		}
		for _, ev := range h.audit.snapshot() {
			if ev.Action == action {
				var data map[string]any
				if err := json.Unmarshal(ev.Data, &data); err != nil {
					t.Fatalf("%s data %q: %v", action, ev.Data, err)
				}
				return data
			}
		}
		t.Fatalf("no %s row", action)
		return nil
	}

	got := delAudit("/api/v1/governance/assignments/"+asg.ID.String(), "governance.assignment.delete")
	if got["subject_type"] != "user" || got["subject"] != "alice@corp.example" || got["profile_id"] != loose.ID.String() || got["priority"] != float64(2) {
		t.Errorf("assignment delete data = %v, want the subject, profile and priority it removed", got)
	}
	got = delAudit("/api/v1/permissions/grants/"+grant.ID.String(), "capability.grant.delete")
	if got["subject"] != "bob@corp.example" || got["capability"] != capAgent || got["value"] != govAgentVal || got["effect"] != "deny" {
		t.Errorf("grant delete data = %v, want the subject, capability, value and effect it removed", got)
	}
	got = delAudit("/api/v1/governance/profiles/"+loose.ID.String(), "governance.profile.delete")
	if got["name"] != "loose" {
		t.Errorf("profile delete data = %v, want the profile name", got)
	}
}

// flipBeforeDelete is another admin's upsert landing on the grant's natural key just before
// the direct delete runs: the row removed is then the flipped one.
type flipBeforeDelete struct{ store.PG }

func (f flipBeforeDelete) DeleteCapabilityGrant(ctx context.Context, id uuid.UUID) (types.CapabilityGrant, error) {
	cur, err := store.GetCapabilityGrantQ(ctx, f.Pool, id, false)
	if err != nil {
		return types.CapabilityGrant{}, err
	}
	cur.Effect = types.CapabilityAllow
	if _, err := f.UpsertCapabilityGrant(ctx, cur); err != nil {
		return types.CapabilityGrant{}, err
	}
	return f.PG.DeleteCapabilityGrant(ctx, id)
}

// TestPG_DirectGrantDeleteAuditsTheRowItRemoved: a write between a read and the delete must not
// leave the audit naming an effect the removed row no longer had.
func TestPG_DirectGrantDeleteAuditsTheRowItRemoved(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(throwawayPGPool(t))
	h := newHarness(t)
	srv := New(baseTestConfig(h, flipBeforeDelete{pg}))
	grant, err := pg.UpsertCapabilityGrant(ctx, types.CapabilityGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: "carol@corp.example", Capability: capAgent, Value: govAgentVal,
		Effect: types.CapabilityDeny, CreatedBy: "seed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := do(t, srv, http.MethodDelete, "/api/v1/permissions/grants/"+grant.ID.String(), adminToken, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s, want 204", w.Code, w.Body.String())
	}
	for _, ev := range h.audit.snapshot() {
		if ev.Action == "capability.grant.delete" {
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil || data["effect"] != "allow" {
				t.Errorf("grant delete data = %s (%v), want the removed row's effect allow", ev.Data, err)
			}
			return
		}
	}
	t.Fatal("no capability.grant.delete row")
}
