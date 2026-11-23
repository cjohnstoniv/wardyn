// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wallsTopoStore serves the topology's reads from memory: runs in ListRunsFiltered's order, decision rows in seq
// order (AuditWallsRows' filter and cursor), canned counts, the first allow rows per run, envelopes and the site
// config. Methods the topology never calls are the nil embedded Store's.
type wallsTopoStore struct {
	store.Store
	runs       []types.AgentRun
	rows       []store.DecisionRow
	counts     []store.WallsCount
	countErr   error
	firsts     map[uuid.UUID][]store.DecisionRow
	firstErrAt int // the AuditWallsFirstAllows call (1-based) that fails; 0 for none
	firstCalls [][]uuid.UUID
	firstSince [][]time.Time
	envs       map[uuid.UUID]store.RunPolicyEnvelope
	site       types.SiteConfig
	reads      int // AuditWallsRows calls
	runReads   int // ListRunsFiltered calls
}

// add appends rows, numbering their seq in order.
func (s *wallsTopoStore) add(rows ...store.DecisionRow) {
	for _, r := range rows {
		r.Seq = int64(len(s.rows) + 1)
		s.rows = append(s.rows, r)
	}
}

func (s *wallsTopoStore) ListRunsFiltered(_ context.Context, _ store.RunFilter, p store.Page) ([]types.AgentRun, error) {
	s.runReads++
	lo := min(p.Offset, len(s.runs))
	return slices.Clone(s.runs[lo:min(lo+p.Limit, len(s.runs))]), nil
}

func (s *wallsTopoStore) CountHiddenRuns(context.Context, store.RunFilter) (int, int, error) {
	return 0, 0, nil
}

func (s *wallsTopoStore) AuditWallsRows(_ context.Context, q store.DecisionQuery) ([]store.DecisionRow, store.DecisionCursor, error) {
	s.reads++
	var out []store.DecisionRow
	next := q.After
	for _, r := range s.rows {
		action := r.Action
		if action == "egress.pending" {
			action = "egress.hold"
		}
		if r.Seq <= q.After.Seq || r.RecordedAt.Before(q.Since) || !slices.Contains(q.Actions, action) ||
			(len(q.RunIDs) > 0 && !slices.Contains(q.RunIDs, r.RunID)) {
			continue
		}
		if len(out) == q.Limit {
			break
		}
		out = append(out, r)
		next = store.DecisionCursor{Seq: r.Seq}
	}
	return out, next, nil
}

func (s *wallsTopoStore) AuditWallsCounts(context.Context, []string, time.Time, []uuid.UUID, int) ([]store.WallsCount, error) {
	return s.counts, s.countErr
}

func (s *wallsTopoStore) AuditWallsFirstAllows(_ context.Context, runIDs []uuid.UUID, since []time.Time, perRun int) ([]store.DecisionRow, error) {
	s.firstCalls = append(s.firstCalls, slices.Clone(runIDs))
	s.firstSince = append(s.firstSince, slices.Clone(since))
	if len(s.firstCalls) == s.firstErrAt {
		return nil, errors.New("read refused")
	}
	var out []store.DecisionRow
	for _, id := range runIDs {
		rows := s.firsts[id]
		out = append(out, rows[:min(len(rows), perRun)]...)
	}
	return out, nil
}

func (s *wallsTopoStore) RunPolicyEnvelopes(_ context.Context, ids []uuid.UUID, _ []string) (map[uuid.UUID]store.RunPolicyEnvelope, error) {
	out := map[uuid.UUID]store.RunPolicyEnvelope{}
	for _, id := range ids {
		if env, ok := s.envs[id]; ok {
			out[id] = env
		}
	}
	return out, nil
}

func (s *wallsTopoStore) GetSiteConfig(context.Context) (types.SiteConfig, error) { return s.site, nil }

func (s *wallsTopoStore) ListGovernanceUnits(context.Context) ([]types.GovernanceUnit, error) {
	return nil, nil
}

func (s *wallsTopoStore) ListGovernanceMemberships(context.Context) ([]types.GovernanceMembership, error) {
	return nil, nil
}

// wallsTopoUnitStore adds a unit admin's scope reads: the grants ListUnitAdminOf answers, and a unit tree read
// that fails with treeErr or else reads no unit. The embedded unit audit reader is never called.
type wallsTopoUnitStore struct {
	*wallsTopoStore
	store.UnitAuditReader
	store.UnitAdminStore
	grants  []types.GovernanceUnitAdmin
	treeErr error
}

func (s *wallsTopoUnitStore) ListUnitAdminOf(context.Context, []string, []string) ([]types.GovernanceUnitAdmin, error) {
	return s.grants, nil
}

func (s *wallsTopoUnitStore) DryRunGovernance(context.Context, func(store.Querier) error) error {
	return s.treeErr
}

// wallsTopoClock is a fixed clock a test moves by hand.
type wallsTopoClock struct{ now time.Time }

func (c *wallsTopoClock) Now() time.Time { return c.now }

// wallsTopoServer is a server on st whose clock is clk.
func wallsTopoServer(st store.Store, clk *wallsTopoClock) *Server {
	return &Server{cfg: Config{Store: st, Now: clk.Now}}
}

// wallsTopoNewClock starts at a fixed instant with no date in it.
func wallsTopoNewClock() *wallsTopoClock {
	return &wallsTopoClock{now: time.Unix(0, 0).UTC().Add(48 * time.Hour)}
}

// wallsTopoGet sends GET /api/v1/walls/topology?query to the handler as a signed-in sub with role.
func wallsTopoGet(s *Server, sub, role, query string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/walls/topology?"+query, nil)
	r = r.WithContext(withOIDCRole(withOIDCHuman(r.Context(), sub), role))
	w := httptest.NewRecorder()
	s.handleWallsTopology(w, r)
	return w
}

// wallsTopoBody decodes a 200 answer.
func wallsTopoBody(t *testing.T, w *httptest.ResponseRecorder) WallsTopology {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body WallsTopology
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return body
}

// wallsTopoKeys decodes a 200 answer as JSON objects.
func wallsTopoKeys(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func wallsTopoRun(unit *uuid.UUID, state types.RunState, created time.Time) types.AgentRun {
	return types.AgentRun{ID: uuid.New(), UnitID: unit, State: state, CreatedAt: created, CreatedBy: "sub-walls-owner"}
}

func wallsTopoRow(run uuid.UUID, action, source, host string, weight int64, at time.Time) store.DecisionRow {
	return store.DecisionRow{ID: uuid.New(), RunID: run, Action: action, RuleSource: source, Host: host, Port: 443, Weight: weight,
		Time: at, DecidedAt: at, RecordedAt: at, Format: 1}
}

// The fold: a security caller's counts are weighted, keep a late row in late only and keep the control and
// configuration classes, while their hosts never reach hosts or edges; a unit admin's counts are one per row
// and leave those rows out entirely. Rows of a run outside the snapshot count nowhere.
func TestWallsTopologyFold(t *testing.T) {
	at := wallsTopoNewClock().now
	unit := uuid.New()
	a := wallsTopoRun(&unit, types.RunRunning, at)
	b := wallsTopoRun(nil, types.RunCompleted, at)
	late := wallsTopoRow(b.ID, "egress.allow", "policy:allowlist", "pypi.org", 2, at)
	late.DecidedAt = at.Add(-50 * time.Second)
	guest := wallsTopoRow(a.ID, "egress.deny", "policy:denied", "evil.example", 1, at)
	guest.Flow = "flow-1"
	rows := []store.DecisionRow{
		wallsTopoRow(a.ID, "egress.allow", "policy:allowlist", "pypi.org", 3, at),
		guest,
		wallsTopoRow(b.ID, "egress.hold", "approval:pending", "pypi.org", 1, at),
		wallsTopoRow(a.ID, "egress.allow", "brokered:mint", "cp.wardyn.example", 4, at),
		wallsTopoRow(a.ID, "egress.allow", "builtin:upstream-proxy", "proxy.corp.example", 1, at),
		late,
		wallsTopoRow(uuid.New(), "egress.allow", "policy:allowlist", "stray.example", 9, at),
	}
	for _, security := range []bool{true, false} {
		f := wallsTopoNewFold(security, []types.AgentRun{a, b})
		for _, r := range rows {
			f.add(r)
		}
		hosts, served := f.hosts()
		var names []string
		for _, h := range hosts {
			names = append(names, h.Host)
		}
		if !slices.Equal(names, []string{"pypi.org", "evil.example"}) {
			t.Errorf("security %v: hosts %v, want pypi.org then evil.example", security, names)
		}
		gotA, gotB := f.unit[unit].wire(security), f.unit[store.DefaultUnitID].wire(security)
		var wantA, wantB any
		if security {
			wantA = &WallsCounters{Allowed: 3, Refused: 1, Guest: 1, Control: 4, Config: 1}
			wantB = &WallsCounters{Held: 1, Late: 2}
		} else {
			wantA = &WallsUnitCounters{Allowed: 1, Refused: 1, Guest: 1}
			wantB = &WallsUnitCounters{Allowed: 1, Held: 1}
		}
		if fmt.Sprint(gotA) != fmt.Sprint(wantA) || fmt.Sprint(gotB) != fmt.Sprint(wantB) {
			t.Errorf("security %v: unit counters %+v and %+v, want %+v and %+v", security, gotA, gotB, wantA, wantB)
		}
		if n := len(f.unitHostEdges(served)); n != 3 {
			t.Errorf("security %v: %d unit_host edges, want 3", security, n)
		}
		if edges := f.runHostEdges(served); len(edges[a.ID]) != 2 || len(edges[b.ID]) != 1 {
			t.Errorf("security %v: run_host edges %d and %d, want 2 and 1", security, len(edges[a.ID]), len(edges[b.ID]))
		}
		runs := f.unitRuns()
		if u := runs[unit]; u.Live != 1 || u.Held != 0 || u.ByState[types.RunRunning] != 1 {
			t.Errorf("security %v: unit runs %+v, want one live running run", security, u)
		}
		if u := runs[store.DefaultUnitID]; u.Live != 0 || u.Held != 1 || u.ByState[types.RunCompleted] != 1 {
			t.Errorf("security %v: Default unit runs %+v, want one held completed run", security, u)
		}
	}
}

// The hosts cap: past wallsTopoMaxHosts the quietest destinations go, and no edge lands on one of them; a run
// keeps its wallsTopoRunHostEdges busiest edges.
func TestWallsTopologyHostsCap(t *testing.T) {
	at := wallsTopoNewClock().now
	run := wallsTopoRun(nil, types.RunRunning, at)
	f := wallsTopoNewFold(true, []types.AgentRun{run})
	for i := range wallsTopoMaxHosts + 1 {
		f.add(wallsTopoRow(run.ID, "egress.allow", "policy:allowlist", fmt.Sprintf("h%03d.example", i), int64(wallsTopoMaxHosts+2-i), at))
	}
	hosts, served := f.hosts()
	if len(hosts) != wallsTopoMaxHosts || hosts[0].Host != "h000.example" || served[wallsTopoHostKey{host: "h500.example", port: 443}] {
		t.Fatalf("%d hosts, first %s; want %d, the busiest first and the quietest dropped", len(hosts), hosts[0].Host, wallsTopoMaxHosts)
	}
	for _, e := range f.unitHostEdges(served) {
		if e.Host == "h500.example" {
			t.Error("a unit_host edge lands on a host the answer does not serve")
		}
	}
	if edges := f.runHostEdges(served)[run.ID]; len(edges) != wallsTopoRunHostEdges || edges[0].Host != "h000.example" {
		t.Errorf("run_host edges %d, want the busiest %d", len(edges), wallsTopoRunHostEdges)
	}
}

// The "<5" rule: elsewhere is the out-of-scope totals of the count read, served as counts when 5 or more runs
// are elsewhere and as "<5" in every field below that. A store that cannot count, or a failed count, leaves
// elsewhere out.
func TestWallsTopologyElsewhereRule(t *testing.T) {
	clk := wallsTopoNewClock()
	st := &wallsTopoStore{counts: []store.WallsCount{
		{Action: "", InScope: false, Runs: 7, Live: 3},
		{Action: "egress.allow", InScope: false, Rows: 12},
		{Action: "egress.deny", InScope: false, Rows: 4},
		{Action: "egress.hold", InScope: false, Rows: 2, Runs: 1},
		{Action: "egress.allow", InScope: true, Rows: 900},
		{Action: "", InScope: true, Runs: 40, Live: 40},
	}}
	s := wallsTopoServer(st, clk)
	sc := wallsTopoScope{units: []uuid.UUID{uuid.New()}}
	got := s.wallsTopoElsewhere(context.Background(), sc, clk.now, s.wallsTopoCaps())
	if got == nil || *got != (WallsElsewhere{Runs: 7, Live: 3, Held: 1, NotAllowed: 4, Allowed: 12}) {
		t.Fatalf("elsewhere = %+v", got)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"runs":7,"live":3,"held":1,"not_allowed":4,"allowed":12}`; string(raw) != want {
		t.Errorf("elsewhere JSON %s, want %s", raw, want)
	}
	st.counts[0].Runs = 4
	few := s.wallsTopoElsewhere(context.Background(), sc, clk.now, s.wallsTopoCaps())
	// Compared as decoded JSON: encoding/json writes "<" as \u003c, the same string to every reader.
	fewRaw, err := json.Marshal(few)
	var fewFields map[string]any
	if err == nil {
		err = json.Unmarshal(fewRaw, &fewFields)
	}
	lt5 := map[string]any{"runs": "<5", "live": "<5", "held": "<5", "not_allowed": "<5", "allowed": "<5"}
	if err != nil || !reflect.DeepEqual(fewFields, lt5) {
		t.Errorf("four runs elsewhere: %s, %v; want every field \"<5\"", fewRaw, err)
	}
	st.countErr = errors.New("count refused")
	if got := s.wallsTopoElsewhere(context.Background(), sc, clk.now, s.wallsTopoCaps()); got != nil {
		t.Errorf("a failed count = %+v, want none", got)
	}
	if got := wallsTopoServer(&capStore{}, clk).wallsTopoElsewhere(context.Background(), sc, clk.now, wallsDefaultCaps()); got != nil {
		t.Errorf("a store that cannot count = %+v, want none", got)
	}
}

// wallsTopoOpen is a security caller's first page over n runs with limit, and the cursor of its next page.
func wallsTopoOpen(t *testing.T, s *Server, sub string, limit int) string {
	t.Helper()
	body := wallsTopoBody(t, wallsTopoGet(s, sub, oidc.RoleSecurityAdmin, fmt.Sprintf("limit=%d", limit)))
	if body.Runs.NextCursor == "" {
		t.Fatal("the first page has no next cursor")
	}
	return body.Runs.NextCursor
}

// wallsTopoRunsStore is a store of n live runs in the Default unit, created a second apart.
func wallsTopoRunsStore(clk *wallsTopoClock, n int) *wallsTopoStore {
	st := &wallsTopoStore{}
	for i := range n {
		st.runs = append(st.runs, wallsTopoRun(nil, types.RunRunning, clk.now.Add(-time.Duration(i)*time.Second)))
	}
	return st
}

// An expired cursor, another caller's, one opened at a scope the caller no longer has, and one never opened all
// get the same 409 bytes; a cursor this route never wrote is a 400.
func TestWallsTopologyCursorGone(t *testing.T) {
	clk := wallsTopoNewClock()
	st := wallsTopoRunsStore(clk, 3)
	s := wallsTopoServer(st, clk)
	cursor := wallsTopoOpen(t, s, "sub-walls-a", 1)
	if w := wallsTopoGet(s, "sub-walls-a", oidc.RoleSecurityAdmin, "limit=1&cursor="+cursor); w.Code != http.StatusOK {
		t.Fatalf("the owner's continuation: %d %s", w.Code, w.Body.String())
	}
	var never wallsTopoCursor
	never.id[0] = 1
	gone := map[string]*httptest.ResponseRecorder{
		"another caller's": wallsTopoGet(s, "sub-walls-b", oidc.RoleSecurityAdmin, "cursor="+cursor),
		"a narrower scope": wallsTopoGet(s, "sub-walls-a", oidc.RoleUser, "cursor="+cursor),
		"one never opened": wallsTopoGet(s, "sub-walls-a", oidc.RoleSecurityAdmin, "cursor="+never.encode()),
		"past its offset":  wallsTopoGet(s, "sub-walls-a", oidc.RoleSecurityAdmin, "cursor="+wallsTopoCursor{id: wallsTopoMustDecode(t, cursor).id, offset: 9}.encode()),
	}
	clk.now = clk.now.Add(wallsTopoSnapshotTTL)
	want := wallsTopoGet(s, "sub-walls-a", oidc.RoleSecurityAdmin, "cursor="+cursor)
	if want.Code != http.StatusConflict || errorReason(want) != string(reasonWallsSnapshotGone) {
		t.Fatalf("an expired cursor: %d %s, want 409 %s", want.Code, want.Body.String(), reasonWallsSnapshotGone)
	}
	for name, w := range gone {
		if w.Code != want.Code || w.Body.String() != want.Body.String() {
			t.Errorf("%s cursor: %d %q, want the expired cursor's bytes %q", name, w.Code, w.Body.String(), want.Body.String())
		}
	}
	if w := wallsTopoGet(s, "sub-walls-a", oidc.RoleSecurityAdmin, "cursor=not-a-cursor"); w.Code != http.StatusBadRequest {
		t.Errorf("a malformed cursor: %d, want 400", w.Code)
	}
}

func wallsTopoMustDecode(t *testing.T, raw string) wallsTopoCursor {
	t.Helper()
	c, ok := wallsTopoDecodeCursor(raw)
	if !ok {
		t.Fatalf("cursor %q does not decode", raw)
	}
	return c
}

// partial at the cap: a read bound that stops the row read before the window's rows run out sets partial; the
// same rows under the default bounds do not.
func TestWallsTopologyPartialAtTheReadCap(t *testing.T) {
	clk := wallsTopoNewClock()
	st := wallsTopoRunsStore(clk, 1)
	for range 3 {
		st.add(wallsTopoRow(st.runs[0].ID, "egress.allow", "policy:allowlist", "pypi.org", 1, clk.now))
	}
	s := wallsTopoServer(st, clk)
	if body := wallsTopoBody(t, wallsTopoGet(s, "sub-walls-a", oidc.RoleSecurityAdmin, "")); body.Partial {
		t.Error("the default bounds read every row and still set partial")
	}
	s.wallsCaps = wallsCaps{readLimit: 1, reads: 2}
	if body := wallsTopoBody(t, wallsTopoGet(s, "sub-walls-b", oidc.RoleSecurityAdmin, "")); !body.Partial {
		t.Error("two reads of one row over three rows left partial unset")
	}
}

// TestWallsTopologyNeverServesAControlPlaneHost: a run's calls to Wardyn's own control plane (control and tool
// rule rows) and the upstream start rows count, but their hosts are in no hosts entry and on no edge, for
// either projection and either edge form.
func TestWallsTopologyNeverServesAControlPlaneHost(t *testing.T) {
	clk := wallsTopoNewClock()
	st := wallsTopoRunsStore(clk, 1)
	run := st.runs[0].ID
	hidden := []string{"cp.wardyn.example", "tools.wardyn.example", "proxy.corp.example"}
	st.add(wallsTopoRow(run, "egress.allow", "brokered:mint", hidden[0], 1, clk.now),
		wallsTopoRow(run, "egress.allow", "policy:tool-allow", hidden[1], 1, clk.now),
		wallsTopoRow(run, "egress.allow", "builtin:upstream-proxy", hidden[2], 1, clk.now),
		wallsTopoRow(run, "egress.allow", "policy:allowlist", "pypi.org", 1, clk.now))
	s := wallsTopoServer(st, clk)
	for i, query := range []string{"edges=summary", "edges=full"} {
		w := wallsTopoGet(s, fmt.Sprintf("sub-walls-%d", i), oidc.RoleSecurityAdmin, query)
		body := wallsTopoBody(t, w)
		if len(body.Hosts) != 1 || body.Hosts[0].Host != "pypi.org" {
			t.Errorf("%s: hosts %+v, want pypi.org only", query, body.Hosts)
		}
		for _, h := range hidden {
			if strings.Contains(w.Body.String(), h) {
				t.Errorf("%s: the answer serves %s", query, h)
			}
		}
	}
	f := wallsTopoNewFold(false, st.runs)
	for _, r := range st.rows {
		f.add(r)
	}
	hosts, served := f.hosts()
	raw, err := json.Marshal([]any{hosts, f.unitHostEdges(served), f.runHostEdges(served)})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hidden {
		if strings.Contains(string(raw), h) {
			t.Errorf("a unit admin's hosts or edges serve %s", h)
		}
	}
}

// TestWallsTopologyNilScopeServesNothing: each way unitAuditScopeFor answers nil (a store without the unit audit
// reader, no grant, a scope read that fails, no unit in the caller's subtrees) serves a unit admin who reached
// the handler zero runs, hosts and edges and no elsewhere, and reads no decision row and no run; and a run whose
// unit is outside the scope is not wallsUnitRunScope's.
func TestWallsTopologyNilScopeServesNothing(t *testing.T) {
	clk := wallsTopoNewClock()
	grant := []types.GovernanceUnitAdmin{{UnitID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: "sub-walls-unit"}}
	newBase := func() *wallsTopoStore {
		st := wallsTopoRunsStore(clk, 2)
		st.add(wallsTopoRow(st.runs[0].ID, "egress.allow", "policy:allowlist", "pypi.org", 1, clk.now))
		st.counts = []store.WallsCount{{Action: "", Runs: 9, Live: 9}}
		return st
	}
	cases := []struct {
		name  string
		store func(*wallsTopoStore) store.Store
	}{
		{"no unit audit reader", func(b *wallsTopoStore) store.Store { return b }},
		{"no grant", func(b *wallsTopoStore) store.Store { return &wallsTopoUnitStore{wallsTopoStore: b} }},
		{"a scope read fails", func(b *wallsTopoStore) store.Store {
			return &wallsTopoUnitStore{wallsTopoStore: b, grants: grant, treeErr: errors.New("tree read refused")}
		}},
		{"no unit in the subtrees", func(b *wallsTopoStore) store.Store { return &wallsTopoUnitStore{wallsTopoStore: b, grants: grant} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := newBase()
			s := wallsTopoServer(c.store(base), clk)
			w := wallsTopoGet(s, "sub-walls-unit", oidc.RoleUser, "edges=full")
			body, keys := wallsTopoBody(t, w), wallsTopoKeys(t, w)
			if body.Scope != wallsTopoScopeUnit || len(body.Runs.Items) != 0 || len(body.Hosts) != 0 || len(body.Edges.UnitHost)+len(body.Edges.RunHost) != 0 {
				t.Errorf("served %s", w.Body.String())
			}
			if _, ok := keys["elsewhere"]; ok {
				t.Error("an empty scope is served elsewhere")
			}
			if base.reads != 0 || base.runReads != 0 {
				t.Errorf("%d row reads and %d run reads, want none", base.reads, base.runReads)
			}
		})
	}
	t.Run("a run outside the scope", func(t *testing.T) {
		s := wallsTopoServer(&wallsTopoUnitStore{wallsTopoStore: newBase(), grants: grant}, clk)
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r = r.WithContext(withOIDCRole(withOIDCHuman(r.Context(), "sub-walls-unit"), oidc.RoleUser))
		unit := grant[0].UnitID
		if s.wallsUnitRunScope(r, types.AgentRun{ID: uuid.New(), UnitID: &unit}) {
			t.Error("a run in a unit the caller's subtrees do not hold is in scope")
		}
	})
}

// TestWallsTopologyRunCap: with caps.runs at 3, four runs serve 3 and partial; a unit admin's runs with no unit
// are dropped before the cap counts, so three kept runs behind two such runs serve 3 and partial stays unset.
func TestWallsTopologyRunCap(t *testing.T) {
	clk := wallsTopoNewClock()
	st := wallsTopoRunsStore(clk, 4)
	s := wallsTopoServer(st, clk)
	s.wallsCaps = wallsCaps{runs: 3}
	if body := wallsTopoBody(t, wallsTopoGet(s, "sub-walls-a", oidc.RoleSecurityAdmin, "")); len(body.Runs.Items) != 3 || !body.Partial {
		t.Errorf("four runs under a cap of 3: %d served, partial %v; want 3 and partial", len(body.Runs.Items), body.Partial)
	}
	def := store.DefaultUnitID
	unitRuns := &wallsTopoStore{runs: []types.AgentRun{
		wallsTopoRun(nil, types.RunRunning, clk.now), wallsTopoRun(nil, types.RunRunning, clk.now),
		wallsTopoRun(&def, types.RunRunning, clk.now), wallsTopoRun(&def, types.RunRunning, clk.now), wallsTopoRun(&def, types.RunRunning, clk.now),
	}}
	sc := wallsTopoScope{units: []uuid.UUID{def}, in: map[uuid.UUID]bool{def: true}}
	runs, partial, err := s.wallsTopoRuns(context.Background(), unitRuns, sc, wallsTopoDefaultWindow, 3)
	if err != nil || len(runs) != 3 || partial {
		t.Errorf("a Default-unit admin: %d runs, partial %v, %v; want 3 and not partial", len(runs), partial, err)
	}
	for _, run := range runs {
		if run.UnitID == nil {
			t.Error("a run with no unit was kept for a unit admin")
		}
	}
}

// TestWallsTopologyLimiters: a 4th first page in a burst is refused; a snapshot's 5 pages all load; the 11th
// continuation in a burst is refused while a first page keeps its own budget; and a continuation naming another
// caller's live snapshot gets the expired cursor's 409 bytes, charged to the asker.
func TestWallsTopologyLimiters(t *testing.T) {
	clk := wallsTopoNewClock()
	st := wallsTopoRunsStore(clk, 50)
	s := wallsTopoServer(st, clk)
	s.wallsCaps = wallsCaps{runs: 50}
	limited := func(w *httptest.ResponseRecorder) bool {
		return w.Code == http.StatusTooManyRequests && errorReason(w) == string(reasonWallsRateLimited)
	}

	for i := range 4 {
		w := wallsTopoGet(s, "sub-walls-burst", oidc.RoleSecurityAdmin, "limit=10")
		if got := limited(w); got != (i == 3) {
			t.Fatalf("first page %d: %d %s", i+1, w.Code, w.Body.String())
		}
	}

	cursor := wallsTopoOpen(t, s, "sub-walls-pages", 10)
	for page := 2; cursor != ""; page++ {
		body := wallsTopoBody(t, wallsTopoGet(s, "sub-walls-pages", oidc.RoleSecurityAdmin, "limit=10&cursor="+cursor))
		if len(body.Runs.Items) != 10 {
			t.Fatalf("page %d has %d runs", page, len(body.Runs.Items))
		}
		if cursor = body.Runs.NextCursor; cursor == "" && page != 5 {
			t.Fatalf("the snapshot ended at page %d, want 5", page)
		}
	}

	cursor = wallsTopoOpen(t, s, "sub-walls-cont", 10)
	for i := range 11 {
		w := wallsTopoGet(s, "sub-walls-cont", oidc.RoleSecurityAdmin, "limit=10&cursor="+cursor)
		if got := limited(w); got != (i == 10) {
			t.Fatalf("continuation %d: %d %s", i+1, w.Code, w.Body.String())
		}
	}
	if w := wallsTopoGet(s, "sub-walls-cont", oidc.RoleSecurityAdmin, "limit=10"); w.Code != http.StatusOK {
		t.Errorf("a first page after the continuation budget ran out: %d %s", w.Code, w.Body.String())
	}

	cursor = wallsTopoOpen(t, s, "sub-walls-owner", 10)
	var never wallsTopoCursor
	gone := wallsTopoGet(s, "sub-walls-asker", oidc.RoleSecurityAdmin, "cursor="+never.encode())
	for i := range 10 {
		w := wallsTopoGet(s, "sub-walls-asker", oidc.RoleSecurityAdmin, "cursor="+cursor)
		if i < 9 && (w.Code != http.StatusConflict || w.Body.String() != gone.Body.String()) {
			t.Fatalf("another caller's live snapshot, ask %d: %d %q, want the gone bytes %q", i+1, w.Code, w.Body.String(), gone.Body.String())
		}
		if i == 9 && !limited(w) {
			t.Fatalf("the asker's 11th continuation: %d %s, want 429", w.Code, w.Body.String())
		}
	}
	if w := wallsTopoGet(s, "sub-walls-owner", oidc.RoleSecurityAdmin, "limit=10&cursor="+cursor); w.Code != http.StatusOK {
		t.Errorf("the owner's continuation after another caller's asks: %d %s", w.Code, w.Body.String())
	}
}

// TestWallsRunnerStarts: one probe per runner, its newest run first, each probe with its own run's created_at;
// a runner whose probe returned no allow row falls back to its next newest run, at most 3 times; a failed pass
// stops the read and adds no row.
func TestWallsRunnerStarts(t *testing.T) {
	clk := wallsTopoNewClock()
	one, two := uuid.New(), uuid.New()
	hosted := func(runner *uuid.UUID, age time.Duration) types.AgentRun {
		r := wallsTopoRun(nil, types.RunRunning, clk.now.Add(-age))
		r.RunnerID = runner
		return r
	}
	oneNew, oneOld := hosted(&one, time.Minute), hosted(&one, time.Hour)
	twoRuns := []types.AgentRun{hosted(&two, 2*time.Minute), hosted(&two, 3*time.Minute), hosted(&two, 4*time.Minute),
		hosted(&two, 5*time.Minute), hosted(&two, 6*time.Minute)}
	start := wallsTopoRow(oneNew.ID, "egress.allow", "builtin:upstream-proxy", "proxy.corp.example", 1, clk.now.Add(-2*time.Hour))
	newStore := func(failAt int) *wallsTopoStore {
		return &wallsTopoStore{firstErrAt: failAt, firsts: map[uuid.UUID][]store.DecisionRow{
			oneNew.ID:     {start},
			twoRuns[4].ID: {wallsTopoRow(twoRuns[4].ID, "egress.allow", "builtin:upstream-proxy", "proxy.corp.example", 1, clk.now)},
		}}
	}
	runs := append([]types.AgentRun{oneOld, twoRuns[2], oneNew}, twoRuns[0], twoRuns[4], twoRuns[1], twoRuns[3])

	st := newStore(0)
	starts := wallsTopoServer(st, clk).wallsRunnerStarts(context.Background(), runs)
	if len(starts) != 1 || starts[0].ID != start.ID {
		t.Errorf("starts = %+v, want the one start row of the first runner's newest run", starts)
	}
	wantCalls := [][]uuid.UUID{{oneNew.ID, twoRuns[0].ID}, {twoRuns[1].ID}, {twoRuns[2].ID}, {twoRuns[3].ID}}
	if len(st.firstCalls) != len(wantCalls) {
		t.Fatalf("%d probe passes %v, want %d", len(st.firstCalls), st.firstCalls, len(wantCalls))
	}
	byID := map[uuid.UUID]types.AgentRun{}
	for _, r := range runs {
		byID[r.ID] = r
	}
	byString := func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) }
	for i, ids := range st.firstCalls {
		if len(st.firstSince[i]) != len(ids) {
			t.Fatalf("pass %d sent %d runs and %d times", i+1, len(ids), len(st.firstSince[i]))
		}
		for j, id := range ids {
			if !st.firstSince[i][j].Equal(byID[id].CreatedAt) {
				t.Errorf("pass %d: run %s probed from %v, want its created_at %v", i+1, id, st.firstSince[i][j], byID[id].CreatedAt)
			}
		}
		got, want := slices.Clone(ids), slices.Clone(wantCalls[i])
		slices.SortFunc(got, byString)
		slices.SortFunc(want, byString)
		if !slices.Equal(got, want) {
			t.Errorf("pass %d probed %v, want %v", i+1, got, want)
		}
	}

	failed := newStore(2)
	if starts := wallsTopoServer(failed, clk).wallsRunnerStarts(context.Background(), runs); len(failed.firstCalls) != 2 ||
		slices.ContainsFunc(starts, func(r store.DecisionRow) bool { return r.RunID != oneNew.ID }) {
		t.Errorf("a failed second pass: %d passes, starts %+v; want the read stopped there with no row of it", len(failed.firstCalls), starts)
	}
}

// TestWallsTopologyEmptyListsArePresent: a first page with no units and no hosts sends both keys as [], never
// absent and never null.
func TestWallsTopologyEmptyListsArePresent(t *testing.T) {
	clk := wallsTopoNewClock()
	keys := wallsTopoKeys(t, wallsTopoGet(wallsTopoServer(&wallsTopoStore{}, clk), "sub-walls-unit", oidc.RoleUser, ""))
	for _, k := range []string{"units", "hosts"} {
		if list, ok := keys[k].([]any); !ok || len(list) != 0 {
			t.Errorf("an empty first page's %s is %#v, want []", k, keys[k])
		}
	}
}

// TestWallsTopologySnapshotKeepsNoRunRows: a snapshot keeps no whole run row, so no task text, and a continuation
// still reads each run's door from what it keeps (a No network run is sealed).
func TestWallsTopologySnapshotKeepsNoRunRows(t *testing.T) {
	clk := wallsTopoNewClock()
	st := wallsTopoRunsStore(clk, 2)
	for i := range st.runs {
		st.runs[i].Task = "planted-task-text"
	}
	st.runs[1].WallPreset = &types.WallPresetOnRun{ID: types.WallPresetNoNetwork, Scope: types.WallPresetScopeBuiltin, Name: "planted-preset-name"}
	s := wallsTopoServer(st, clk)
	cursor := wallsTopoOpen(t, s, "sub-walls-a", 1)
	snap := s.wallsSnapshots.get(wallsTopoMustDecode(t, cursor).id, clk.now)
	if snap == nil {
		t.Fatal("the first page stored no snapshot")
	}
	for _, planted := range []string{"planted-task-text", "planted-preset-name"} {
		if strings.Contains(fmt.Sprintf("%+v", *snap), planted) {
			t.Errorf("the snapshot keeps %q", planted)
		}
	}
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		if seen[typ] {
			return
		}
		seen[typ] = true
		if typ == reflect.TypeFor[types.AgentRun]() || typ == reflect.TypeFor[types.WallPresetOnRun]() {
			t.Errorf("the snapshot reaches %s", typ)
		}
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			walk(typ.Elem())
		case reflect.Map:
			walk(typ.Key())
			walk(typ.Elem())
		case reflect.Struct:
			for i := range typ.NumField() {
				walk(typ.Field(i).Type)
			}
		}
	}
	walk(reflect.TypeFor[wallsTopoSnapshot]())
	next := wallsTopoBody(t, wallsTopoGet(s, "sub-walls-a", oidc.RoleSecurityAdmin, "limit=1&cursor="+cursor))
	if len(next.Runs.Items) != 1 || next.Runs.Items[0].ID != st.runs[1].ID || next.Runs.Items[0].Egress != wallsEgressSealed {
		t.Errorf("the continuation serves %+v, want the No network run, sealed", next.Runs.Items)
	}
}

// wallsTopoApprovals answers the attention read with the pending approvals of the runs asked for.
type wallsTopoApprovals struct {
	ApprovalService // only ListPendingApprovalsForRuns is asked
	pending         []types.ApprovalRequest
}

func (a wallsTopoApprovals) ListPendingApprovalsForRuns(_ context.Context, ids []uuid.UUID) ([]types.ApprovalRequest, error) {
	var out []types.ApprovalRequest
	for _, ap := range a.pending {
		if slices.Contains(ids, ap.RunID) {
			out = append(out, ap)
		}
	}
	return out, nil
}

// wallsTopoSponsorStore is a wallsTopoStore whose every service identity is sponsored by sponsor.
type wallsTopoSponsorStore struct {
	*wallsTopoStore
	store.ServiceIdentityStore // only GetServiceIdentity is asked
	sponsor                    string
}

func (s *wallsTopoSponsorStore) GetServiceIdentity(_ context.Context, name string) (types.ServiceIdentity, error) {
	return types.ServiceIdentity{Name: name, Sponsor: s.sponsor}, nil
}

// TestWallsTopologySnapshotKeepsAttention: a continuation served from the snapshot gives a security caller the same
// attention the whole run rows give. A lost run reads as lost (its lost time), and a service's run under the second
// human switch is the caller's to decide (its recorded human author is someone else); without that author no
// decider can be shown not to be the author, and the hold would read as an admin's.
func TestWallsTopologySnapshotKeepsAttention(t *testing.T) {
	t.Setenv(envEgressSecondHuman, "true")
	clk := wallsTopoNewClock()
	st := wallsTopoRunsStore(clk, 3)
	lostAt := clk.now.Add(-time.Minute)
	st.runs[1].LostAt = &lostAt
	st.runs[2].CreatedBy, st.runs[2].HumanAuthor = types.ServicePrincipalPrefix+"ci", "sub-walls-author"
	s := wallsTopoServer(&wallsTopoSponsorStore{wallsTopoStore: st, sponsor: "sub-walls-sponsor"}, clk)
	s.cfg.Approvals = wallsTopoApprovals{pending: []types.ApprovalRequest{{ID: uuid.New(), RunID: st.runs[2].ID,
		Kind: types.ApprovalEgressDomain, State: types.ApprovalPending, RequestedAt: clk.now,
		RequestedScope: json.RawMessage(`{"host":"pypi.org","port":443,"mode":"wait_for_review"}`)}}}
	cursor := wallsTopoOpen(t, s, "sub-walls-a", 1)
	next := wallsTopoBody(t, wallsTopoGet(s, "sub-walls-a", oidc.RoleSecurityAdmin, "limit=2&cursor="+cursor))
	got := map[uuid.UUID]*types.RunAttention{}
	for _, it := range next.Runs.Items {
		got[it.ID] = it.Attention
	}
	if a := got[st.runs[1].ID]; a == nil || a.Kind != types.AttentionLost {
		t.Errorf("the lost run's attention is %+v, want lost", a)
	}
	if a := got[st.runs[2].ID]; a == nil || a.Kind != types.AttentionApproval || a.By != types.AttentionYou || a.Pending != 1 {
		t.Errorf("the service run's attention is %+v, want an approval for you, 1 pending", a)
	}
}

// TestWallsTopologyPartialOnlyWhenRowsRemain: reads that end exactly at the read cap leave partial unset, one row
// more sets it, and a probe of one row is the only read past the cap.
func TestWallsTopologyPartialOnlyWhenRowsRemain(t *testing.T) {
	for _, c := range []struct {
		rows    int
		partial bool
	}{{10, false}, {11, true}} {
		clk := wallsTopoNewClock()
		st := wallsTopoRunsStore(clk, 1)
		for range c.rows {
			st.add(wallsTopoRow(st.runs[0].ID, "egress.allow", "policy:allowlist", "pypi.org", 1, clk.now))
		}
		s := wallsTopoServer(st, clk)
		s.wallsCaps = wallsCaps{readLimit: 5, reads: 2}
		body := wallsTopoBody(t, wallsTopoGet(s, "sub-walls-a", oidc.RoleSecurityAdmin, ""))
		if body.Partial != c.partial || st.reads != 3 {
			t.Errorf("%d rows under 2 reads of 5: partial %v after %d reads, want %v after 3", c.rows, body.Partial, st.reads, c.partial)
		}
	}
}

// TestWallsTopologyRunListPartialOnlyWhenRunsRemain: a unit admin's run list that fills every page of the page
// bound, its runs with no unit dropped, is partial only when a probe finds one more run past the bound.
func TestWallsTopologyRunListPartialOnlyWhenRunsRemain(t *testing.T) {
	clk := wallsTopoNewClock()
	def := store.DefaultUnitID
	sc := wallsTopoScope{units: []uuid.UUID{def}, in: map[uuid.UUID]bool{def: true}}
	const keep = 1 // pages of keep+1 runs, so the page bound covers exactly this many runs
	bound := (keep + 1) * wallsTopoRunPages
	for _, c := range []struct {
		runs    int
		partial bool
	}{{bound, false}, {bound + 1, true}} {
		st := &wallsTopoStore{runs: []types.AgentRun{wallsTopoRun(&def, types.RunRunning, clk.now)}}
		for len(st.runs) < c.runs {
			st.runs = append(st.runs, wallsTopoRun(nil, types.RunRunning, clk.now))
		}
		s := wallsTopoServer(st, clk)
		runs, partial, err := s.wallsTopoRuns(context.Background(), st, sc, wallsTopoDefaultWindow, keep)
		if err != nil || len(runs) != 1 || partial != c.partial || st.runReads != wallsTopoRunPages+1 {
			t.Errorf("%d listed runs: %d kept, partial %v, %d reads, %v; want 1 kept, partial %v, %d reads",
				c.runs, len(runs), partial, st.runReads, err, c.partial, wallsTopoRunPages+1)
		}
	}
}
