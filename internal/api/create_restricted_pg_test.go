// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_CreateRestrictedIsNeverOpen: a policy created with ?restricted=true is closed to a member from the
// moment it exists, a create that conflicts leaves no restriction behind, and the list written afterwards opens it
// to the person named. On the real store, with an admin and a member.
func TestPG_CreateRestrictedIsNeverOpen(t *testing.T) {
	e := newComponentsPG(t)
	const body = `{"name":"payments","spec":{"min_confinement_class":"CC2","first_use_approval":"always_deny"}}`
	listed := func(who string) map[string]bool {
		t.Helper()
		cookie := e.member
		if who == "admin" {
			cookie = e.admin
		}
		w := e.do(t, http.MethodGet, "/api/v1/policies", cookie, "")
		var rows []types.RunPolicy
		if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || w.Code != http.StatusOK {
			t.Fatalf("%s list = %d %s (%v)", who, w.Code, w.Body.String(), err)
		}
		out := map[string]bool{}
		for _, p := range rows {
			out[p.ID.String()] = true
		}
		return out
	}

	w := e.do(t, http.MethodPost, "/api/v1/policies?restricted=true", e.admin, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("restricted create = %d %s", w.Code, w.Body.String())
	}
	var created types.RunPolicy
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created.ID.String()

	w = e.do(t, http.MethodGet, "/api/v1/permissions/availability/policy/"+id, e.admin, "")
	var av availabilityView
	if err := json.Unmarshal(w.Body.Bytes(), &av); err != nil || w.Code != http.StatusOK || !av.Restricted || len(av.AllowedBy) != 0 {
		t.Fatalf("availability = %d %s (%v), want restricted with nobody listed", w.Code, w.Body.String(), err)
	}
	if listed("member")[id] || !listed("admin")[id] {
		t.Errorf("member sees it %v, admin sees it %v; want false and true", listed("member")[id], listed("admin")[id])
	}

	if w := e.do(t, http.MethodPost, "/api/v1/policies?restricted=true", e.admin, body); w.Code != http.StatusConflict {
		t.Fatalf("same name = %d %s, want 409", w.Code, w.Body.String())
	}
	rows, err := e.st.ListCapabilityRestrictions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows[capPolicy]) != 1 || !rows[capPolicy][id] {
		t.Errorf("policy restrictions = %v, want exactly the first policy's", rows[capPolicy])
	}

	e.grant(t, "user", "sub-member", capPolicy, id, "allow")
	if !listed("member")[id] {
		t.Error("the member is not offered the policy after the list names them")
	}
}
