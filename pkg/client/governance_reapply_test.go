// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// reapplyFake keeps units and records each write. A unit create named in failOnce answers 500 once; a
// create whose parent it does not hold answers 400; after the first read, each unit named in moveAfterRead
// is re-parented to the unit named beside it, as another admin's write would.
type reapplyFake struct {
	mu            sync.Mutex
	units         []client.GovernanceUnit
	failOnce      map[string]bool
	moveAfterRead map[string]string
	reads         int
	writes        []string // "METHOD unit-name", or "POST membership <unit-name>"
}

func (f *reapplyFake) named(name string) *client.GovernanceUnit {
	for i := range f.units {
		if f.units[i].Name == name {
			return &f.units[i]
		}
	}
	return nil
}

func (f *reapplyFake) nameOf(id uuid.UUID) string {
	for _, u := range f.units {
		if u.ID == id {
			return u.Name
		}
	}
	return "unknown " + id.String()
}

func (f *reapplyFake) client(t *testing.T) *client.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch path := strings.TrimPrefix(r.URL.Path, "/api/v1/governance"); {
		case r.Method == http.MethodGet && path == "":
			writeJSON(w, http.StatusOK, client.GovernanceDocument{Units: f.units, Memberships: []client.GovernanceMembership{}})
			if f.reads++; f.reads == 1 {
				for name, to := range f.moveAfterRead {
					id := f.named(to).ID
					f.named(name).ParentID = &id
				}
			}
		case r.Method == http.MethodPost && path == "/units":
			var req client.GovernanceUnitRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.writes = append(f.writes, "POST "+req.Name)
			if f.failOnce[req.Name] {
				delete(f.failOnce, req.Name)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "boom"})
				return
			}
			u := client.GovernanceUnit{ID: uuid.New(), Name: req.Name}
			if len(req.ParentID) > 0 && string(req.ParentID) != "null" {
				var parent uuid.UUID
				_ = json.Unmarshal(req.ParentID, &parent)
				if strings.HasPrefix(f.nameOf(parent), "unknown") {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "base does not exist"})
					return
				}
				u.ParentID = &parent
			}
			if len(req.Overlay) > 0 && string(req.Overlay) != "null" {
				u.Overlay = &types.CeilingOverlay{}
				_ = json.Unmarshal(req.Overlay, u.Overlay)
			}
			f.units = append(f.units, u)
			writeJSON(w, http.StatusCreated, client.GovernanceUnitResponse{Unit: u})
		case r.Method == http.MethodPost && path == "/memberships":
			var m client.GovernanceMembership
			_ = json.NewDecoder(r.Body).Decode(&m)
			f.writes = append(f.writes, "POST membership "+f.nameOf(m.UnitID))
			m.ID = uuid.New()
			writeJSON(w, http.StatusOK, m)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/units/"):
			id := uuid.MustParse(strings.TrimPrefix(path, "/units/"))
			f.writes = append(f.writes, "DELETE "+f.nameOf(id))
			f.units = slices.DeleteFunc(f.units, func(u client.GovernanceUnit) bool { return u.ID == id })
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return newTestClient(srv)
}

// reapplyDoc is a document from another install: a unit, a unit composed on it and a membership of the
// second, each naming the other by that install's ids.
func reapplyDoc() client.GovernanceDocument {
	rootID, teamID := uuid.New(), uuid.New()
	return client.GovernanceDocument{
		Units: []client.GovernanceUnit{{ID: rootID, Name: "root"}, {ID: teamID, Name: "team", ParentID: &rootID, Overlay: &types.CeilingOverlay{}}},
		Memberships: []client.GovernanceMembership{
			{SubjectType: client.CapabilitySubjectUser, Subject: "ana", UnitID: teamID, Priority: 1},
		},
	}
}

// The document an apply is given is never changed, so applying it again resolves its own ids the same way:
// after a success, and after a failure part way.
func TestApplyGovernanceLeavesTheDocumentAsGiven(t *testing.T) {
	ctx := context.Background()
	t.Run("after a success", func(t *testing.T) {
		f := &reapplyFake{}
		c := f.client(t)
		doc := reapplyDoc()
		before := client.GovernanceDocument{Units: slices.Clone(doc.Units), Memberships: slices.Clone(doc.Memberships)}
		for range 2 {
			if _, err := c.ApplyGovernanceResult(ctx, doc, false); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(doc.Units, before.Units) || !reflect.DeepEqual(doc.Memberships, before.Memberships) {
			t.Errorf("the apply changed the document: %+v", doc)
		}
		want := []string{"POST root", "POST team", "POST membership team", "POST membership team"}
		if !slices.Equal(f.writes, want) {
			t.Errorf("writes = %v, want %v", f.writes, want)
		}
	})
	t.Run("after a failure part way", func(t *testing.T) {
		f := &reapplyFake{failOnce: map[string]bool{"team": true}}
		c := f.client(t)
		doc := reapplyDoc()
		if _, err := c.ApplyGovernanceResult(ctx, doc, false); err == nil {
			t.Fatal("the first apply succeeded, want the team write to fail")
		}
		if _, err := c.ApplyGovernanceResult(ctx, doc, false); err != nil {
			t.Fatalf("the retry of the same document: %v", err)
		}
		want := []string{"POST root", "POST team", "POST team", "POST membership team"}
		if !slices.Equal(f.writes, want) {
			t.Errorf("writes = %v, want %v", f.writes, want)
		}
	})
}

// An --under prune reads the subtree again before it deletes: a unit moved out of it while the apply ran
// is not deleted, and the apply says so.
func TestApplyUnderPruneSparesAUnitMovedOut(t *testing.T) {
	root, other := uuid.New(), uuid.New()
	f := &reapplyFake{
		units: []client.GovernanceUnit{
			{ID: root, Name: "root"}, {ID: uuid.New(), Name: "team", ParentID: &root}, {ID: other, Name: "other"},
		},
		moveAfterRead: map[string]string{"team": "other"},
	}
	c := f.client(t)
	doc := client.GovernanceDocument{Units: []client.GovernanceUnit{{ID: root, Name: "root"}}}
	_, err := c.ApplyGovernanceResultUnder(context.Background(), doc, "root", true)
	if err == nil || !strings.Contains(err.Error(), `"team" moved out of "root"`) {
		t.Fatalf("apply = %v, want the moved unit named", err)
	}
	if slices.ContainsFunc(f.writes, func(w string) bool { return strings.HasPrefix(w, "DELETE") }) {
		t.Errorf("writes = %v, want no delete", f.writes)
	}
}

// Two entries with one name, or with one id, are refused before any write: either would be matched to
// whichever came last. Entries with no id are fine.
func TestApplyGovernanceRefusesARepeatedUnit(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		name  string
		units []client.GovernanceUnit
		want  string
	}{
		{"name", []client.GovernanceUnit{{Name: "team"}, {Name: " team "}}, `more than one governance profile named "team"`},
		{"id", []client.GovernanceUnit{{ID: id, Name: "a"}, {ID: id, Name: "b"}}, `"a" and "b" have the same id`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &reapplyFake{}
			c := f.client(t)
			doc := client.GovernanceDocument{Units: tc.units}
			if _, err := c.PlanGovernance(context.Background(), doc, false); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("plan = %v, want %q", err, tc.want)
			}
			if _, err := c.ApplyGovernanceResult(context.Background(), doc, false); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("apply = %v, want %q", err, tc.want)
			}
			if len(f.writes) != 0 {
				t.Errorf("writes = %v, want none", f.writes)
			}
		})
	}
	f := &reapplyFake{}
	if _, err := f.client(t).ApplyGovernanceResult(context.Background(),
		client.GovernanceDocument{Units: []client.GovernanceUnit{{Name: "a"}, {Name: "b"}}}, false); err != nil {
		t.Errorf("two new units with no id: %v", err)
	}
}
