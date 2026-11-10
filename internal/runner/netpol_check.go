// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "time"

// NetPolCheck is a Kubernetes substrate's NetworkPolicy canary verdict and the time of the check that
// produced it: a later check that disagreed, but has not yet changed the verdict, never moves At.
// The zero value means the substrate reports none (not Kubernetes, or older than the re-check).
type NetPolCheck struct {
	Verdict string    `json:"verdict,omitempty"`
	At      time.Time `json:"at,omitzero"`
}

// NetPolCheck verdicts.
const (
	NetPolEnforced     = "enforced"     // a deny-all policy blocked the canary's connect
	NetPolAcknowledged = "acknowledged" // the operator accepted an ambient default-deny; never proof
	NetPolNotProven    = "not_proven"   // three checks in a row could not tell; new sandboxes are refused
	NetPolRefused      = "refused"      // checks showed it not enforced, and no override covers the new sandboxes; they are refused
	NetPolUnenforced   = "unenforced"   // not enforced, under WARDYN_K8S_ALLOW_UNENFORCED_NETPOL
)

// netPolRank orders the verdicts, worst highest. Not enforced under the override is worst: new
// sandboxes are still created unconfined. An unknown verdict ranks 0 and never wins.
var netPolRank = map[string]int{NetPolEnforced: 1, NetPolAcknowledged: 2, NetPolNotProven: 3, NetPolRefused: 4, NetPolUnenforced: 5}

// KnownNetPolVerdict reports whether v is one of the NetPolCheck verdicts.
func KnownNetPolVerdict(v string) bool { return netPolRank[v] > 0 }

// NetPolVerdictChanged reports whether a verdict moved from prev to next: both are verdicts this build
// knows, and they differ. A first verdict (prev empty) is a baseline, not a change, and a word this build
// does not know is never one.
func NetPolVerdictChanged(prev, next string) bool {
	return KnownNetPolVerdict(prev) && KnownNetPolVerdict(next) && prev != next
}

// NetPolAtLeast reports whether v is a known verdict at least as bad as floor.
func NetPolAtLeast(v, floor string) bool {
	return KnownNetPolVerdict(v) && netPolRank[v] >= netPolRank[floor]
}

// WorseNetPolCheck returns the worse of two checks, each keeping its own time. Between equal verdicts it
// returns the older check, so "last confirmed" never claims more than every substrate showed.
func WorseNetPolCheck(a, b NetPolCheck) NetPolCheck {
	if NetPolSecondWins(a, b) {
		return b
	}
	return a
}

// NetPolSecondWins reports whether WorseNetPolCheck(a, b) returns b: its verdict is worse, or the
// verdicts rank the same and b is the older check (or a has no time). A caller that must say whose
// check won (its verdict, time and source together) asks this rather than comparing verdicts.
func NetPolSecondWins(a, b NetPolCheck) bool {
	ra, rb := netPolRank[a.Verdict], netPolRank[b.Verdict]
	if ra != rb {
		return rb > ra
	}
	return a.At.IsZero() || !b.At.IsZero() && b.At.Before(a.At)
}
