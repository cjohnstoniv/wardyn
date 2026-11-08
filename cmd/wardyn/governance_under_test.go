// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// underGovernanceFake answers a unit admin of platform the way a deployment with an envelope does: GET
// /governance carries Default, platform below it, team below platform and other beside it; a unit write
// inside the envelope applies, and every admin grant and deciders write is held, the held change rendered
// from the request (target_kind, op, target_key and the proposed body) as the server renders it.
type underGovernanceFake struct {
	mu                         sync.Mutex
	def, platform, team, other sdk.GovernanceUnit
	memberships                []sdk.GovernanceMembership
	rules                      map[uuid.UUID][]sdk.DeciderRule
	writes                     []string
	held                       []sdk.GovernanceChange
	notes                      int
}

func newUnderGovernanceFake(t *testing.T) (*underGovernanceFake, *httptest.Server) {
	t.Helper()
	f := &underGovernanceFake{rules: map[uuid.UUID][]sdk.DeciderRule{}}
	f.def = sdk.GovernanceUnit{ID: uuid.New(), Name: "Default", Kind: "default"}
	f.platform = sdk.GovernanceUnit{ID: uuid.New(), Name: "platform", Kind: "unit", ParentID: &f.def.ID}
	f.team = sdk.GovernanceUnit{ID: uuid.New(), Name: "team", Kind: "unit", ParentID: &f.platform.ID}
	f.other = sdk.GovernanceUnit{ID: uuid.New(), Name: "other", Kind: "unit", ParentID: &f.def.ID}
	f.memberships = []sdk.GovernanceMembership{
		{ID: uuid.New(), SubjectType: "user", Subject: "ann@corp.com", UnitID: f.team.ID, Priority: 1},
		{ID: uuid.New(), SubjectType: "user", Subject: "olaf@corp.com", UnitID: f.other.ID, Priority: 1},
	}
	f.rules[f.team.ID] = []sdk.DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"unit_admins"}}}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *underGovernanceFake) units() []sdk.GovernanceUnit {
	return []sdk.GovernanceUnit{f.def, f.platform, f.team, f.other}
}

// label is s with every unit and membership id as its name, so a test reads which row a write touched.
func (f *underGovernanceFake) label(s string) string {
	for _, u := range f.units() {
		s = strings.ReplaceAll(s, u.ID.String(), "<"+u.Name+">")
	}
	for _, m := range f.memberships {
		s = strings.ReplaceAll(s, m.ID.String(), "<"+m.Subject+">")
	}
	return s
}

func (f *underGovernanceFake) hold(w http.ResponseWriter, kind, op, key string, body []byte) {
	ch := sdk.GovernanceChange{ID: uuid.New(), TargetKind: kind, Op: op, TargetKey: key, State: "pending", ProposedBy: "pat@corp.com",
		ProposedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(72 * time.Hour).UTC(), Diff: sdk.GovernanceChangeDiff{After: json.RawMessage(body)}}
	f.held = append(f.held, ch)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"pending_change": ch})
}

func (f *underGovernanceFake) read() map[string]any {
	var views []map[string]any
	for _, u := range f.units() {
		if rules, ok := f.rules[u.ID]; ok {
			views = append(views, map[string]any{"unit_id": u.ID, "rules": rules})
		}
	}
	return map[string]any{"units": f.units(), "memberships": f.memberships, "unit_admins": []sdk.GovernanceUnitAdmin{}, "deciders": views}
}

func (f *underGovernanceFake) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/governance" {
			_ = json.NewEncoder(w).Encode(f.read())
			return
		}
		body, _ := io.ReadAll(r.Body)
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/governance/")
		if strings.HasSuffix(path, "/note") {
			f.notes++
			_ = json.NewEncoder(w).Encode(sdk.GovernanceChange{ID: uuid.New(), State: "pending"})
			return
		}
		f.writes = append(f.writes, f.label(r.Method+" "+r.URL.Path))
		unitID := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSuffix(path, "/deciders"), "/admins"), "units/")
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/deciders"):
			f.hold(w, "unit_deciders", "update", unitID, body)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/admins"):
			f.hold(w, "unit_admin", "create", unitID, body)
		case r.Method == http.MethodPut && strings.HasPrefix(path, "units/"):
			var req sdk.GovernanceUnitRequest
			_ = json.Unmarshal(body, &req)
			u := f.team
			u.Name, u.Mode = req.Name, req.Mode
			_ = json.NewEncoder(w).Encode(sdk.GovernanceUnitResponse{Unit: u})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// getUnder runs `governance get --under platform` and returns the printed document and its file.
func getUnder(t *testing.T, url string) (sdk.GovernanceDocument, string) {
	t.Helper()
	stdout, stderr, err := runGovernance(t, url, "get", "--under", "platform")
	if err != nil {
		t.Fatalf("get --under: %v\n%s", err, stderr)
	}
	var doc sdk.GovernanceDocument
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("get --under printed no document: %v\n%s", err, stdout)
	}
	path := filepath.Join(t.TempDir(), "platform.json")
	if err := os.WriteFile(path, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	return doc, path
}

// TestGovernanceSetUnitAdminRoundTripNoop: a unit admin whose root's parent is outside their subtree runs
// `get --under > f; set --under f` and nothing is written, so no audit row is added.
func TestGovernanceSetUnitAdminRoundTripNoop(t *testing.T) {
	f, srv := newUnderGovernanceFake(t)
	doc, path := getUnder(t, srv.URL)
	if len(doc.Units) != 2 || doc.Deciders["team"] == nil {
		t.Fatalf("get --under = %+v, want platform and team with team's rules", doc)
	}
	if _, stderr, err := runGovernance(t, srv.URL, "set", path, "--under", "platform", "--prune"); err != nil {
		t.Fatalf("set --under: %v\n%s", err, stderr)
	}
	if len(f.writes) != 0 {
		t.Errorf("get → set under platform wrote %v, want nothing", f.writes)
	}
}

// TestGovernanceSetInsideEnvelopeAppliesAndHolds: inside the envelope a unit write applies, and a held
// deciders write prints pending, exits 0 and carries --reason, as the same write through the API does.
func TestGovernanceSetInsideEnvelopeAppliesAndHolds(t *testing.T) {
	f, srv := newUnderGovernanceFake(t)
	doc, _ := getUnder(t, srv.URL)
	for i := range doc.Units {
		if doc.Units[i].Name == "team" {
			doc.Units[i].Mode = "pass_through"
		}
	}
	doc.Deciders["team"] = []sdk.DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"unit_admins", "group:team-leads"}}}
	stdout, stderr, err := runGovernance(t, srv.URL, "set", writeGovernanceFile(t, doc), "--under", "platform", "--reason", "team leads decide egress")
	if err != nil {
		t.Fatalf("set exited non-zero on a held write: %v\n%s", err, stderr)
	}
	want := []string{"PUT /api/v1/governance/units/<team>", "PUT /api/v1/governance/units/<team>/deciders"}
	if !slices.Equal(f.writes, want) {
		t.Errorf("writes = %v, want %v", f.writes, want)
	}
	for _, line := range []string{"pending approval: 1 change(s)", "note written on 1 held change(s)"} {
		if !strings.Contains(stderr, line) {
			t.Errorf("stderr missing %q:\n%s", line, stderr)
		}
	}
	if f.notes != 1 || !json.Valid([]byte(stdout)) {
		t.Errorf("notes %d, stdout %q; want one note and a document", f.notes, stdout)
	}
}

// TestGovernanceSetUnderPruneScoped: set --under --prune of a document without team deletes team and its
// membership, and touches nothing outside platform's subtree.
func TestGovernanceSetUnderPruneScoped(t *testing.T) {
	f, srv := newUnderGovernanceFake(t)
	delete(f.rules, f.team.ID)
	doc, _ := getUnder(t, srv.URL)
	doc.Units = slices.DeleteFunc(doc.Units, func(u sdk.GovernanceUnit) bool { return u.Name == "team" })
	doc.Memberships = []sdk.GovernanceMembership{}
	if _, stderr, err := runGovernance(t, srv.URL, "set", writeGovernanceFile(t, doc), "--under", "platform", "--prune"); err != nil {
		t.Fatalf("set --under --prune: %v\n%s", err, stderr)
	}
	want := []string{"DELETE /api/v1/governance/memberships/<ann@corp.com>", "DELETE /api/v1/governance/units/<team>"}
	if !slices.Equal(f.writes, want) {
		t.Errorf("prune under platform wrote %v, want %v", f.writes, want)
	}
}

// TestGovernancePlanUnderMatchesAPIHeldChanges: plan --under lists the writes, and set --under holds the
// same changes (target_kind, op, target_key, diff) as the equivalent direct API calls.
func TestGovernancePlanUnderMatchesAPIHeldChanges(t *testing.T) {
	f, srv := newUnderGovernanceFake(t)
	doc, _ := getUnder(t, srv.URL)
	rules := []sdk.DeciderRule{{Kinds: []string{"egress_domain"}, Who: []string{"unit_admins", "group:team-leads"}}}
	doc.Deciders["team"] = rules
	doc.Admins = map[string][]sdk.GovernanceAdminSubject{"team": {{SubjectType: "group", Subject: "team-leads"}}}
	path := writeGovernanceFile(t, doc)

	stdout, stderr, err := runGovernance(t, srv.URL, "plan", path, "--under", "platform")
	if err != nil {
		t.Fatalf("plan --under: %v\n%s", err, stderr)
	}
	var plan sdk.GovernancePlan
	if err := json.Unmarshal([]byte(stdout), &plan); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, s := range plan.Steps {
		kinds = append(kinds, s.Kind+":"+s.Op)
	}
	if !slices.Equal(kinds, []string{"unit_admin:create", "deciders:update"}) || len(f.writes) != 0 {
		t.Fatalf("plan --under = %v (writes %v), want the grant and the deciders update and no write", kinds, f.writes)
	}

	if _, stderr, err := runGovernance(t, srv.URL, "set", path, "--under", "platform"); err != nil {
		t.Fatalf("set --under: %v\n%s", err, stderr)
	}
	viaSet := f.held
	f.held = nil
	c := sdk.New(srv.URL, "tok")
	if _, _, err := c.GrantUnitAdmin(context.Background(), f.team.ID, "group", "team-leads"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PutUnitDeciders(context.Background(), f.team.ID, rules); err != nil {
		t.Fatal(err)
	}
	type held struct {
		kind, op, key string
		diff          sdk.GovernanceChangeDiff
	}
	shape := func(chs []sdk.GovernanceChange) []held {
		out := make([]held, len(chs))
		for i, ch := range chs {
			out[i] = held{ch.TargetKind, ch.Op, ch.TargetKey, ch.Diff}
		}
		return out
	}
	if got, want := shape(viaSet), shape(f.held); len(got) != 2 || !reflect.DeepEqual(got, want) {
		t.Errorf("set --under held %+v, the API held %+v; want the same changes", got, want)
	}
}
