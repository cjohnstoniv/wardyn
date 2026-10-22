// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// govServer is a control plane holding one governance document; it records
// every write it sees and fails the request whose "METHOD path-prefix" is in
// failOn.
type govServer struct {
	mu     sync.Mutex
	doc    client.GovernanceDocument
	writes []string
	bodies map[string]json.RawMessage
	failOn string
}

func (g *govServer) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		if g.failOn != "" && strings.HasPrefix(key, g.failOn) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "boom"})
			return
		}
		if r.Method != http.MethodGet {
			g.writes = append(g.writes, key)
			if r.Body != nil {
				var raw json.RawMessage
				_ = json.NewDecoder(r.Body).Decode(&raw)
				g.bodies[key] = raw
			}
		}
		switch {
		case key == "GET /api/v1/governance":
			writeJSON(w, http.StatusOK, g.doc)
		case key == "POST /api/v1/governance/units":
			writeJSON(w, http.StatusCreated, client.GovernanceUnitResponse{
				Unit: client.GovernanceUnit{ID: uuid.New(), Name: "fresh"}})
		case strings.HasPrefix(key, "PUT /api/v1/governance/units/"):
			id := uuid.MustParse(strings.TrimPrefix(key, "PUT /api/v1/governance/units/"))
			writeJSON(w, http.StatusOK, client.GovernanceUnitResponse{
				Unit: client.GovernanceUnit{ID: id, Name: "kept"}})
		case key == "POST /api/v1/governance/memberships":
			writeJSON(w, http.StatusOK, client.GovernanceMembership{ID: uuid.New()})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s", key)
		}
	}
}

func newGovServer(t *testing.T, doc client.GovernanceDocument) (*govServer, *client.Client) {
	t.Helper()
	g := &govServer{doc: doc, bodies: map[string]json.RawMessage{}}
	srv := httptest.NewServer(g.handler(t))
	t.Cleanup(srv.Close)
	return g, newTestClient(srv)
}

func TestApplyGovernance_ContactRidesTheWriteAndKeepsAnUnchangedProfileAWriteOnlyWhenItDiffers(t *testing.T) {
	id := uuid.New()
	stored := client.PolicyContact{Owner: "platform", Email: "p@example.com"}
	existing := client.GovernanceUnit{ID: id, Name: "kept", Contact: &stored}

	t.Run("nil contact leaves the stored one alone: no write", func(t *testing.T) {
		g, c := newGovServer(t, client.GovernanceDocument{Units: []client.GovernanceUnit{existing}})
		in := client.GovernanceUnit{Name: "kept"} // an older document that never sets Contact
		if _, err := c.ApplyGovernance(context.Background(), client.GovernanceDocument{Units: []client.GovernanceUnit{in}}, false); err != nil {
			t.Fatal(err)
		}
		if len(g.writes) != 0 {
			t.Fatalf("writes = %v, want none: a nil contact must not rewrite or wipe the stored one", g.writes)
		}
	})

	t.Run("an identical contact is a no-op", func(t *testing.T) {
		g, c := newGovServer(t, client.GovernanceDocument{Units: []client.GovernanceUnit{existing}})
		same := stored
		in := client.GovernanceUnit{Name: "kept", Contact: &same}
		if _, err := c.ApplyGovernance(context.Background(), client.GovernanceDocument{Units: []client.GovernanceUnit{in}}, false); err != nil {
			t.Fatal(err)
		}
		if len(g.writes) != 0 {
			t.Fatalf("writes = %v, want none", g.writes)
		}
	})

	t.Run("a different contact is PUT to the existing id with the contact in the body", func(t *testing.T) {
		g, c := newGovServer(t, client.GovernanceDocument{Units: []client.GovernanceUnit{existing}})
		changed := client.PolicyContact{Owner: "security"}
		in := client.GovernanceUnit{Name: "kept", Contact: &changed}
		if _, err := c.ApplyGovernance(context.Background(), client.GovernanceDocument{Units: []client.GovernanceUnit{in}}, false); err != nil {
			t.Fatal(err)
		}
		key := "PUT /api/v1/governance/units/" + id.String()
		body, ok := g.bodies[key]
		if !ok {
			t.Fatalf("writes = %v, want %s", g.writes, key)
		}
		var req struct {
			Contact *client.PolicyContact `json:"contact"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Contact == nil || req.Contact.Owner != "security" {
			t.Fatalf("body %s: contact = %+v, err %v; want owner security", body, req.Contact, err)
		}
	})
}

func TestApplyGovernance_FailuresNameTheStepAndStopThere(t *testing.T) {
	profID, asgID := uuid.New(), uuid.New()
	populated := client.GovernanceDocument{
		Units:       []client.GovernanceUnit{{ID: profID, Name: "stale"}},
		Memberships: []client.GovernanceMembership{{ID: asgID, SubjectType: "user", Subject: "old@example.com", UnitID: profID}},
	}
	incoming := func() client.GovernanceDocument {
		return client.GovernanceDocument{
			Units:       []client.GovernanceUnit{{Name: "fresh"}},
			Memberships: []client.GovernanceMembership{{SubjectType: "user", Subject: "new@example.com"}},
		}
	}
	cases := []struct {
		name, failOn, want string
		prune              bool
		forbid             []string // write prefixes that must never be issued: the apply stops at the failure
	}{
		{"read current state", "GET /api/v1/governance", "read current governance state", false,
			[]string{"POST", "PUT", "DELETE"}},
		{"profile write", "POST /api/v1/governance/units", `apply governance profile "fresh"`, false,
			[]string{"POST /api/v1/governance/memberships", "DELETE"}},
		{"assignment write", "POST /api/v1/governance/memberships", "apply governance assignment", true,
			[]string{"DELETE"}},
		{"assignment prune", "DELETE /api/v1/governance/memberships/", "prune governance assignment", true,
			[]string{"DELETE /api/v1/governance/units/"}},
		{"profile prune", "DELETE /api/v1/governance/units/", `prune governance profile "stale"`, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, c := newGovServer(t, populated)
			g.failOn = tc.failOn
			got, err := c.ApplyGovernance(context.Background(), incoming(), tc.prune)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if len(got.Units) != 0 || len(got.Memberships) != 0 {
				t.Fatalf("a failed apply returned a document: %+v", got)
			}
			for _, w := range g.writes {
				for _, f := range tc.forbid {
					if strings.HasPrefix(w, f) {
						t.Fatalf("writes = %v; %q was issued after the failure", g.writes, w)
					}
				}
			}
		})
	}
}

func TestApplyGovernance_PruneDeletesAssignmentsBeforeProfilesAndOnlyWhatTheDocOmits(t *testing.T) {
	keepID, dropID, asgKeep, asgDrop := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	g, c := newGovServer(t, client.GovernanceDocument{
		Units: []client.GovernanceUnit{{ID: keepID, Name: "kept"}, {ID: dropID, Name: "dropped"}},
		Memberships: []client.GovernanceMembership{
			{ID: asgKeep, SubjectType: "user", Subject: "a", UnitID: keepID},
			{ID: asgDrop, SubjectType: "user", Subject: "b", UnitID: dropID},
		},
	})
	doc := client.GovernanceDocument{
		Units:       []client.GovernanceUnit{{ID: keepID, Name: "kept"}},
		Memberships: []client.GovernanceMembership{{SubjectType: "user", Subject: "a", UnitID: keepID}},
	}
	if _, err := c.ApplyGovernance(context.Background(), doc, true); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"DELETE /api/v1/governance/memberships/" + asgDrop.String(),
		"DELETE /api/v1/governance/units/" + dropID.String(),
	}
	if strings.Join(g.writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes = %v, want exactly %v (assignments first, nothing the doc names)", g.writes, want)
	}
}

func TestApplyGovernanceResult_PartialFailureReportsStepsSoFar(t *testing.T) {
	g, c := newGovServer(t, client.GovernanceDocument{})
	g.failOn = "POST /api/v1/governance/memberships"
	doc := client.GovernanceDocument{
		Units:       []client.GovernanceUnit{{Name: "fresh"}},
		Memberships: []client.GovernanceMembership{{SubjectType: "user", Subject: "alice@example.com", UnitID: uuid.New()}},
	}
	res, err := c.ApplyGovernanceResult(context.Background(), doc, false)
	if err == nil {
		t.Fatal("want the assignment failure")
	}
	if len(res.Steps) != 2 {
		t.Fatalf("steps = %+v, want the profile create then the failed assignment", res.Steps)
	}
	create, asg := res.Steps[0], res.Steps[1]
	if create.Kind != "unit" || create.Op != "create" || create.Name != "fresh" || create.Outcome != "applied" {
		t.Fatalf("profile step = %+v, want an applied create of fresh", create)
	}
	if asg.Kind != "membership" || asg.Op != "upsert" || asg.Outcome != "failed" || asg.Error == "" {
		t.Fatalf("assignment step = %+v, want a failed upsert with its error", asg)
	}
}
