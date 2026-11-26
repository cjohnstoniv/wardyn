// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunHostDecisions reads the host-list decisions of recent runs: scoped by units or by owner, bounded to a
// window and a run cap, and only the three rule sources the host list decides.
func TestRunHostDecisionsPG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	var reader store.RunHostDecisionReader = pg
	u1, u2 := uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	since, until := now.Add(-30*24*time.Hour), now.Add(time.Minute)

	runAt := func(unit uuid.UUID, owner string, created time.Time) uuid.UUID {
		t.Helper()
		r := newRun(types.RunRunning)
		r.UnitID = &unit
		r.CreatedBy = owner
		id := persistRun(t, ctx, pool, r).ID
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET created_at = $2 WHERE id = $1`, id, created); err != nil {
			t.Fatalf("set created_at: %v", err)
		}
		return id
	}
	row := func(run uuid.UUID, action, host, source string) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"host": host, "rule_source": source})
		ev := types.AuditEvent{ID: uuid.New(), Time: now, RunID: &run, ActorType: types.ActorAgent, Actor: "proxy", Action: action, Target: host, Outcome: "success", Data: data}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("seed %s: %v", action, err)
		}
	}
	rowAt := func(run uuid.UUID, action, host string, port int, source string) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"host": host, "port": port, "rule_source": source})
		ev := types.AuditEvent{ID: uuid.New(), Time: now, RunID: &run, ActorType: types.ActorAgent, Actor: "proxy", Action: action, Target: host, Outcome: "success", Data: data}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("seed %s: %v", action, err)
		}
	}
	alice, bob := "alice-"+uuid.NewString()+"@example.com", "bob-"+uuid.NewString()+"@example.com"
	rA := runAt(u1, alice, now.Add(-time.Hour))
	rB := runAt(u1, bob, now.Add(-2*time.Hour))
	rC := runAt(u2, alice, now.Add(-3*time.Hour))
	rOld := runAt(u1, alice, now.Add(-45*24*time.Hour))

	row(rA, "egress.allow", " A.Example ", egress.RuleSourcePolicyAllowed)
	row(rA, "egress.allow", "a.example", egress.RuleSourcePolicyAllowed) // the same decision twice is one row
	row(rA, "egress.deny", "b.example", egress.RuleSourceDefaultDeny)
	row(rA, "egress.deny", "e.example", egress.RuleSourcePolicyDenied)
	row(rA, "egress.allow", "api.anthropic.com", "brokered:llm")
	row(rA, "egress.allow", "c.example", "approval:"+uuid.NewString())
	row(rA, "egress.deny", "d.example", "policy:host-rule:approval-wait")
	row(rA, "egress.deny", "f.example", egress.RuleSourceDialFailed)
	row(rA, "egress.deny", "g.example", "policy:no-network")
	row(rB, "egress.allow", "a.example", egress.RuleSourcePolicyAllowed)
	row(rC, "egress.deny", "b.example", egress.RuleSourceDefaultDeny)
	row(rOld, "egress.allow", "old.example", egress.RuleSourcePolicyAllowed)

	hostsOf := func(rows []store.RunHostDecision) map[uuid.UUID][]string {
		m := map[uuid.UUID][]string{}
		for _, r := range rows {
			m[r.RunID] = append(m[r.RunID], r.Decision+" "+r.Host+" "+r.RuleSource)
		}
		return m
	}

	t.Run("units scope returns only the three host-list sources", func(t *testing.T) {
		rows, totals, err := reader.RunHostDecisions(ctx, store.ReplayScope{Units: []uuid.UUID{u1}}, since, until, 2000, nil)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if totals != (store.ReplayTotals{RunsInScope: 2, RunsCounted: 2}) {
			t.Fatalf("totals = %+v, want 2 in scope, 2 counted", totals)
		}
		want := map[uuid.UUID][]string{
			rA: {"allow a.example policy:allowed", "deny b.example policy:default-deny", "deny e.example policy:denied"},
			rB: {"allow a.example policy:allowed"},
		}
		if got := hostsOf(rows); !reflect.DeepEqual(got, want) {
			t.Fatalf("rows = %v, want %v", got, want)
		}
		for _, r := range rows {
			if r.UnitID == nil || *r.UnitID != u1 || r.CreatedAt.IsZero() {
				t.Fatalf("row %+v lost its unit or run time", r)
			}
			if r.RunID == rA && r.Owner != alice {
				t.Fatalf("owner = %q, want %q", r.Owner, alice)
			}
		}
	})

	t.Run("owner scope spans units", func(t *testing.T) {
		rows, totals, err := reader.RunHostDecisions(ctx, store.ReplayScope{Owner: alice}, since, until, 2000, nil)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if totals.RunsInScope != 2 || totals.RunsCounted != 2 || totals.Truncated {
			t.Fatalf("totals = %+v, want the two in-window runs of alice", totals)
		}
		got := hostsOf(rows)
		if len(got) != 2 || len(got[rA]) != 3 || !reflect.DeepEqual(got[rC], []string{"deny b.example policy:default-deny"}) {
			t.Fatalf("rows = %v", got)
		}
	})

	t.Run("the window is on the run's creation time", func(t *testing.T) {
		rows, totals, err := reader.RunHostDecisions(ctx, store.ReplayScope{Units: []uuid.UUID{u1}}, now.Add(-50*24*time.Hour), now.Add(-40*24*time.Hour), 2000, nil)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if totals.RunsInScope != 1 || len(rows) != 1 || rows[0].RunID != rOld || rows[0].Host != "old.example" {
			t.Fatalf("rows = %+v totals = %+v, want only the old run", rows, totals)
		}
	})

	t.Run("the cap keeps the most recent runs and says it truncated", func(t *testing.T) {
		rows, totals, err := reader.RunHostDecisions(ctx, store.ReplayScope{Units: []uuid.UUID{u1}}, since, until, 1, nil)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if totals != (store.ReplayTotals{RunsInScope: 2, RunsCounted: 1, Truncated: true}) {
			t.Fatalf("totals = %+v", totals)
		}
		for _, r := range rows {
			if r.RunID != rA {
				t.Fatalf("row of %s survived a cap of 1; the newest run is %s", r.RunID, rA)
			}
		}
		if len(rows) != 3 {
			t.Fatalf("rows = %d, want the newest run's 3", len(rows))
		}
	})

	t.Run("a scope names exactly one of units and owner", func(t *testing.T) {
		for name, sc := range map[string]store.ReplayScope{"neither": {}, "both": {Units: []uuid.UUID{u1}, Owner: alice}} {
			if _, _, err := reader.RunHostDecisions(ctx, sc, since, until, 10, nil); err == nil {
				t.Fatalf("%s: want an error", name)
			}
		}
	})

	// Excluded runs (the assistant's own) are left out in SQL, before the total and before the cap.
	u3 := uuid.New()
	carol := "carol-" + uuid.NewString() + "@example.com"
	eNewest := runAt(u3, carol, now.Add(-10*time.Minute)) // excluded, has a row
	eBare := runAt(u3, carol, now.Add(-20*time.Minute))   // excluded, no eligible audit row at all
	nA := runAt(u3, carol, now.Add(-30*time.Minute))
	nB := runAt(u3, carol, now.Add(-40*time.Minute))
	row(eBare, "egress.allow", "api.anthropic.com", "brokered:llm") // not a host-list row
	rowAt(eNewest, "egress.allow", "x.example", 443, egress.RuleSourcePolicyAllowed)
	rowAt(nA, "egress.allow", "reg.example", 443, egress.RuleSourcePolicyAllowed)
	rowAt(nA, "egress.allow", "reg.example", 8443, egress.RuleSourcePolicyAllowed)
	row(nA, "egress.allow", "old.example", egress.RuleSourcePolicyAllowed) // no data.port: an older row
	rowAt(nB, "egress.deny", "z.example", 443, egress.RuleSourceDefaultDeny)
	excluded := []uuid.UUID{eNewest, eBare}
	u3Scope := store.ReplayScope{Units: []uuid.UUID{u3}}

	t.Run("excluded runs are in neither total, with or without a host-list row", func(t *testing.T) {
		rows, totals, err := reader.RunHostDecisions(ctx, u3Scope, since, until, 2000, excluded)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if totals != (store.ReplayTotals{RunsInScope: 2, RunsCounted: 2}) {
			t.Fatalf("totals = %+v, want 2 in scope and 2 counted (eBare has no eligible row and still must not count)", totals)
		}
		for _, r := range rows {
			if r.RunID == eNewest || r.RunID == eBare {
				t.Fatalf("row %+v of an excluded run came back", r)
			}
		}
	})

	t.Run("with a cap of 1 a newest excluded run does not displace the ordinary run", func(t *testing.T) {
		rows, totals, err := reader.RunHostDecisions(ctx, u3Scope, since, until, 1, excluded)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if totals != (store.ReplayTotals{RunsInScope: 2, RunsCounted: 1, Truncated: true}) {
			t.Fatalf("totals = %+v", totals)
		}
		if len(rows) == 0 {
			t.Fatal("no rows: the ordinary run was displaced from the sample by an excluded one")
		}
		for _, r := range rows {
			if r.RunID != nA {
				t.Fatalf("row of %s with a cap of 1; the newest ordinary run is %s", r.RunID, nA)
			}
		}
	})

	t.Run("without an exclusion the same cap samples the newest run, excluded or not", func(t *testing.T) {
		for name, none := range map[string][]uuid.UUID{"nil": nil, "empty": {}} {
			rows, totals, err := reader.RunHostDecisions(ctx, u3Scope, since, until, 1, none)
			if err != nil {
				t.Fatalf("%s: read: %v", name, err)
			}
			if totals != (store.ReplayTotals{RunsInScope: 4, RunsCounted: 1, Truncated: true}) || len(rows) != 1 || rows[0].RunID != eNewest {
				t.Fatalf("%s: rows = %+v totals = %+v, want only the newest run of 4", name, rows, totals)
			}
		}
	})

	t.Run("the port is carried and ports are not collapsed; an older row has port 0", func(t *testing.T) {
		rows, _, err := reader.RunHostDecisions(ctx, u3Scope, since, until, 2000, excluded)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		got := map[string]bool{}
		for _, r := range rows {
			if r.RunID == nA {
				got[fmt.Sprintf("%s:%d", r.Host, r.Port)] = true
			}
		}
		want := map[string]bool{"reg.example:443": true, "reg.example:8443": true, "old.example:0": true}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("attempts = %v, want %v", got, want)
		}
	})
}
