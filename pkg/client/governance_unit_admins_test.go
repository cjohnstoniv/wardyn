// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// unitAdminServer serves one unit, "team", with the given locks and grants, and records each write
// with its body. holdGrants answers every grant 202 with a pending change.
type unitAdminServer struct {
	mu         sync.Mutex
	unit       client.GovernanceUnit
	grants     []client.GovernanceUnitAdmin
	holdGrants bool
	writes     []string
	bodies     map[string]json.RawMessage
}

func newUnitAdminServer(t *testing.T, locks []string, grants ...client.GovernanceUnitAdmin) (*unitAdminServer, *client.Client) {
	t.Helper()
	g := &unitAdminServer{
		unit:   client.GovernanceUnit{ID: uuid.New(), Name: "team", LockedKnobs: locks},
		grants: grants, bodies: map[string]json.RawMessage{},
	}
	for i := range g.grants {
		g.grants[i].UnitID = g.unit.ID
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		if r.Method != http.MethodGet {
			g.writes = append(g.writes, key)
			var raw json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&raw)
			g.bodies[key] = raw
		}
		units := "/api/v1/governance/units/" + g.unit.ID.String()
		switch {
		case key == "GET /api/v1/governance":
			writeJSON(w, http.StatusOK, map[string]any{
				"units": []client.GovernanceUnit{g.unit}, "memberships": []client.GovernanceMembership{}, "unit_admins": g.grants,
			})
		case key == "PUT "+units:
			writeJSON(w, http.StatusOK, client.GovernanceUnitResponse{Unit: g.unit})
		case key == "POST "+units+"/admins" && g.holdGrants:
			writeJSON(w, http.StatusAccepted, map[string]any{"pending_change": map[string]any{
				"id": uuid.NewString(), "target_kind": "governance_unit_admin", "op": "create", "state": "pending",
			}})
		case key == "POST "+units+"/admins":
			writeJSON(w, http.StatusCreated, client.GovernanceUnitAdmin{ID: uuid.New(), UnitID: g.unit.ID})
		case strings.HasPrefix(key, "DELETE "+units+"/admins/"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s", key)
		}
	}))
	t.Cleanup(srv.Close)
	return g, newTestClient(srv)
}

func alice() client.GovernanceUnitAdmin {
	return client.GovernanceUnitAdmin{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "alice@example.com"}
}

func eng() client.GovernanceAdminSubject {
	return client.GovernanceAdminSubject{SubjectType: types.CapabilitySubjectGroup, Subject: "eng"}
}

func TestGetGovernanceFoldsUnitAdminsByUnitName(t *testing.T) {
	a := alice()
	_, c := newUnitAdminServer(t, nil, a)
	doc, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []client.GovernanceAdminSubject{{SubjectType: types.CapabilitySubjectUser, Subject: "alice@example.com"}}
	if got := doc.Admins["team"]; !slices.Equal(got, want) {
		t.Errorf("admins[team] = %+v, want %+v", got, want)
	}
	if g := doc.UnitAdminGrants(); len(g) != 1 || g[0].ID != a.ID {
		t.Errorf("UnitAdminGrants = %+v, want alice's grant with its id", g)
	}
}

func TestApplyGovernanceGrantsTheAdminsTheServerLacks(t *testing.T) {
	g, c := newUnitAdminServer(t, nil, alice())
	doc, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	doc.Admins["team"] = append(doc.Admins["team"], eng())
	if _, err := c.ApplyGovernanceResult(context.Background(), doc, false); err != nil {
		t.Fatal(err)
	}
	post := "POST /api/v1/governance/units/" + g.unit.ID.String() + "/admins"
	if !slices.Equal(g.writes, []string{post}) {
		t.Fatalf("writes = %v, want only the grant of eng (alice is already an admin)", g.writes)
	}
	if body := string(g.bodies[post]); body != `{"subject_type":"group","subject":"eng"}` {
		t.Errorf("grant body = %s", body)
	}
}

func TestApplyGovernancePruneRevokesTheAdminsTheFileOmits(t *testing.T) {
	a := alice()
	g, c := newUnitAdminServer(t, nil, a)
	doc, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	doc.Admins = map[string][]client.GovernanceAdminSubject{"team": {eng()}}
	plan, err := c.PlanGovernance(context.Background(), doc, true)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, s := range plan.Steps {
		kinds = append(kinds, s.Kind+" "+s.Op+" "+s.Subject)
	}
	if !slices.Equal(kinds, []string{"unit_admin create eng", "unit_admin delete alice@example.com"}) || !plan.Destructive {
		t.Errorf("plan = %v (destructive %v), want the eng grant and alice's revoke", kinds, plan.Destructive)
	}
	if _, err := c.ApplyGovernanceResult(context.Background(), doc, true); err != nil {
		t.Fatal(err)
	}
	units := "/api/v1/governance/units/" + g.unit.ID.String()
	if want := []string{"POST " + units + "/admins", "DELETE " + units + "/admins/" + a.ID.String()}; !slices.Equal(g.writes, want) {
		t.Errorf("writes = %v, want %v", g.writes, want)
	}
}

func TestApplyGovernanceHeldGrantIsPendingAndSkipsPrune(t *testing.T) {
	g, c := newUnitAdminServer(t, nil, alice())
	g.holdGrants = true
	doc, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	doc.Admins = map[string][]client.GovernanceAdminSubject{"team": {eng()}}
	res, err := c.ApplyGovernanceResult(context.Background(), doc, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pending) != 1 || !res.PruneSkipped {
		t.Errorf("pending = %d, prune skipped = %v; want the held grant and no revoke after it", len(res.Pending), res.PruneSkipped)
	}
	if len(g.writes) != 1 {
		t.Errorf("writes = %v, want only the held grant", g.writes)
	}
}

func TestApplyGovernanceWritesLockedKnobs(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stored, doc []string
		wantBody    string // "" for no write
	}{
		{"a new lock is sent", nil, []string{"recording"}, `["recording"]`},
		{"a dropped lock is cleared", []string{"recording"}, nil, `[]`},
		{"the same locks in another order write nothing", []string{"allowed_domains", "recording"}, []string{"recording", "allowed_domains"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, c := newUnitAdminServer(t, tc.stored)
			doc, err := c.GetGovernance(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			doc.Units[0].LockedKnobs = tc.doc
			if _, err := c.ApplyGovernanceResult(context.Background(), doc, false); err != nil {
				t.Fatal(err)
			}
			put := "PUT /api/v1/governance/units/" + g.unit.ID.String()
			if tc.wantBody == "" {
				if len(g.writes) != 0 {
					t.Errorf("writes = %v, want none", g.writes)
				}
				return
			}
			var body struct {
				LockedKnobs json.RawMessage `json:"locked_knobs"`
			}
			if err := json.Unmarshal(g.bodies[put], &body); err != nil || string(body.LockedKnobs) != tc.wantBody {
				t.Errorf("PUT locked_knobs = %s (%v), want %s", body.LockedKnobs, err, tc.wantBody)
			}
		})
	}
}
