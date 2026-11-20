// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

func replayRow(run uuid.UUID, host, decision, source string) store.RunHostDecision {
	return store.RunHostDecision{RunID: run, Owner: "p@example.com", CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Host: host, Decision: decision, RuleSource: source}
}

// replayCounts counts only decisions the host list made, run by run, and never an excluded run.
func TestReplayCounts(t *testing.T) {
	run, other := uuid.New(), uuid.New()
	totals := store.ReplayTotals{RunsInScope: 5, RunsCounted: 5}
	admits := func(hosts ...string) func(string) bool {
		return func(h string) bool {
			for _, a := range hosts {
				if a == h {
					return true
				}
			}
			return false
		}
	}
	for _, c := range []struct {
		name        string
		rows        []store.RunHostDecision
		allowed     func(string) bool
		excluded    func(uuid.UUID) bool
		refuse      int
		allow       int
		runsCounted int
	}{
		{"allowed host the draft drops", []store.RunHostDecision{replayRow(run, "a.example", "allow", egress.RuleSourcePolicyAllowed)}, admits(), nil, 1, 0, 5},
		{"default-deny host the draft admits", []store.RunHostDecision{replayRow(run, "b.example", "deny", egress.RuleSourceDefaultDeny)}, admits("b.example"), nil, 0, 1, 5},
		{"deny-list host the draft admits", []store.RunHostDecision{replayRow(run, "e.example", "deny", egress.RuleSourcePolicyDenied)}, admits("e.example"), nil, 0, 1, 5},
		{"deny-list host still on the draft's deny list", []store.RunHostDecision{replayRow(run, "e.example", "deny", egress.RuleSourcePolicyDenied)}, admits(), nil, 0, 0, 5},
		{"brokered model allow", []store.RunHostDecision{replayRow(run, "api.anthropic.com", "allow", "brokered:llm")}, admits(), nil, 0, 0, 5},
		{"approval allow", []store.RunHostDecision{replayRow(run, "c.example", "allow", "approval:"+uuid.NewString())}, admits(), nil, 0, 0, 5},
		{"host rule deny", []store.RunHostDecision{replayRow(run, "d.example", "deny", "policy:host-rule:approval-wait")}, admits("d.example"), nil, 0, 0, 5},
		{"network fault deny", []store.RunHostDecision{replayRow(run, "f.example", "deny", egress.RuleSourceDialFailed)}, admits("f.example"), nil, 0, 0, 5},
		{"assistant run", []store.RunHostDecision{replayRow(run, "a.example", "allow", egress.RuleSourcePolicyAllowed)}, admits(), func(id uuid.UUID) bool { return id == run }, 0, 0, 4},
		{"one run, two dropped hosts", []store.RunHostDecision{replayRow(run, "a.example", "allow", egress.RuleSourcePolicyAllowed), replayRow(run, "z.example", "allow", egress.RuleSourcePolicyAllowed)}, admits(), nil, 1, 0, 5},
		{"wildcard predicate admits a subdomain", []store.RunHostDecision{replayRow(run, "api.github.com", "allow", egress.RuleSourcePolicyAllowed)}, func(h string) bool { return strings.HasSuffix(h, ".github.com") }, nil, 0, 0, 5},
		{"no rows", nil, admits(), nil, 0, 0, 5},
		{"two runs at one dropped host", []store.RunHostDecision{replayRow(run, "a.example", "allow", egress.RuleSourcePolicyAllowed), replayRow(other, "a.example", "allow", egress.RuleSourcePolicyAllowed)}, admits(), nil, 2, 0, 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := replayCounts(c.rows, totals, c.allowed, c.excluded)
			if got.WouldRefuse != c.refuse || got.WouldAllow != c.allow || got.RunsCounted != c.runsCounted || got.RunsInScope != 5 {
				t.Fatalf("refuse=%d allow=%d counted=%d scope=%d, want %d %d %d 5", got.WouldRefuse, got.WouldAllow, got.RunsCounted, got.RunsInScope, c.refuse, c.allow, c.runsCounted)
			}
			if (c.refuse+c.allow == 0) != (len(got.Runs) == 0) {
				t.Fatalf("Runs = %+v with refuse=%d allow=%d", got.Runs, c.refuse, c.allow)
			}
		})
	}

	t.Run("truncated totals pass through", func(t *testing.T) {
		got := replayCounts(nil, store.ReplayTotals{RunsInScope: 3000, RunsCounted: 2000, Truncated: true}, admits(), nil)
		if !got.Truncated || got.RunsInScope != 3000 || got.RunsCounted != 2000 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("hosts and runs are described and ordered", func(t *testing.T) {
		t1, t2 := time.Unix(100, 0).UTC(), time.Unix(200, 0).UTC()
		rows := []store.RunHostDecision{
			{RunID: other, Owner: "o", CreatedAt: t2, Host: "b.example", Decision: "deny", RuleSource: egress.RuleSourceDefaultDeny},
			{RunID: run, Owner: "r", CreatedAt: t1, Host: "b.example", Decision: "allow", RuleSource: egress.RuleSourcePolicyAllowed},
			{RunID: run, Owner: "r", CreatedAt: t1, Host: "a.example", Decision: "allow", RuleSource: egress.RuleSourcePolicyAllowed},
		}
		got := replayCounts(rows, totals, admits("b.example"), nil)
		wantHosts := []replayHost{
			{Host: "a.example", Runs: 1, Today: "allow", Under: "deny", RunsChanged: 1},
			{Host: "b.example", Runs: 2, Today: "mixed", Under: "allow", RunsChanged: 1},
		}
		if !reflect.DeepEqual(got.Hosts, wantHosts) {
			t.Fatalf("hosts = %+v, want %+v", got.Hosts, wantHosts)
		}
		wantRuns := []replayRun{
			{RunID: run, Owner: "r", CreatedAt: t1, Hosts: []string{"a.example"}},
			{RunID: other, Owner: "o", CreatedAt: t2, Hosts: []string{"b.example"}},
		}
		if !reflect.DeepEqual(got.Runs, wantRuns) || got.WouldRefuse != 1 || got.WouldAllow != 1 {
			t.Fatalf("runs = %+v refuse=%d allow=%d", got.Runs, got.WouldRefuse, got.WouldAllow)
		}
	})
}
