// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// sectionsFake is a control plane for the deciders section and --under: GET /governance carries its units,
// memberships, grants and deciders (each unit's own rules, plus units that only inherit, listed with none),
// and each PUT deciders is applied, held (202) or refused (422) per unit.
type sectionsFake struct {
	mu           sync.Mutex
	units        []client.GovernanceUnit
	memberships  []client.GovernanceMembership
	grants       []client.GovernanceUnitAdmin
	rules        map[uuid.UUID][]client.DeciderRule
	inheritOnly  []uuid.UUID
	restrictions []client.Restriction
	held         map[uuid.UUID]bool
	holdCreates  bool                    // POST /governance/units answers 202
	refuse       map[uuid.UUID][2]string // reason, message
	calls        []string                // every write: "METHOD path #body-hash", ids as unit names
	paths        []string                // every write: "METHOD path", ids as unit names
}

func newSectionsFake(t *testing.T) (*sectionsFake, *client.Client) {
	t.Helper()
	f := &sectionsFake{rules: map[uuid.UUID][]client.DeciderRule{}, held: map[uuid.UUID]bool{}, refuse: map[uuid.UUID][2]string{}}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	return f, newTestClient(srv)
}

// unit adds a unit under parent (nil for the top) and returns it.
func (f *sectionsFake) unit(name string, parent *client.GovernanceUnit) client.GovernanceUnit {
	u := client.GovernanceUnit{ID: uuid.New(), Name: name, Kind: "unit"}
	if parent != nil {
		pid := parent.ID
		u.ParentID = &pid
	}
	f.units = append(f.units, u)
	return u
}

func (f *sectionsFake) label(s string) string {
	for _, u := range f.units {
		s = strings.ReplaceAll(s, u.ID.String(), "<"+u.Name+">")
	}
	for _, m := range f.memberships {
		s = strings.ReplaceAll(s, m.ID.String(), "<"+m.Subject+">")
	}
	for _, g := range f.grants {
		s = strings.ReplaceAll(s, g.ID.String(), "<admin "+g.Subject+">")
	}
	return s
}

func (f *sectionsFake) read() map[string]any {
	var views []map[string]any
	for _, u := range f.units {
		if rules, ok := f.rules[u.ID]; ok {
			views = append(views, map[string]any{"unit_id": u.ID, "rules": rules})
		} else if slices.Contains(f.inheritOnly, u.ID) {
			views = append(views, map[string]any{"unit_id": u.ID, "rules": []client.DeciderRule{}, "from": map[string]any{"egress_domain": uuid.New()}})
		}
	}
	return map[string]any{"units": f.units, "memberships": f.memberships, "unit_admins": f.grants, "deciders": views, "restrictions": f.restrictions}
}

func (f *sectionsFake) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/governance" {
			writeJSON(w, http.StatusOK, f.read())
			return
		}
		body, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256([]byte(f.label(string(body))))
		f.paths = append(f.paths, f.label(r.Method+" "+r.URL.Path))
		f.calls = append(f.calls, f.label(r.Method+" "+r.URL.Path)+" #"+hex.EncodeToString(sum[:])[:12])
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/governance/")
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/deciders"):
			f.putDeciders(w, uuid.MustParse(strings.TrimSuffix(strings.TrimPrefix(path, "units/"), "/deciders")), body)
		case r.Method == http.MethodPost && path == "units" && f.holdCreates:
			writeJSON(w, http.StatusAccepted, pendingChange("create", "governance_unit", "fresh"))
		case r.Method == http.MethodPost && path == "units":
			var req client.GovernanceUnitRequest
			_ = json.Unmarshal(body, &req)
			u := client.GovernanceUnit{ID: uuid.New(), Name: req.Name, Kind: "unit"}
			f.units = append(f.units, u)
			writeJSON(w, http.StatusCreated, client.GovernanceUnitResponse{Unit: u})
		case r.Method == http.MethodPut && strings.HasPrefix(path, "units/"):
			id := uuid.MustParse(strings.TrimPrefix(path, "units/"))
			var req client.GovernanceUnitRequest
			_ = json.Unmarshal(body, &req)
			writeJSON(w, http.StatusOK, client.GovernanceUnitResponse{Unit: client.GovernanceUnit{ID: id, Name: req.Name, Kind: "unit"}})
		case r.Method == http.MethodPost && path == "memberships":
			var m client.GovernanceMembership
			_ = json.Unmarshal(body, &m)
			m.ID = uuid.New()
			writeJSON(w, http.StatusOK, m)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/admins"):
			writeJSON(w, http.StatusCreated, client.GovernanceUnitAdmin{ID: uuid.New()})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (f *sectionsFake) putDeciders(w http.ResponseWriter, id uuid.UUID, body []byte) {
	if refusal, ok := f.refuse[id]; ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": refusal[1], "reason": refusal[0]})
		return
	}
	if f.held[id] {
		writeJSON(w, http.StatusAccepted, pendingChange("update", "unit_deciders", id.String()))
		return
	}
	var req struct {
		Rules []client.DeciderRule `json:"rules"`
	}
	_ = json.Unmarshal(body, &req)
	f.rules[id] = req.Rules
	if len(req.Rules) == 0 {
		delete(f.rules, id)
	}
	writeJSON(w, http.StatusOK, client.UnitDeciders{UnitID: id, Rules: req.Rules})
}

func (f *sectionsFake) decidersWrites() []string {
	var out []string
	for _, p := range f.paths {
		if strings.HasSuffix(p, "/deciders") {
			out = append(out, p)
		}
	}
	return out
}

func egressRule(who ...string) client.DeciderRule {
	return client.DeciderRule{Kinds: []string{"egress_domain"}, Who: who}
}

func elevationRule() client.DeciderRule {
	return client.DeciderRule{Kinds: []string{"elevation"}, Who: []string{"unit_admins"}, MaxElevation: "4h",
		EligibleRequesters: []string{"group:support-leads"}, RenewAfter: "2h", TicketPattern: "INC-[0-9]+"}
}

// platformTree is Default with platform (and team below it) and other beside it, each with a membership,
// a grant on platform and on other, and rules on team and on other.
func platformTree(f *sectionsFake) (def, platform, team, other client.GovernanceUnit) {
	def = client.GovernanceUnit{ID: uuid.New(), Name: "Default", Kind: "default"}
	f.units = append(f.units, def)
	platform = f.unit("platform", &def)
	team = f.unit("team", &platform)
	other = f.unit("other", &def)
	f.memberships = []client.GovernanceMembership{
		{ID: uuid.New(), SubjectType: "user", Subject: "ann@corp.com", UnitID: team.ID, Priority: 1},
		{ID: uuid.New(), SubjectType: "user", Subject: "olaf@corp.com", UnitID: other.ID, Priority: 1},
	}
	f.grants = []client.GovernanceUnitAdmin{
		{ID: uuid.New(), UnitID: platform.ID, SubjectType: "user", Subject: "pat@corp.com"},
		{ID: uuid.New(), UnitID: other.ID, SubjectType: "user", Subject: "otto@corp.com"},
	}
	f.rules[team.ID] = []client.DeciderRule{egressRule("unit_admins")}
	f.rules[other.ID] = []client.DeciderRule{egressRule("owner")}
	return def, platform, team, other
}

func stepsOf(steps []client.ApplyStep, kind string) []client.ApplyStep {
	var out []client.ApplyStep
	for _, s := range steps {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

// TestApplyGovernanceResultUnchangedBytes: moving the apply's body behind the read sends the same
// requests, in the same order, with the same bodies, for a document that creates, updates, assigns,
// grants and prunes. The list was recorded before the move.
func TestApplyGovernanceResultUnchangedBytes(t *testing.T) {
	f, c := newSectionsFake(t)
	def := client.GovernanceUnit{ID: uuid.New(), Name: "Default", Kind: "default"}
	f.units = append(f.units, def)
	platform := f.unit("platform", &def)
	team := f.unit("team", &platform)
	old := f.unit("old", &platform)
	f.memberships = []client.GovernanceMembership{
		{ID: uuid.New(), SubjectType: "user", Subject: "ann@corp.com", UnitID: team.ID, Priority: 1},
		{ID: uuid.New(), SubjectType: "user", Subject: "bob@corp.com", UnitID: old.ID, Priority: 1},
	}
	f.grants = []client.GovernanceUnitAdmin{{ID: uuid.New(), UnitID: platform.ID, SubjectType: "user", Subject: "pat@corp.com"}}

	changed := team
	changed.Mode = "pass_through"
	fresh := client.GovernanceUnit{ID: uuid.New(), Name: "fresh", ParentID: &platform.ID}
	doc := client.GovernanceDocument{
		Units: []client.GovernanceUnit{def, platform, changed, fresh},
		Memberships: []client.GovernanceMembership{
			{SubjectType: "user", Subject: "ann@corp.com", UnitID: team.ID, Priority: 2},
			{SubjectType: "user", Subject: "dee@corp.com", UnitID: fresh.ID, Priority: 1},
		},
		Admins: map[string][]client.GovernanceAdminSubject{
			"platform": {{SubjectType: "user", Subject: "pat@corp.com"}},
			"team":     {{SubjectType: "group", Subject: "eng"}},
		},
	}
	if _, err := c.ApplyGovernanceResult(context.Background(), doc, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"PUT /api/v1/governance/units/<team> #87d2bbb62754", "POST /api/v1/governance/units #c904c6b1433d",
		"POST /api/v1/governance/memberships #efcad00664e0", "POST /api/v1/governance/memberships #5e61648f2af5",
		"POST /api/v1/governance/units/<team>/admins #af9aae2ff235", "DELETE /api/v1/governance/memberships/<bob@corp.com> #e3b0c44298fc",
		"DELETE /api/v1/governance/units/<old> #e3b0c44298fc"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("requests changed:\n got %#v\nwant %#v", f.calls, want)
	}
}

// TestGetGovernanceFoldsOwnDecidersOnly: the read's deciders become the document's section by unit name,
// own rules only: a unit that only inherits (empty rules) is absent, and with no rules there is no section.
func TestGetGovernanceFoldsOwnDecidersOnly(t *testing.T) {
	f, c := newSectionsFake(t)
	_, platform, team, other := platformTree(f)
	f.inheritOnly = []uuid.UUID{platform.ID}
	doc, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]client.DeciderRule{"team": f.rules[team.ID], "other": f.rules[other.ID]}
	if !reflect.DeepEqual(doc.Deciders, want) {
		t.Errorf("deciders = %#v, want %#v", doc.Deciders, want)
	}

	f.rules = map[uuid.UUID][]client.DeciderRule{}
	doc, err = c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(doc)
	if doc.Deciders != nil || strings.Contains(string(b), `"deciders"`) {
		t.Errorf("an install with no rules printed a deciders section: %s", b)
	}
}

// TestPlanDecidersSteps: a changed unit's rules are an update; under prune, a unit with rules the section
// omits is a delete. A unit whose rules are unchanged is no step.
func TestPlanDecidersSteps(t *testing.T) {
	f, c := newSectionsFake(t)
	_, platform, _, other := platformTree(f)
	current, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	doc := current
	doc.Deciders = map[string][]client.DeciderRule{
		"team":     current.Deciders["team"],
		"platform": {egressRule("group:platform-leads")},
	}
	plan, err := c.PlanGovernance(context.Background(), doc, false)
	if err != nil {
		t.Fatal(err)
	}
	got := stepsOf(plan.Steps, "deciders")
	if len(got) != 1 || got[0].Op != "update" || got[0].Name != "platform" || got[0].ID == nil || *got[0].ID != platform.ID ||
		!slices.Equal(got[0].Fields, []string{"rules"}) {
		t.Fatalf("deciders steps = %+v, want one update of platform's rules", got)
	}
	plan, err = c.PlanGovernance(context.Background(), doc, true)
	if err != nil {
		t.Fatal(err)
	}
	got = stepsOf(plan.Steps, "deciders")
	if len(got) != 2 || got[1].Op != "delete" || got[1].Name != "other" || *got[1].ID != other.ID || !plan.Destructive {
		t.Fatalf("pruned deciders steps = %+v, want platform's update then other's delete", got)
	}
}

// TestApplyUnchangedDecidersNoWrite: a get → set of a document with a deciders section writes nothing.
func TestApplyUnchangedDecidersNoWrite(t *testing.T) {
	f, c := newSectionsFake(t)
	platformTree(f)
	doc, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ApplyGovernanceResult(context.Background(), doc, true); err != nil {
		t.Fatal(err)
	}
	if len(f.paths) != 0 {
		t.Errorf("get → set wrote %v, want nothing", f.paths)
	}
}

// TestDecidersAbsentSectionNeverPruned: a document with no deciders key (written before the section
// existed) leaves every unit's rules alone, under --prune too.
func TestDecidersAbsentSectionNeverPruned(t *testing.T) {
	f, c := newSectionsFake(t)
	platformTree(f)
	doc, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	doc.Deciders = nil
	plan, err := c.PlanGovernance(context.Background(), doc, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := stepsOf(plan.Steps, "deciders"); len(got) != 0 {
		t.Errorf("plan of a document with no section = %+v, want no deciders step", got)
	}
	if _, err := c.ApplyGovernanceResult(context.Background(), doc, true); err != nil {
		t.Fatal(err)
	}
	if got := f.decidersWrites(); len(got) != 0 {
		t.Errorf("apply wrote %v, want no deciders write", got)
	}
}

// TestDecidersPresentEmptySectionPrunes: a present, empty section is authoritative: --prune clears the
// rules of every unit it omits, and without --prune nothing is cleared.
func TestDecidersPresentEmptySectionPrunes(t *testing.T) {
	f, c := newSectionsFake(t)
	platformTree(f)
	doc, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	doc.Deciders = map[string][]client.DeciderRule{}
	if _, err := c.ApplyGovernanceResult(context.Background(), doc, false); err != nil {
		t.Fatal(err)
	}
	if got := f.decidersWrites(); len(got) != 0 {
		t.Fatalf("apply without --prune wrote %v, want nothing", got)
	}
	res, err := c.ApplyGovernanceResult(context.Background(), doc, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PUT /api/v1/governance/units/<team>/deciders", "PUT /api/v1/governance/units/<other>/deciders"}
	if got := f.decidersWrites(); !slices.Equal(got, want) {
		t.Errorf("prune wrote %v, want %v", got, want)
	}
	if got := stepsOf(res.Steps, "deciders"); len(got) != 2 || got[0].Op != "delete" || got[0].Outcome != "applied" {
		t.Errorf("prune steps = %+v, want two applied deletes", got)
	}
}

// TestDecidersCanonicalCompare: the document's durations are compared in the server's canonical form, so
// "240m" against a stored "4h" is no step.
func TestDecidersCanonicalCompare(t *testing.T) {
	f, c := newSectionsFake(t)
	_, _, team, _ := platformTree(f)
	f.rules[team.ID] = []client.DeciderRule{elevationRule()}
	doc, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := elevationRule()
	r.MaxElevation, r.RenewAfter = "240m", "120m"
	doc.Deciders["team"] = []client.DeciderRule{r}
	plan, err := c.PlanGovernance(context.Background(), doc, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := stepsOf(plan.Steps, "deciders"); len(got) != 0 {
		t.Errorf("240m against 4h planned %+v, want no step", got)
	}
	r.MaxElevation = "25h"
	doc.Deciders["team"] = []client.DeciderRule{r}
	if _, err := c.ApplyGovernanceResult(context.Background(), doc, false); err == nil || !strings.Contains(err.Error(), "max_elevation") {
		t.Errorf("an invalid rule applied with %v, want the max_elevation error", err)
	}
	if len(f.paths) != 0 {
		t.Errorf("an invalid document wrote %v, want nothing", f.paths)
	}
}

// TestDecidersElevationFieldsRoundTrip: every rule field counts. A get → plan is no step, and a change to
// any one of eligible_requesters, renew_after or ticket_pattern is an update.
func TestDecidersElevationFieldsRoundTrip(t *testing.T) {
	f, c := newSectionsFake(t)
	_, _, team, _ := platformTree(f)
	f.rules[team.ID] = []client.DeciderRule{elevationRule()}
	doc, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := c.PlanGovernance(context.Background(), doc, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := stepsOf(plan.Steps, "deciders"); len(got) != 0 {
		t.Fatalf("get → plan = %+v, want no step", got)
	}
	for field, edit := range map[string]func(*client.DeciderRule){
		"eligible_requesters": func(r *client.DeciderRule) { r.EligibleRequesters = []string{"group:support-leads", "group:oncall"} },
		"renew_after":         func(r *client.DeciderRule) { r.RenewAfter = "3h" },
		"ticket_pattern":      func(r *client.DeciderRule) { r.TicketPattern = "CHG-[0-9]+" },
	} {
		r := elevationRule()
		edit(&r)
		doc.Deciders["team"] = []client.DeciderRule{r}
		plan, err := c.PlanGovernance(context.Background(), doc, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := stepsOf(plan.Steps, "deciders"); len(got) != 1 || got[0].Op != "update" || got[0].Name != "team" {
			t.Errorf("%s changed: steps = %+v, want team's update", field, got)
		}
	}
}

// widenFixture is a parent and its child, named so that name order puts the child first, both with one
// rule, and a document that widens both.
func widenFixture(f *sectionsFake) (parent, child client.GovernanceUnit, doc client.GovernanceDocument) {
	def := client.GovernanceUnit{ID: uuid.New(), Name: "Default", Kind: "default"}
	f.units = append(f.units, def)
	parent = f.unit("z-parent", &def)
	child = f.unit("a-child", &parent)
	f.rules[parent.ID] = []client.DeciderRule{egressRule("group:a")}
	f.rules[child.ID] = []client.DeciderRule{egressRule("group:a")}
	doc = client.GovernanceDocument{Units: []client.GovernanceUnit{def, parent, child}, Memberships: []client.GovernanceMembership{},
		Deciders: map[string][]client.DeciderRule{"a-child": {egressRule("group:a", "group:b")}, "z-parent": {egressRule("group:a", "group:b")}}}
	return parent, child, doc
}

// TestGovernancePlanWidensParentAndChild: a document that widens a parent and its child together lists and
// writes the parent first. With direct writes both apply. With the parent's write held, the child's 422
// (wider than its stored parent) is deferred behind the parent, not failed.
func TestGovernancePlanWidensParentAndChild(t *testing.T) {
	f, c := newSectionsFake(t)
	_, _, doc := widenFixture(f)
	plan, err := c.PlanGovernance(context.Background(), doc, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := stepsOf(plan.Steps, "deciders"); len(got) != 2 || got[0].Name != "z-parent" || got[1].Name != "a-child" {
		t.Fatalf("plan = %+v, want z-parent then a-child", got)
	}
	res, err := c.ApplyGovernanceResult(context.Background(), doc, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PUT /api/v1/governance/units/<z-parent>/deciders", "PUT /api/v1/governance/units/<a-child>/deciders"}
	if got := f.decidersWrites(); !slices.Equal(got, want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	if got := stepsOf(res.Steps, "deciders"); len(got) != 2 || got[0].Outcome != "applied" || got[1].Outcome != "applied" {
		t.Fatalf("direct steps = %+v, want both applied", got)
	}

	f2, c2 := newSectionsFake(t)
	parent, child, doc := widenFixture(f2)
	f2.held[parent.ID] = true
	f2.refuse[child.ID] = [2]string{"decider_rule_wider_than_parent", "rules[0]: who: group:b is wider than the rules above"}
	res, err = c2.ApplyGovernanceResult(context.Background(), doc, false)
	if err != nil {
		t.Fatalf("a child behind a held parent failed the apply: %v", err)
	}
	if len(res.Pending) != 1 || !slices.Contains(res.Deferred, client.GovernanceDeferredWrite{Kind: "deciders", Unit: "a-child", Base: "z-parent"}) {
		t.Fatalf("pending %d, deferred %+v; want the parent held and the child deferred behind it", len(res.Pending), res.Deferred)
	}
	got := stepsOf(res.Steps, "deciders")
	if len(got) != 2 || got[0].Outcome != "pending" || got[1].Outcome == "failed" ||
		!strings.Contains(got[1].Error, "waits for z-parent's held change; run set again after it is approved") {
		t.Errorf("held steps = %+v, want the parent pending and the child waiting on it", got)
	}
}

// TestApplyDecidersOther422IsUnitError: a 422 with no held step above the unit fails that unit's step with
// the server's message, which names the rule and the field.
func TestApplyDecidersOther422IsUnitError(t *testing.T) {
	for _, reason := range []string{"invalid_deciders", "decider_rule_wider_than_parent"} {
		f, c := newSectionsFake(t)
		_, child, doc := widenFixture(f)
		doc.Deciders = map[string][]client.DeciderRule{"a-child": {egressRule("group:a", "group:b")}}
		f.refuse[child.ID] = [2]string{reason, "rules[0]: who: group:b is wider than the rules above"}
		res, err := c.ApplyGovernanceResult(context.Background(), doc, false)
		var apiErr *client.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: err = %v, want the unit's 422", reason, err)
		}
		got := stepsOf(res.Steps, "deciders")
		if len(got) != 1 || got[0].Outcome != "failed" || !strings.Contains(got[0].Error, "rules[0]: who") || len(res.Deferred) != 0 {
			t.Errorf("%s: steps = %+v, deferred %+v; want a-child's failed step naming the field", reason, got, res.Deferred)
		}
	}
}

// TestDecidersUnknownUnitIsError: a section naming a unit neither the document nor the server has is an
// error, in the plan and before any write of an apply.
func TestDecidersUnknownUnitIsError(t *testing.T) {
	f, c := newSectionsFake(t)
	platformTree(f)
	doc, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	doc.Deciders["ghost"] = []client.DeciderRule{egressRule("owner")}
	const want = `deciders names unit "ghost", which neither the document nor the server has`
	if _, err := c.PlanGovernance(context.Background(), doc, false); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("plan err = %v, want %q", err, want)
	}
	doc.Units = append(doc.Units, client.GovernanceUnit{Name: "brand-new"})
	if _, err := c.ApplyGovernanceResult(context.Background(), doc, false); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("apply err = %v, want %q", err, want)
	}
	if len(f.paths) != 0 {
		t.Errorf("apply wrote %v before refusing, want nothing", f.paths)
	}
}

// TestGetUnderPrintsSubtree: get --under is the unit and the units below it, with their memberships,
// admins and rules, and no restrictions.
func TestGetUnderPrintsSubtree(t *testing.T) {
	f, c := newSectionsFake(t)
	_, platform, team, _ := platformTree(f)
	f.restrictions = []client.Restriction{{ID: uuid.New(), Name: "no-prod"}}
	for _, under := range []string{"platform", platform.ID.String()} {
		doc, err := c.GetGovernanceUnder(context.Background(), under)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, u := range doc.Units {
			names = append(names, u.Name)
		}
		if !slices.Equal(names, []string{"platform", "team"}) || len(doc.Memberships) != 1 || doc.Memberships[0].UnitID != team.ID ||
			len(doc.Admins) != 1 || doc.Admins["platform"] == nil || len(doc.Deciders) != 1 || doc.Deciders["team"] == nil || doc.Restrictions != nil {
			t.Errorf("--under %s = units %v, memberships %+v, admins %v, deciders %v, restrictions %v; want platform's subtree only",
				under, names, doc.Memberships, doc.Admins, doc.Deciders, doc.Restrictions)
		}
	}
	if _, err := c.GetGovernanceUnder(context.Background(), "nowhere"); err == nil || !strings.Contains(err.Error(), `no unit "nowhere" in the governance you can read`) {
		t.Errorf("unknown --under: err = %v", err)
	}
}

// TestPlanUnderFiltersSubtree: a plan under a unit, with prune, only ever names units of its subtree.
func TestPlanUnderFiltersSubtree(t *testing.T) {
	f, c := newSectionsFake(t)
	platformTree(f)
	doc, err := c.GetGovernanceUnder(context.Background(), "platform")
	if err != nil {
		t.Fatal(err)
	}
	doc.Memberships = []client.GovernanceMembership{}
	doc.Admins, doc.Deciders = nil, map[string][]client.DeciderRule{}
	plan, err := c.PlanGovernanceUnder(context.Background(), doc, "platform", true)
	if err != nil {
		t.Fatal(err)
	}
	var touched []string
	for _, s := range plan.Steps {
		touched = append(touched, s.Kind+":"+s.Op+":"+s.Name+s.Unit)
	}
	want := []string{"unit_admin:delete:platform", "deciders:delete:team", "membership:delete:team"}
	if !slices.Equal(touched, want) {
		t.Errorf("plan under platform = %v, want %v", touched, want)
	}
}

// TestPlanUnderRefusesOutsideEntries: every document entry outside the subtree is refused, all of them
// in one error, before any write.
func TestPlanUnderRefusesOutsideEntries(t *testing.T) {
	f, c := newSectionsFake(t)
	_, platform, _, other := platformTree(f)
	doc, err := c.GetGovernanceUnder(context.Background(), "platform")
	if err != nil {
		t.Fatal(err)
	}
	stray := client.GovernanceUnit{ID: uuid.New(), Name: "stray", ParentID: &other.ID}
	doc.Units = append(doc.Units, other, stray, client.GovernanceUnit{ID: uuid.New(), Name: "fresh", ParentID: &platform.ID})
	doc.Memberships = append(doc.Memberships, client.GovernanceMembership{SubjectType: "user", Subject: "olaf@corp.com", UnitID: other.ID})
	doc.Admins["other"] = []client.GovernanceAdminSubject{{SubjectType: "user", Subject: "otto@corp.com"}}
	doc.Deciders["other"] = []client.DeciderRule{egressRule("owner")}
	_, err = c.PlanGovernanceUnder(context.Background(), doc, platform.ID.String(), true)
	if err == nil {
		t.Fatal("a document reaching outside the subtree was planned")
	}
	for _, want := range []string{`unit "other"`, `unit "stray"`, `membership user "olaf@corp.com"`, `admins names unit "other"`, `deciders names unit "other"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), `"fresh"`) {
		t.Errorf("a new unit under the root was refused:\n%v", err)
	}
	if _, err := c.ApplyGovernanceResultUnder(context.Background(), doc, "platform", true); err == nil {
		t.Error("the apply took the same document")
	}
	if len(f.paths) != 0 {
		t.Errorf("a refused document wrote %v", f.paths)
	}
}

// TestPlanUnderRefusesRestrictions: restrictions are organisation-wide, so a document under a unit
// carries none.
func TestPlanUnderRefusesRestrictions(t *testing.T) {
	f, c := newSectionsFake(t)
	platformTree(f)
	doc, err := c.GetGovernanceUnder(context.Background(), "platform")
	if err != nil {
		t.Fatal(err)
	}
	doc.Restrictions = []client.Restriction{{Name: "no-prod"}}
	const want = "restrictions are organisation-wide; write them without --under"
	if _, err := c.PlanGovernanceUnder(context.Background(), doc, "platform", false); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
}

// TestPlanUnderRequiresRoot: the document must carry the unit --under names, so a prune can never
// delete it.
func TestPlanUnderRequiresRoot(t *testing.T) {
	f, c := newSectionsFake(t)
	platformTree(f)
	doc, err := c.GetGovernanceUnder(context.Background(), "platform")
	if err != nil {
		t.Fatal(err)
	}
	doc.Units = slices.DeleteFunc(doc.Units, func(u client.GovernanceUnit) bool { return u.Name == "platform" })
	delete(doc.Admins, "platform")
	const want = `the document must include "platform", the unit --under names`
	if _, err := c.PlanGovernanceUnder(context.Background(), doc, "platform", true); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
}

// TestPruneUnderNeverTouchesOutside: an apply under a unit with prune and a document of the root alone
// deletes what is below the root and nothing outside it.
func TestPruneUnderNeverTouchesOutside(t *testing.T) {
	f, c := newSectionsFake(t)
	_, platform, _, _ := platformTree(f)
	doc := client.GovernanceDocument{Units: []client.GovernanceUnit{platform}, Memberships: []client.GovernanceMembership{},
		Deciders: map[string][]client.DeciderRule{}}
	if _, err := c.ApplyGovernanceResultUnder(context.Background(), doc, "platform", true); err != nil {
		t.Fatal(err)
	}
	if len(f.paths) == 0 {
		t.Fatal("prune under platform wrote nothing; want team's rows deleted")
	}
	for _, p := range f.paths {
		if strings.Contains(p, "other") || strings.Contains(p, "olaf") || strings.Contains(p, "otto") || strings.Contains(p, "Default") ||
			strings.Contains(p, "<platform>") && !strings.Contains(p, "/admins/") {
			t.Errorf("prune under platform wrote %s", p)
		}
	}
}

// TestDecidersDeferralKind: a deciders write deferred because its unit's own write is held carries Kind
// "deciders" and no Base, so it never reads as an assignment or a composed profile.
func TestDecidersDeferralKind(t *testing.T) {
	f, c := newSectionsFake(t)
	_, platform, _, _ := platformTree(f)
	f.holdCreates = true
	doc, err := c.GetGovernance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	doc.Units = append(doc.Units, client.GovernanceUnit{ID: uuid.New(), Name: "fresh", ParentID: &platform.ID})
	doc.Deciders["fresh"] = []client.DeciderRule{egressRule("unit_admins")}
	res, err := c.ApplyGovernanceResult(context.Background(), doc, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Deferred, client.GovernanceDeferredWrite{Kind: "deciders", Unit: "fresh"}) {
		t.Errorf("deferred = %+v, want fresh's deciders write as Kind deciders", res.Deferred)
	}
	if got := f.decidersWrites(); len(got) != 0 {
		t.Errorf("a unit whose create is held had its rules written: %v", got)
	}
}

// TestApplyUnderDocumentIsSubtree: the document an apply under a unit returns is the read after it,
// narrowed as get --under prints it, so `set --under > f` can be set again under the same unit.
func TestApplyUnderDocumentIsSubtree(t *testing.T) {
	f, c := newSectionsFake(t)
	platformTree(f)
	doc, err := c.GetGovernanceUnder(context.Background(), "platform")
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.ApplyGovernanceResultUnder(context.Background(), doc, "platform", true)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, u := range res.Document.Units {
		names = append(names, u.Name)
	}
	if !slices.Equal(names, []string{"platform", "team"}) || res.Document.Deciders["other"] != nil || len(res.Document.Memberships) != 1 {
		t.Fatalf("document = units %v, deciders %v, memberships %+v; want platform's subtree", names, res.Document.Deciders, res.Document.Memberships)
	}
	if _, err := c.ApplyGovernanceResultUnder(context.Background(), res.Document, "platform", true); err != nil || len(f.paths) != 0 {
		t.Errorf("setting the returned document again: err %v, writes %v; want a no-op", err, f.paths)
	}
}

// TestGrandchildDeferralNamesHeldRoot: below a held parent and a deferred child, a grandchild refused as
// wider waits on the parent's held change, the one that is actually pending, not on the child.
func TestGrandchildDeferralNamesHeldRoot(t *testing.T) {
	f, c := newSectionsFake(t)
	parent, child, doc := widenFixture(f)
	grand := f.unit("m-grandchild", &child)
	f.rules[grand.ID] = []client.DeciderRule{egressRule("group:a")}
	doc.Units = append(doc.Units, grand)
	doc.Deciders["m-grandchild"] = []client.DeciderRule{egressRule("group:a", "group:b")}
	f.held[parent.ID] = true
	wider := [2]string{"decider_rule_wider_than_parent", "rules[0]: who: group:b is wider than the rules above"}
	f.refuse[child.ID], f.refuse[grand.ID] = wider, wider
	res, err := c.ApplyGovernanceResult(context.Background(), doc, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Deferred, client.GovernanceDeferredWrite{Kind: "deciders", Unit: "m-grandchild", Base: "z-parent"}) {
		t.Errorf("deferred = %+v, want the grandchild waiting on z-parent", res.Deferred)
	}
	got := stepsOf(res.Steps, "deciders")
	if len(got) != 3 || got[2].Name != "m-grandchild" || !strings.Contains(got[2].Error, "waits for z-parent's held change") {
		t.Errorf("steps = %+v, want the grandchild's step naming z-parent", got)
	}
}
