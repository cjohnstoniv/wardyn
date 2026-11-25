// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wallsTopoOrgStore adds a static runner inventory to wallsTopoStore: runners in a fixed order, paged as the
// active filter pages them, each runner's tally, and no pool membership.
type wallsTopoOrgStore struct {
	*wallsTopoStore
	store.RunnerInventoryStore
	runners []types.Runner
	tally   map[string]types.RunnerTally
}

func (s *wallsTopoOrgStore) ListRunnersPage(_ context.Context, filter types.RunnerFilter, _ time.Time, p store.Page) ([]types.Runner, error) {
	var rows []types.Runner
	for _, r := range s.runners {
		if filter == types.RunnerFilterAll || (filter == types.RunnerFilterRevoked) == (r.State == types.RunnerRevoked) {
			rows = append(rows, r)
		}
	}
	lo, hi := min(p.Offset, len(rows)), len(rows)
	if p.Limit > 0 {
		hi = min(lo+p.Limit, len(rows))
	}
	return slices.Clone(rows[lo:hi]), nil
}

func (s *wallsTopoOrgStore) RunnerUsage(context.Context, []uuid.UUID, []string, uuid.UUID) (map[string]types.RunnerTally, error) {
	return s.tally, nil
}

func (s *wallsTopoOrgStore) RunnerPoolsOf(context.Context, []uuid.UUID) (map[uuid.UUID][]types.RunnerPoolRef, error) {
	return nil, nil
}

// wallsTopoOrg is the runner fixture: a busy online organisation runner, an idle one, an unclaimed and a revoked
// organisation runner, an online person-owned runner with runs and an offline one, and one run on the busy runner
// and one on the person's runner.
type wallsTopoOrg struct {
	st                                         *wallsTopoOrgStore
	s                                          *Server
	clk                                        *wallsTopoClock
	busy, idle, unclaimed, revoked, laptop, pc types.Runner
	hosted, device                             types.AgentRun
}

func wallsTopoNewOrg(t *testing.T) *wallsTopoOrg {
	t.Helper()
	clk := wallsTopoNewClock()
	runner := func(name string, state types.RunnerState, kind types.RunnerOwnerKind) types.Runner {
		return types.Runner{ID: uuid.New(), Name: name, Owner: "sub-walls-" + name, State: state, OwnerKind: kind, CreatedAt: clk.now.Add(-time.Hour)}
	}
	o := &wallsTopoOrg{clk: clk,
		busy: runner("busy", types.RunnerClaimed, types.RunnerOwnerOrg), idle: runner("idle", types.RunnerClaimed, types.RunnerOwnerOrg),
		unclaimed: runner("unclaimed", types.RunnerUnclaimed, types.RunnerOwnerOrg), revoked: runner("revoked", types.RunnerRevoked, types.RunnerOwnerOrg),
		laptop: runner("laptop", types.RunnerClaimed, types.RunnerOwnerPerson), pc: runner("pc", types.RunnerClaimed, types.RunnerOwnerPerson)}
	o.unclaimed.CreatedAt = clk.now
	o.hosted = wallsTopoRun(nil, types.RunRunning, clk.now.Add(-time.Hour))
	o.hosted.RunnerID = &o.busy.ID
	o.device = wallsTopoRun(nil, types.RunRunning, clk.now.Add(-time.Minute))
	o.device.RunnerID = &o.laptop.ID
	o.st = &wallsTopoOrgStore{wallsTopoStore: &wallsTopoStore{runs: []types.AgentRun{o.device, o.hosted}},
		runners: []types.Runner{o.pc, o.busy, o.revoked, o.laptop, o.unclaimed, o.idle},
		tally:   map[string]types.RunnerTally{o.busy.ID.String(): {Runs: 1}, o.laptop.ID.String(): {Runs: 2}}}
	o.s = wallsTopoServer(o.st, clk)
	online := map[uuid.UUID]bool{o.busy.ID: true, o.laptop.ID: true}
	o.s.cfg.RunnerOnline = func(id uuid.UUID) bool { return online[id] }
	return o
}

// wallsTopoAs is a GET of path as a signed-in sub with role.
func wallsTopoAs(path, sub, role string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	return r.WithContext(withOIDCRole(withOIDCHuman(r.Context(), sub), role))
}

// wallsTopoOrgBody is the security keys of a topology answer, runners kept as their bytes.
type wallsTopoOrgBody struct {
	Scope    string            `json:"scope"`
	Partial  bool              `json:"partial"`
	Runners  []json.RawMessage `json:"runners"`
	Devices  *WallsDevices     `json:"devices"`
	Gates    []WallsGate       `json:"gates"`
	Boundary *WallsBoundary    `json:"boundary"`
}

func wallsTopoOrgGet(t *testing.T, s *Server, sub, role string) wallsTopoOrgBody {
	t.Helper()
	w := wallsTopoGet(s, sub, role, "")
	if w.Code != http.StatusOK {
		t.Fatalf("topology as %s: %d %s", role, w.Code, w.Body.String())
	}
	var body wallsTopoOrgBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// wallsTopoCompact is raw without insignificant space, so items nested at different depths compare byte for byte.
func wallsTopoCompact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// wallsTopoRunnerID is the id field of one runner item.
func wallsTopoRunnerID(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v.ID
}

// TestWallsTopologyRunnersMatchGetRunners: for a security admin and a super admin, each runners[] item is, byte
// for byte, the item for that id in GET /api/v1/runners from the same caller on the same store.
func TestWallsTopologyRunnersMatchGetRunners(t *testing.T) {
	o := wallsTopoNewOrg(t)
	for i, role := range []string{oidc.RoleSecurityAdmin, oidc.RoleAdmin} {
		sub := fmt.Sprintf("sub-walls-reader-%d", i)
		w := httptest.NewRecorder()
		o.s.handleListRunners(w, wallsTopoAs("/api/v1/runners", sub, role))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: GET /runners %d %s", role, w.Code, w.Body.String())
		}
		var listed []json.RawMessage
		if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
			t.Fatal(err)
		}
		byID := map[string]string{}
		for _, raw := range listed {
			byID[wallsTopoRunnerID(t, raw)] = wallsTopoCompact(t, raw)
		}
		body := wallsTopoOrgGet(t, o.s, sub, role)
		if len(body.Runners) != 2 {
			t.Fatalf("%s: %d runners, want the 2 claimed organisation runners", role, len(body.Runners))
		}
		for _, raw := range body.Runners {
			id := wallsTopoRunnerID(t, raw)
			if got, want := wallsTopoCompact(t, raw), byID[id]; got != want {
				t.Errorf("%s: runner %s\n topology %s\n /runners %s", role, id, got, want)
			}
		}
	}
}

// TestWallsTopologyRunnerSet: unclaimed, revoked and person-owned runners are not in runners[] and an idle
// claimed organisation runner is; devices counts the claimed person-owned runners, with online and runs as
// their runner views state them; and runners read over two pages in either order give the same bytes.
func TestWallsTopologyRunnerSet(t *testing.T) {
	o := wallsTopoNewOrg(t)
	body := wallsTopoOrgGet(t, o.s, "sub-walls-sec", oidc.RoleSecurityAdmin)
	var ids []string
	for _, raw := range body.Runners {
		ids = append(ids, wallsTopoRunnerID(t, raw))
	}
	want := []string{o.busy.ID.String(), o.idle.ID.String()}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Errorf("runners %v, want the busy and the idle organisation runners by id %v", ids, want)
	}
	views, ok := o.s.runnerViews(httptest.NewRecorder(), wallsTopoAs("/", "sub-walls-sec", oidc.RoleSecurityAdmin), o.st,
		[]types.Runner{o.laptop, o.pc}, true)
	if !ok {
		t.Fatal("runner views failed")
	}
	devices := WallsDevices{Count: len(views)}
	for _, v := range views {
		if v.Online {
			devices.Online++
		}
		devices.Runs += v.Usage.InUse.Runs
	}
	if body.Devices == nil || *body.Devices != devices || devices.Online != 1 {
		t.Errorf("devices %+v, want %+v with one online", body.Devices, devices)
	}

	many := make([]types.Runner, wallsTopoRunnerPage+1)
	for i := range many {
		many[i] = types.Runner{ID: uuid.New(), Name: fmt.Sprintf("org-%d", i), Owner: "sub-walls-org", State: types.RunnerClaimed,
			OwnerKind: types.RunnerOwnerOrg, CreatedAt: o.clk.now.Add(-time.Hour)}
	}
	reversed := slices.Clone(many)
	slices.Reverse(reversed)
	var pages []string
	for i, order := range [][]types.Runner{many, reversed} {
		o.st.runners = order
		got := wallsTopoOrgGet(t, o.s, fmt.Sprintf("sub-walls-order-%d", i), oidc.RoleSecurityAdmin)
		if got.Partial || len(got.Runners) != len(many) {
			t.Fatalf("order %d: %d runners, partial %v; want %d and not partial", i, len(got.Runners), got.Partial, len(many))
		}
		raw, err := json.Marshal(got.Runners)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, wallsTopoCompact(t, raw))
	}
	if pages[0] != pages[1] {
		t.Error("the same runners read in two orders give different bytes")
	}
}

// TestWallsTopologyProjectionSwitch: a super admin and a security admin get scope org and the security keys; a
// unit admin who is not a security operator gets unit_subtree and none of them.
func TestWallsTopologyProjectionSwitch(t *testing.T) {
	o := wallsTopoNewOrg(t)
	for i, role := range []string{oidc.RoleAdmin, oidc.RoleSecurityAdmin} {
		keys := wallsTopoKeys(t, wallsTopoGet(o.s, fmt.Sprintf("sub-walls-sw-%d", i), role, ""))
		if keys["scope"] != wallsTopoScopeOrg {
			t.Errorf("%s: scope %v, want org", role, keys["scope"])
		}
		for _, k := range []string{"boundary", "runners", "devices", "gates"} {
			if _, ok := keys[k]; !ok {
				t.Errorf("%s: no %s key", role, k)
			}
		}
	}
	unit := wallsTopoServer(&wallsTopoUnitStore{wallsTopoStore: o.st.wallsTopoStore}, o.clk)
	keys := wallsTopoKeys(t, wallsTopoGet(unit, "sub-walls-unit", oidc.RoleUser, ""))
	if keys["scope"] != wallsTopoScopeUnit {
		t.Errorf("a unit admin: scope %v, want unit_subtree", keys["scope"])
	}
	for _, k := range []string{"boundary", "restrictions", "runners", "devices", "pools", "sites", "gates"} {
		if _, ok := keys[k]; ok {
			t.Errorf("a unit admin is served %s", k)
		}
	}
}

// TestWallsTopologyGates: the direct gate is always there; the site's upstream URL gate carries addr for a super
// admin only and a secret-ref site gate never does; the start row of a runner-hosted run, older than the window,
// still makes that runner's gate, with runner_id and no ca_trusted; and no gate id names a host or a port.
func TestWallsTopologyGates(t *testing.T) {
	o := wallsTopoNewOrg(t)
	o.st.site = types.SiteConfig{UpstreamProxyURL: "http://proxy.corp.example:3128"}
	start := wallsTopoRow(o.hosted.ID, "egress.allow", "builtin:upstream-proxy", "proxy.corp.example", 1, o.clk.now.Add(-time.Hour))
	start.Via = "upstream-proxy"
	o.st.firsts = map[uuid.UUID][]store.DecisionRow{o.hosted.ID: {start}}
	find := func(gates []WallsGate, source string) *WallsGate {
		for i := range gates {
			if gates[i].Source == source && gates[i].Kind != "direct" {
				return &gates[i]
			}
		}
		return nil
	}
	for i, role := range []string{oidc.RoleSecurityAdmin, oidc.RoleAdmin} {
		gates := wallsTopoOrgGet(t, o.s, fmt.Sprintf("sub-walls-gate-%d", i), role).Gates
		if !slices.ContainsFunc(gates, func(g WallsGate) bool { return g.Kind == "direct" }) {
			t.Errorf("%s: no direct gate in %+v", role, gates)
		}
		site := find(gates, "site")
		if site == nil || (site.Addr != "") != (role == oidc.RoleAdmin) {
			t.Errorf("%s: site gate %+v, want addr for a super admin only", role, site)
		}
		rg := find(gates, "runner")
		if rg == nil || rg.RunnerID == nil || *rg.RunnerID != o.busy.ID || rg.CATrusted != nil {
			t.Errorf("%s: runner gate %+v, want the busy runner's, with no ca_trusted", role, rg)
		}
		for _, g := range gates {
			if strings.Contains(g.ID, "proxy.corp.example") || strings.Contains(g.ID, "3128") {
				t.Errorf("%s: gate id %q names a host or a port", role, g.ID)
			}
		}
	}
	o.st.site = types.SiteConfig{UpstreamProxySecretRef: "corp-proxy"}
	if site := find(wallsTopoOrgGet(t, o.s, "sub-walls-gate-ref", oidc.RoleAdmin).Gates, "site"); site == nil || site.Addr != "" {
		t.Errorf("a secret-ref site gate %+v, want one with no addr", site)
	}
}

// TestWallsTopologyPagesKeepTheirKeys: a continuation page has no units, runners or gates key, and its items
// carry egress as the first page's do.
func TestWallsTopologyPagesKeepTheirKeys(t *testing.T) {
	o := wallsTopoNewOrg(t)
	o.st.envs = map[uuid.UUID]store.RunPolicyEnvelope{}
	for _, run := range o.st.runs {
		id := run.ID
		o.st.envs[id] = store.RunPolicyEnvelope{Resolve: []types.AuditEvent{{ID: uuid.New(), RunID: &id, Time: o.clk.now,
			Action: "run.policy.resolve", Data: json.RawMessage(`{"allowed_domains":["pypi.org"]}`)}}}
	}
	items := func(keys map[string]any) []any {
		runs, _ := keys["runs"].(map[string]any)
		list, _ := runs["items"].([]any)
		return list
	}
	first := wallsTopoKeys(t, wallsTopoGet(o.s, "sub-walls-keys", oidc.RoleSecurityAdmin, "limit=1"))
	firstRuns, _ := first["runs"].(map[string]any)
	cursor, _ := firstRuns["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("the first page has no next cursor")
	}
	next := wallsTopoKeys(t, wallsTopoGet(o.s, "sub-walls-keys", oidc.RoleSecurityAdmin, "limit=1&cursor="+cursor))
	for _, k := range []string{"units", "hosts", "runners", "gates"} {
		if _, ok := next[k]; ok {
			t.Errorf("the continuation page has a %s key", k)
		}
	}
	for name, page := range map[string]map[string]any{"first": first, "continuation": next} {
		list := items(page)
		if len(list) != 1 {
			t.Fatalf("the %s page has %d items, want 1", name, len(list))
		}
		if item, _ := list[0].(map[string]any); item["egress"] != wallsEgressAllowlist {
			t.Errorf("the %s page's item egress %v, want %s", name, item["egress"], wallsEgressAllowlist)
		}
	}
}

// TestWallsTopologyRunnerListPartialOnlyWhenRunnersRemain: an active runner list that fills every page of the page
// bound is partial only when a probe finds one more runner past it.
func TestWallsTopologyRunnerListPartialOnlyWhenRunnersRemain(t *testing.T) {
	bound := wallsTopoRunnerPage * wallsTopoRunnerPages
	for _, c := range []struct {
		runners int
		partial bool
	}{{bound, false}, {bound + 1, true}} {
		st := &wallsTopoOrgStore{runners: make([]types.Runner, c.runners)}
		for i := range st.runners {
			st.runners[i] = types.Runner{ID: uuid.New(), State: types.RunnerUnclaimed}
		}
		st.runners[0].State = types.RunnerClaimed
		claimed, partial, err := wallsTopoClaimedRunners(context.Background(), st, time.Unix(0, 0))
		if err != nil || len(claimed) != 1 || partial != c.partial {
			t.Errorf("%d active runners: %d claimed, partial %v, %v; want 1 claimed and partial %v", c.runners, len(claimed), partial, err, c.partial)
		}
	}
}
