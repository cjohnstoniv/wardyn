// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// replayHost is one host the counted runs reached: how many runs, what the host list decided for it
// today, what the draft decides, and how many of those runs a different answer would change. It is built
// after the per-attempt comparison, so one host reached on two ports can be "mixed" under the draft.
type replayHost struct {
	Host        string
	Runs        int
	Today       string // "allow", "deny" or "mixed"
	Under       string // "allow", "deny", "mixed" across ports, or "unknown" when no attempt at the host had a recorded port
	RunsChanged int
}

// replayRun is a run with at least one host the draft would answer differently, for the person-only
// drill-down.
type replayRun struct {
	RunID     uuid.UUID
	Owner     string
	CreatedAt time.Time
	Hosts     []string
}

// replayResult is what the counts answer. Runs is for a person who may read the runs; the model never gets it.
// RunsInScope and RunsCounted are the store's: both leave out the excluded (assistant) runs. NotComparable
// is the runs with a host-list decision whose audit row recorded no port (older rows); the draft's answer
// for such an attempt is unknown, so it is never counted as refused, allowed or unchanged.
type replayResult struct {
	RunsInScope   int
	RunsCounted   int
	Truncated     bool
	WouldRefuse   int
	WouldAllow    int
	NotComparable int
	Hosts         []replayHost
	Runs          []replayRun
}

// replayCounts counts, for the runs the store read, how many would now be refused at a host and port they
// reached and how many refused today would now be allowed. Only the three host-list rule sources count.
// The store has already left the excluded runs out of the rows and of both totals, so nothing is dropped
// here. allowedUnder is the draft's whole host verdict for one attempt, host and port, because the proxy
// matches a port-qualified allow or deny entry by both (TOOLS builds it from the builtin evaluator's
// EvaluateHost); this function never reads a policy. A row with Port 0 has no recorded port and is not
// compared. Display hosts are aggregated only after every attempt was compared.
func replayCounts(rows []store.RunHostDecision, totals store.ReplayTotals, allowedUnder func(host string, port int) bool) replayResult {
	res := replayResult{RunsInScope: totals.RunsInScope, RunsCounted: totals.RunsCounted, Truncated: totals.Truncated}
	type hostAcc struct {
		runs                  map[uuid.UUID]bool
		allow, deny           bool
		underAllow, underDeny bool
		changed               map[uuid.UUID]bool
	}
	type runAcc struct {
		run                         replayRun
		refuse, open, notComparable bool
		hosts                       map[string]bool
	}
	hosts := map[string]*hostAcc{}
	runs := map[uuid.UUID]*runAcc{}
	for _, r := range rows {
		var allowedToday bool
		switch r.RuleSource {
		case egress.RuleSourcePolicyAllowed:
			allowedToday = true
		case egress.RuleSourceDefaultDeny, egress.RuleSourcePolicyDenied:
		default:
			continue
		}
		h := hosts[r.Host]
		if h == nil {
			h = &hostAcc{runs: map[uuid.UUID]bool{}, changed: map[uuid.UUID]bool{}}
			hosts[r.Host] = h
		}
		h.runs[r.RunID] = true
		if allowedToday {
			h.allow = true
		} else {
			h.deny = true
		}
		ra := runs[r.RunID]
		if ra == nil {
			ra = &runAcc{run: replayRun{RunID: r.RunID, Owner: r.Owner, CreatedAt: r.CreatedAt}, hosts: map[string]bool{}}
			runs[r.RunID] = ra
		}
		if r.Port <= 0 {
			ra.notComparable = true
			continue
		}
		allowedNow := allowedUnder(r.Host, r.Port)
		if allowedNow {
			h.underAllow = true
		} else {
			h.underDeny = true
		}
		if allowedNow != allowedToday {
			h.changed[r.RunID] = true
			ra.hosts[r.Host] = true
			if allowedToday {
				ra.refuse = true
			} else {
				ra.open = true
			}
		}
	}
	for host, h := range hosts {
		today := "mixed"
		switch {
		case h.allow && !h.deny:
			today = "allow"
		case h.deny && !h.allow:
			today = "deny"
		}
		under := "unknown"
		switch {
		case h.underAllow && h.underDeny:
			under = "mixed"
		case h.underAllow:
			under = "allow"
		case h.underDeny:
			under = "deny"
		}
		res.Hosts = append(res.Hosts, replayHost{Host: host, Runs: len(h.runs), Today: today, Under: under, RunsChanged: len(h.changed)})
	}
	slices.SortFunc(res.Hosts, func(a, b replayHost) int { return cmp.Compare(a.Host, b.Host) })
	for _, ra := range runs {
		if ra.notComparable {
			res.NotComparable++
		}
		if ra.refuse {
			res.WouldRefuse++
		}
		if ra.open {
			res.WouldAllow++
		}
		if len(ra.hosts) == 0 {
			continue
		}
		for h := range ra.hosts {
			ra.run.Hosts = append(ra.run.Hosts, h)
		}
		slices.Sort(ra.run.Hosts)
		res.Runs = append(res.Runs, ra.run)
	}
	slices.SortFunc(res.Runs, func(a, b replayRun) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.RunID.String(), b.RunID.String())
	})
	return res
}
