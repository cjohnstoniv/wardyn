// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func replayRow(run uuid.UUID, host, decision, source string) store.RunHostDecision {
	return replayRowAt(run, host, 443, decision, source)
}

func replayRowAt(run uuid.UUID, host string, port int, decision, source string) store.RunHostDecision {
	return store.RunHostDecision{RunID: run, Owner: "p@example.com", CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Host: host, Port: port, Decision: decision, RuleSource: source}
}

// draftVerdict is the draft's host verdict for one attempt as TOOLS builds it: the proxy's own builtin
// evaluator over the draft's lists, asked with host and port.
func draftVerdict(allowed, denied []string) func(string, int) bool {
	ev := proxy.NewBuiltinEvaluator(types.RunPolicySpec{AllowedDomains: allowed, DeniedDomains: denied})
	return func(host string, port int) bool {
		v, _ := ev.EvaluateHost(context.Background(), egress.Request{Host: host, Port: port})
		return v == egress.VerdictAllow
	}
}

// replayCounts counts only decisions the host list made, run by run, one attempt (host and port) at a time.
func TestReplayCounts(t *testing.T) {
	run, other := uuid.New(), uuid.New()
	totals := store.ReplayTotals{RunsInScope: 5, RunsCounted: 5}
	admits := func(hosts ...string) func(string, int) bool {
		return func(h string, _ int) bool {
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
		allowed     func(string, int) bool
		refuse      int
		allow       int
		runsCounted int
	}{
		{"allowed host the draft drops", []store.RunHostDecision{replayRow(run, "a.example", "allow", egress.RuleSourcePolicyAllowed)}, admits(), 1, 0, 5},
		{"default-deny host the draft admits", []store.RunHostDecision{replayRow(run, "b.example", "deny", egress.RuleSourceDefaultDeny)}, admits("b.example"), 0, 1, 5},
		{"deny-list host the draft admits", []store.RunHostDecision{replayRow(run, "e.example", "deny", egress.RuleSourcePolicyDenied)}, admits("e.example"), 0, 1, 5},
		{"deny-list host still on the draft's deny list", []store.RunHostDecision{replayRow(run, "e.example", "deny", egress.RuleSourcePolicyDenied)}, admits(), 0, 0, 5},
		{"brokered model allow", []store.RunHostDecision{replayRow(run, "api.anthropic.com", "allow", "brokered:llm")}, admits(), 0, 0, 5},
		{"approval allow", []store.RunHostDecision{replayRow(run, "c.example", "allow", "approval:"+uuid.NewString())}, admits(), 0, 0, 5},
		{"host rule deny", []store.RunHostDecision{replayRow(run, "d.example", "deny", "policy:host-rule:approval-wait")}, admits("d.example"), 0, 0, 5},
		{"network fault deny", []store.RunHostDecision{replayRow(run, "f.example", "deny", egress.RuleSourceDialFailed)}, admits("f.example"), 0, 0, 5},
		{"one run, two dropped hosts", []store.RunHostDecision{replayRow(run, "a.example", "allow", egress.RuleSourcePolicyAllowed), replayRow(run, "z.example", "allow", egress.RuleSourcePolicyAllowed)}, admits(), 1, 0, 5},
		{"wildcard predicate admits a subdomain", []store.RunHostDecision{replayRow(run, "api.github.com", "allow", egress.RuleSourcePolicyAllowed)}, func(h string, _ int) bool { return strings.HasSuffix(h, ".github.com") }, 0, 0, 5},
		{"no rows", nil, admits(), 0, 0, 5},
		{"two runs at one dropped host", []store.RunHostDecision{replayRow(run, "a.example", "allow", egress.RuleSourcePolicyAllowed), replayRow(other, "a.example", "allow", egress.RuleSourcePolicyAllowed)}, admits(), 2, 0, 5},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := replayCounts(c.rows, totals, c.allowed)
			if got.WouldRefuse != c.refuse || got.WouldAllow != c.allow || got.RunsCounted != c.runsCounted || got.RunsInScope != 5 {
				t.Fatalf("refuse=%d allow=%d counted=%d scope=%d, want %d %d %d 5", got.WouldRefuse, got.WouldAllow, got.RunsCounted, got.RunsInScope, c.refuse, c.allow, c.runsCounted)
			}
			if (c.refuse+c.allow == 0) != (len(got.Runs) == 0) {
				t.Fatalf("Runs = %+v with refuse=%d allow=%d", got.Runs, c.refuse, c.allow)
			}
		})
	}

	t.Run("truncated totals pass through", func(t *testing.T) {
		got := replayCounts(nil, store.ReplayTotals{RunsInScope: 3000, RunsCounted: 2000, Truncated: true}, admits())
		if !got.Truncated || got.RunsInScope != 3000 || got.RunsCounted != 2000 {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("hosts and runs are described and ordered", func(t *testing.T) {
		t1, t2 := time.Unix(100, 0).UTC(), time.Unix(200, 0).UTC()
		rows := []store.RunHostDecision{
			{RunID: other, Owner: "o", CreatedAt: t2, Host: "b.example", Port: 443, Decision: "deny", RuleSource: egress.RuleSourceDefaultDeny},
			{RunID: run, Owner: "r", CreatedAt: t1, Host: "b.example", Port: 443, Decision: "allow", RuleSource: egress.RuleSourcePolicyAllowed},
			{RunID: run, Owner: "r", CreatedAt: t1, Host: "a.example", Port: 443, Decision: "allow", RuleSource: egress.RuleSourcePolicyAllowed},
		}
		got := replayCounts(rows, totals, admits("b.example"))
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

// The proxy matches a port-qualified allow or deny entry by host and port, so the same host on two ports
// can be answered two ways; a row with no recorded port is never compared.
func TestReplayCountsPorts(t *testing.T) {
	run, other := uuid.New(), uuid.New()
	totals := store.ReplayTotals{RunsInScope: 2, RunsCounted: 2}
	allowedToday := func(r uuid.UUID, host string, port int) store.RunHostDecision {
		return replayRowAt(r, host, port, "allow", egress.RuleSourcePolicyAllowed)
	}
	deniedToday := func(r uuid.UUID, host string, port int) store.RunHostDecision {
		return replayRowAt(r, host, port, "deny", egress.RuleSourcePolicyDenied)
	}
	t.Run("an allow entry for one port refuses the other", func(t *testing.T) {
		rows := []store.RunHostDecision{allowedToday(run, "reg.example", 443), allowedToday(run, "reg.example", 8443)}
		got := replayCounts(rows, totals, draftVerdict([]string{"reg.example:443"}, nil))
		if got.WouldRefuse != 1 || got.WouldAllow != 0 || got.NotComparable != 0 {
			t.Fatalf("got %+v, want 1 refused (8443 only)", got)
		}
		want := []replayHost{{Host: "reg.example", Runs: 1, Today: "allow", Under: "mixed", RunsChanged: 1}}
		if !reflect.DeepEqual(got.Hosts, want) || !reflect.DeepEqual(got.Runs[0].Hosts, []string{"reg.example"}) {
			t.Fatalf("hosts = %+v runs = %+v, want %+v", got.Hosts, got.Runs, want)
		}
	})
	t.Run("two runs on two ports are each judged on their own port", func(t *testing.T) {
		rows := []store.RunHostDecision{allowedToday(run, "reg.example", 443), allowedToday(other, "reg.example", 8443)}
		got := replayCounts(rows, totals, draftVerdict([]string{"reg.example:443"}, nil))
		if got.WouldRefuse != 1 || len(got.Runs) != 1 || got.Runs[0].RunID != other {
			t.Fatalf("got %+v, want only the 8443 run refused", got)
		}
	})
	t.Run("a deny entry for one port opens nothing on the other", func(t *testing.T) {
		rows := []store.RunHostDecision{deniedToday(run, "bad.example", 443), deniedToday(run, "bad.example", 8443)}
		got := replayCounts(rows, totals, draftVerdict([]string{"bad.example"}, []string{"bad.example:443"}))
		if got.WouldAllow != 1 || got.WouldRefuse != 0 {
			t.Fatalf("got %+v, want 1 would-allow (8443 only)", got)
		}
		if got.Hosts[0].Under != "mixed" || got.Hosts[0].Today != "deny" {
			t.Fatalf("host = %+v", got.Hosts[0])
		}
	})
	t.Run("a bare allow entry covers every port", func(t *testing.T) {
		rows := []store.RunHostDecision{allowedToday(run, "reg.example", 443), allowedToday(run, "reg.example", 8443)}
		got := replayCounts(rows, totals, draftVerdict([]string{"reg.example"}, nil))
		if got.WouldRefuse != 0 || len(got.Runs) != 0 || got.Hosts[0].Under != "allow" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("a row with no port is not comparable, not allowed", func(t *testing.T) {
		rows := []store.RunHostDecision{allowedToday(run, "reg.example", 0), deniedToday(other, "bad.example", 0)}
		got := replayCounts(rows, totals, draftVerdict([]string{"bad.example"}, nil))
		if got.NotComparable != 2 || got.WouldRefuse != 0 || got.WouldAllow != 0 || len(got.Runs) != 0 {
			t.Fatalf("got %+v, want 2 not comparable and no change counted", got)
		}
		for _, h := range got.Hosts {
			if h.Under != "unknown" || h.RunsChanged != 0 || h.Runs != 1 {
				t.Fatalf("host = %+v, want reached once and unknown under the draft", h)
			}
		}
	})
	t.Run("an old row beside a port-recorded row of the same host", func(t *testing.T) {
		rows := []store.RunHostDecision{allowedToday(run, "reg.example", 0), allowedToday(run, "reg.example", 443)}
		got := replayCounts(rows, totals, draftVerdict(nil, nil))
		if got.NotComparable != 1 || got.WouldRefuse != 1 || got.Hosts[0].Under != "deny" {
			t.Fatalf("got %+v, want the 443 attempt refused and the run also flagged not comparable", got)
		}
	})
}
