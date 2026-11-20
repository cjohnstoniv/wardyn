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
// today, what the draft decides, and how many of those runs a different answer would change.
type replayHost struct {
	Host        string
	Runs        int
	Today       string // "allow", "deny" or "mixed"
	Under       string // "allow" or "deny"
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
type replayResult struct {
	RunsInScope int
	RunsCounted int
	Truncated   bool
	WouldRefuse int
	WouldAllow  int
	Hosts       []replayHost
	Runs        []replayRun
}

// replayCounts counts, for the runs the store read, how many would now be refused at a host they reached
// and how many refused today would now be allowed. Only the three host-list rule sources count. Rows of a
// run for which excluded is true are dropped first and RunsCounted shrinks by those runs (only a run that
// has rows can be named, so an excluded run with none stays in RunsCounted). allowedUnder is the draft's
// whole host verdict; this function never reads a policy.
func replayCounts(rows []store.RunHostDecision, totals store.ReplayTotals, allowedUnder func(host string) bool, excluded func(runID uuid.UUID) bool) replayResult {
	res := replayResult{RunsInScope: totals.RunsInScope, RunsCounted: totals.RunsCounted, Truncated: totals.Truncated}
	type hostAcc struct {
		runs        map[uuid.UUID]bool
		allow, deny bool
		changed     map[uuid.UUID]bool
	}
	type runAcc struct {
		run          replayRun
		refuse, open bool
		hosts        map[string]bool
	}
	hosts := map[string]*hostAcc{}
	runs := map[uuid.UUID]*runAcc{}
	dropped := map[uuid.UUID]bool{}
	for _, r := range rows {
		if excluded != nil && excluded(r.RunID) {
			dropped[r.RunID] = true
			continue
		}
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
		if allowedUnder(r.Host) != allowedToday {
			h.changed[r.RunID] = true
			ra.hosts[r.Host] = true
			if allowedToday {
				ra.refuse = true
			} else {
				ra.open = true
			}
		}
	}
	res.RunsCounted = max(0, res.RunsCounted-len(dropped))
	for host, h := range hosts {
		today := "mixed"
		switch {
		case h.allow && !h.deny:
			today = "allow"
		case h.deny && !h.allow:
			today = "deny"
		}
		under := "deny"
		if allowedUnder(host) {
			under = "allow"
		}
		res.Hosts = append(res.Hosts, replayHost{Host: host, Runs: len(h.runs), Today: today, Under: under, RunsChanged: len(h.changed)})
	}
	slices.SortFunc(res.Hosts, func(a, b replayHost) int { return cmp.Compare(a.Host, b.Host) })
	for _, ra := range runs {
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
