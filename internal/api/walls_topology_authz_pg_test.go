// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// wallsTopoPGPath is the topology route.
const wallsTopoPGPath = "/api/v1/walls/topology"

// wallsTopoPGGet is the topology as cookie's caller, which must answer 200.
func wallsTopoPGGet(t *testing.T, e *unitAuditEnv, c *http.Cookie, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := e.call(c, http.MethodGet, wallsTopoPGPath+"?"+query, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s?%s: %d %s", wallsTopoPGPath, query, w.Code, w.Body.String())
	}
	return w
}

// wallsTopoPGWalk calls visit with every object key in v at any depth and the key of the object holding it.
func wallsTopoPGWalk(v any, parent string, visit func(key, parent string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			visit(k, parent)
			wallsTopoPGWalk(child, k, visit)
		}
	case []any:
		for _, child := range x {
			wallsTopoPGWalk(child, parent, visit)
		}
	}
}

// wallsTopoPGUnitKeys is every key a unit admin's answer may hold: the top level, the run-item list and its
// counters, WallsUnitCounters, the unit fields GET /governance already serves them with the unit's runs and
// counts, hosts and edges, and elsewhere. A by_state object's keys are run states.
var wallsTopoPGUnitKeys = map[string]bool{
	"as_of": true, "scope": true, "window_sec": true, "partial": true, "units": true, "runs": true, "hosts": true, "edges": true,
	"elsewhere": true, "next_cursor": true, "items": true, "unit_host": true, "run_host": true,
	"id": true, "unit_id": true, "state": true, "confinement_class": true, "placement": true, "runner_id": true,
	"runner_pool_id": true, "machine_kind": true, "created_by": true, "created_at": true, "counters": true,
	"allowed": true, "refused": true, "guest": true, "held": true, "fault": true, "fidelity": true, "unclassified": true,
	"name": true, "parent_id": true, "kind": true, "mode": true, "depth": true, "egress_mode": true, "egress": true,
	"live": true, "by_state": true, "host": true, "port": true, "run_id": true, "not_allowed": true,
}

// TestWallsTopologyUnitProjectionKeys: every key of a unit admin's whole answer, with either edge form, is in
// the allowed set; none of the security keys or row details appears at any depth; and no run item carries
// owner_kind or egress.
func TestWallsTopologyUnitProjectionKeys(t *testing.T) {
	e := newUnitAuditEnv(t)
	for _, run := range []uuid.UUID{e.runA, e.runA1, e.runB, e.runNone} {
		e.seedRow(&run, "egress.allow", "pypi.org", map[string]any{"host": "pypi.org", "port": 443, "rule_source": "policy:allowlist",
			"by": "policy", "approval_id": uuid.NewString(), "sponsor": "sub-walls-sponsor"})
		e.seedRow(&run, "egress.hold", "files.example", map[string]any{"host": "files.example", "port": 443,
			"rule_source": "approval:pending", "policy_contact": "security@corp.example"})
	}
	forbidden := []string{"boundary", "restrictions", "restriction_ids", "pools", "runners", "devices", "sites", "gates",
		"rule_source", "by", "reason", "policy_contact", "subjects", "attention"}
	for _, query := range []string{"edges=summary", "edges=full"} {
		w := wallsTopoPGGet(t, e, e.adminA, query)
		var v map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		wallsTopoPGWalk(v, "", func(key, parent string) {
			if parent == "by_state" {
				return
			}
			if !wallsTopoPGUnitKeys[key] || slices.Contains(forbidden, key) {
				t.Errorf("%s: a unit admin is served the key %q (under %q)", query, key, parent)
			}
		})
		runs, _ := v["runs"].(map[string]any)
		items, _ := runs["items"].([]any)
		if len(items) == 0 {
			t.Fatalf("%s: no run item served", query)
		}
		for _, it := range items {
			item, _ := it.(map[string]any)
			for _, k := range []string{"owner_kind", "egress"} {
				if _, ok := item[k]; ok {
					t.Errorf("%s: a run item carries %s", query, k)
				}
			}
		}
	}
}

// wallsTopoPGSeeds are one row of each class, plus a 129-character host and a host with a space.
func wallsTopoPGSeeds() []struct {
	action string
	data   map[string]any
} {
	type seed = struct {
		action string
		data   map[string]any
	}
	row := func(action, host, source string, extra ...any) seed {
		d := map[string]any{"host": host, "port": 443, "rule_source": source}
		for i := 0; i+1 < len(extra); i += 2 {
			d[extra[i].(string)] = extra[i+1]
		}
		return seed{action, d}
	}
	return []seed{
		row("egress.allow", "pypi.org", "policy:allowlist"),
		row("egress.allow", strings.Repeat("a", 121)+".example", "policy:allowlist"),
		row("egress.allow", "bad host.example", "policy:allowlist"),
		row("egress.deny", "evil.example", "policy:denied"),
		row("egress.deny", "vm.example", "policy:denied", "flow", "flow-1"),
		row("egress.deny", "down.example", "policy:denied", "by", "outside"),
		row("egress.deny", "lost.example", "egress:dropped-decisions-3"),
		row("egress.deny", "odd.example", "policy:something-new"),
		row("egress.hold", "files.example", "approval:pending"),
		row("egress.pending", "files.example", "approval:pending"),
		row("egress.allow", "cp.wardyn.example", "brokered:mint"),
		row("egress.allow", "tools.example", "policy:tool-allow"),
		row("egress.allow", "proxy.corp.example", "builtin:upstream-proxy"),
	}
}

// TestWallsTopologyUnitCountersMatchAudit: with every class seeded inside a unit admin's subtree and outside it,
// the sum of their units' counters equals the egress rows GET /api/v1/audit returns them for the window, less
// the control-plane-host rows and the upstream start rows, and each class equals wallsClassify over the same
// stored rows. A 129-character host and a host with a space are in neither the unit view nor hosts[], and their
// rows still count.
func TestWallsTopologyUnitCountersMatchAudit(t *testing.T) {
	e := newUnitAuditEnv(t)
	ctx := context.Background()
	in := []uuid.UUID{e.runA, e.runA1, e.runOwnA}
	for _, run := range append(slices.Clone(in), e.runB, e.runNone) {
		for _, s := range wallsTopoPGSeeds() {
			e.seedRow(&run, s.action, fmt.Sprint(s.data["host"]), s.data)
		}
	}
	w := wallsTopoPGGet(t, e, e.adminA, "")
	var body struct {
		Units []struct {
			Egress *WallsUnitCounters `json:"egress"`
		} `json:"units"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var sum WallsUnitCounters
	for _, u := range body.Units {
		if c := u.Egress; c != nil {
			sum.Allowed, sum.Refused, sum.Guest, sum.Held = sum.Allowed+c.Allowed, sum.Refused+c.Refused, sum.Guest+c.Guest, sum.Held+c.Held
			sum.Fault, sum.Fidelity, sum.Unclassified = sum.Fault+c.Fault, sum.Fidelity+c.Fidelity, sum.Unclassified+c.Unclassified
		}
	}
	rows, _, err := e.pg.AuditWallsRows(ctx, store.DecisionQuery{Actions: wallsTopoActions, Since: time.Now().Add(-time.Hour), RunIDs: in, Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	var want wallsTopoCount
	hidden := map[uuid.UUID]bool{}
	for _, r := range rows {
		if v := wallsClassify(r); v.controlPlaneHost() || v.Class == wallsConfig {
			hidden[r.ID] = true
		} else {
			want.add(v, 1, false)
		}
	}
	if got := *want.wire(false).(*WallsUnitCounters); got != sum {
		t.Errorf("unit counters %+v, want wallsClassify's %+v", sum, got)
	}
	audit := 0
	for _, ev := range e.read(e.adminA, "/api/v1/audit", "?limit=500") {
		if strings.HasPrefix(ev.Action, "egress.") && !hidden[ev.ID] {
			audit++
		}
	}
	if total := sum.Allowed + sum.Refused + sum.Held + sum.Fault + sum.Fidelity + sum.Unclassified; total != int64(audit) {
		t.Errorf("the unit counters total %d rows, GET /api/v1/audit serves %d", total, audit)
	}
	for _, h := range []string{strings.Repeat("a", 121) + ".example", "bad host.example"} {
		if strings.Contains(w.Body.String(), h) {
			t.Errorf("the answer serves the host %q", h)
		}
	}
}

// TestWallsTopologyOutOfScopeWritesChangeNothing: rows written for runs outside a unit admin's subtree change no
// byte of their answer but elsewhere and as_of, and more of them than the count bound leave partial unset.
func TestWallsTopologyOutOfScopeWritesChangeNothing(t *testing.T) {
	e := newUnitAuditEnv(t)
	e.srv.wallsCaps = wallsCaps{countRows: 20, readLimit: 10}
	strip := func(w *httptest.ResponseRecorder) (string, bool) {
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		partial, _ := m["partial"].(bool)
		delete(m, "elsewhere")
		delete(m, "as_of")
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw), partial
	}
	before, _ := strip(wallsTopoPGGet(t, e, e.adminA, "edges=full"))
	for i := range 30 {
		for _, run := range []uuid.UUID{e.runB, e.runNone} {
			host := fmt.Sprintf("out-%d.example", i)
			e.seedRow(&run, "egress.allow", host, map[string]any{"host": host, "port": 443, "rule_source": "policy:allowlist"})
		}
	}
	after, partial := strip(wallsTopoPGGet(t, e, e.adminA, "edges=full"))
	if after != before {
		t.Errorf("writes outside the subtree changed the answer:\n before %s\n after  %s", before, after)
	}
	if partial {
		t.Error("rows outside the subtree past the count bound set partial")
	}
}

// TestWallsTopologyScopeIsReadPerRequest: after a full revoke, a first page and the old cursor's continuation
// both get 403 unit_scope_denied; with one of two grants revoked, a first page gets 200 and the old cursor's
// continuation gets 409 walls_snapshot_gone.
func TestWallsTopologyScopeIsReadPerRequest(t *testing.T) {
	e := newUnitAuditEnv(t)
	ctx := context.Background()
	e.seedRun(&e.a1.ID, uaRunOwner)
	revoke := func(g types.GovernanceUnitAdmin) {
		t.Helper()
		if _, err := store.RevokeUnitAdminQ(ctx, e.pool, g.UnitID, g.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
	}
	open := func(c *http.Cookie) string {
		t.Helper()
		var body struct {
			Runs WallsRuns `json:"runs"`
		}
		if err := json.Unmarshal(wallsTopoPGGet(t, e, c, "limit=1").Body.Bytes(), &body); err != nil || body.Runs.NextCursor == "" {
			t.Fatalf("first page: %v, next cursor %q", err, body.Runs.NextCursor)
		}
		return body.Runs.NextCursor
	}
	want := func(name string, w *httptest.ResponseRecorder, code int, reason string) {
		t.Helper()
		if w.Code != code || errorReason(w) != reason {
			t.Errorf("%s: %d %s, want %d %s", name, w.Code, w.Body.String(), code, reason)
		}
	}

	one := uaSession(t, "sub-walls-scope-one", []string{}, false)
	g := e.grant(e.a1, types.CapabilitySubjectUser, "sub-walls-scope-one")
	cursor := open(one)
	revoke(g)
	want("a first page after a full revoke", e.call(one, http.MethodGet, wallsTopoPGPath+"?limit=1", ""),
		http.StatusForbidden, string(authz.ReasonUnitScopeDenied))
	want("a continuation after a full revoke", e.call(one, http.MethodGet, wallsTopoPGPath+"?limit=1&cursor="+cursor, ""),
		http.StatusForbidden, string(authz.ReasonUnitScopeDenied))

	two := uaSession(t, "sub-walls-scope-two", []string{}, false)
	e.grant(e.a1, types.CapabilitySubjectUser, "sub-walls-scope-two")
	gb := e.grant(e.b, types.CapabilitySubjectUser, "sub-walls-scope-two")
	cursor = open(two)
	revoke(gb)
	want("a first page with one grant left", e.call(two, http.MethodGet, wallsTopoPGPath+"?limit=1", ""), http.StatusOK, "")
	want("a continuation with one grant left", e.call(two, http.MethodGet, wallsTopoPGPath+"?limit=1&cursor="+cursor, ""),
		http.StatusConflict, string(reasonWallsSnapshotGone))
}

// TestWallsTopologyFirstPageTiming times first pages at production bounds on two seeded shapes: 5,000 runs over
// 50 organisation runners, and 5,000 runs over 2,000 runners. Each has 250,000 window rows, 64 of them allows of
// each runner's newest run. It times 20 first pages from 20 callers and logs the p95; it never fails on time.
// It runs only with WARDYN_WALLS_TIMING=1.
func TestWallsTopologyFirstPageTiming(t *testing.T) {
	if os.Getenv("WARDYN_WALLS_TIMING") != "1" {
		t.Skip("WARDYN_WALLS_TIMING=1 times the first page on 250,000 seeded rows")
	}
	const runsN, rowsN, probeRows, batch = 5000, 250000, 64, 10000
	for _, shape := range []struct {
		name    string
		runners int
	}{{"organisation runners", 50}, {"devices", 2000}} {
		t.Run(shape.name, func(t *testing.T) {
			e := newGovEnv(t)
			ctx := context.Background()
			start := time.Now().UTC()
			runners := make([]uuid.UUID, shape.runners)
			for i := range runners {
				id := uuid.New()
				if _, err := e.pg.CreateRunner(ctx, types.Runner{ID: id, Owner: fmt.Sprintf("sub-walls-timing-%d", i), Name: fmt.Sprintf("r%d", i),
					PublicKey: make([]byte, 32), KeyFingerprint: id.String(), Version: "0.9.0", Limits: roomyRunnerLimits()}); err != nil {
					t.Fatal(err)
				}
				runners[i] = id
			}
			runs := make([]uuid.UUID, runsN)
			for i := range runs {
				id := uuid.New()
				runner := runners[i%len(runners)]
				if _, err := e.pg.CreateRun(ctx, types.AgentRun{ID: id, CreatedAt: start.Add(-time.Duration(i) * time.Millisecond), CreatedBy: "sub-walls-timing",
					Agent: "shell", Task: "t", ConfinementClass: types.CC2, State: types.RunRunning, RunnerTarget: "docker",
					SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerID: &runner}); err != nil {
					t.Fatal(err)
				}
				runs[i] = id
			}
			seed := func(ids []uuid.UUID, n int, source string) {
				t.Helper()
				for done := 0; done < n; done += batch {
					if _, err := e.pool.Exec(ctx, `SELECT count(*) FROM generate_series(1, $1::int) g,
						LATERAL audit_append(gen_random_uuid(), now(), ($2::uuid[])[1 + (g - 1) % cardinality($2::uuid[])], 'agent',
							'spiffe://wardyn.local/walls-timing', 'egress.allow', '', 'success', '',
							jsonb_build_object('host', 'h' || (g % 400) || '.example', 'port', 443, 'rule_source', $3::text)) a`,
						min(batch, n-done), ids, source); err != nil {
						t.Fatalf("seed rows: %v", err)
					}
				}
			}
			probed := runs[:len(runners)] // each runner's newest run
			for range probeRows {
				seed(probed, len(probed), "policy:allowlist")
			}
			seed(runs, rowsN-probeRows*len(probed), "policy:allowlist")
			took := make([]time.Duration, 20)
			for i := range took {
				c := ssoSession(t, fmt.Sprintf("sub-walls-timer-%d", i), fmt.Sprintf("timer-%d@corp.example", i), oidc.RoleSecurityAdmin)
				begin := time.Now()
				w := e.call(c, http.MethodGet, wallsTopoPGPath, "")
				took[i] = time.Since(begin)
				if w.Code != http.StatusOK {
					t.Fatalf("first page %d: %d %s", i, w.Code, w.Body.String())
				}
			}
			slices.Sort(took)
			t.Logf("%s: p95 of 20 first pages = %v (5,000 runs, %d runners, %d rows)", shape.name, took[18], len(runners), rowsN)
		})
	}
}

// TestWallsTopologyDoor: route A (the topology) and the topology's live stream answer each caller as GET
// /api/v1/governance's door does: a super admin, a security admin and a unit admin get in (the topology 200),
// and a member, a portal's delegated token and a service token get the door's own refusal.
func TestWallsTopologyDoor(t *testing.T) {
	e := newUnitAuditEnv(t)
	paths := []string{wallsTopoPGPath, "/api/v1/walls/stream"}
	check := func(who string, call func(path string) *httptest.ResponseRecorder, in bool) {
		t.Helper()
		probe := call("/api/v1/governance")
		if (probe.Code == http.StatusOK) != in {
			t.Fatalf("%s: GET /api/v1/governance %d %s", who, probe.Code, probe.Body.String())
		}
		for _, path := range paths {
			w := call(path)
			switch {
			case in && path == wallsTopoPGPath && w.Code != http.StatusOK:
				t.Errorf("%s on %s: %d %s, want 200", who, path, w.Code, w.Body.String())
			case in && (w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden):
				t.Errorf("%s on %s: %d %s, want past the door", who, path, w.Code, w.Body.String())
			case !in && (w.Code != probe.Code || errorReason(w) != errorReason(probe)):
				t.Errorf("%s on %s: %d %s, want the door's %d %s", who, path, w.Code, w.Body.String(), probe.Code, errorReason(probe))
			}
		}
	}
	h := e.srv.Handler()
	for _, c := range []struct {
		who    string
		cookie *http.Cookie
		in     bool
	}{{"a super admin", e.super, true}, {"a security admin", e.alice, true}, {"a unit admin", e.adminA, true}, {"a member", e.member, false}} {
		check(c.who, func(path string) *httptest.ResponseRecorder { return wallsRoutesServe(t, h, path, c.cookie, "") }, c.in)
	}
	srv, ast, _, _ := newAuthzMatrixServer(t)
	delegated, _ := seedDelegation(t, ast.fakeDelegateStore, "sub-walls-door-portal")
	check("a portal token", func(path string) *httptest.ResponseRecorder {
		return wallsRoutesServe(t, srv.Handler(), path, nil, delegated)
	}, false)
	svc, _, svcToken := wallsRoutesServiceServer(t)
	check("a service token", func(path string) *httptest.ResponseRecorder {
		return wallsRoutesServe(t, svc.Handler(), path, nil, svcToken)
	}, false)
}
