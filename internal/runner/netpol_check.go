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

// WorseNetPolCheck returns the worse of two checks, each keeping its own time. Between equal verdicts it
// returns the older check, so "last confirmed" never claims more than every substrate showed.
func WorseNetPolCheck(a, b NetPolCheck) NetPolCheck {
	ra, rb := netPolRank[a.Verdict], netPolRank[b.Verdict]
	switch {
	case ra != rb:
		if rb > ra {
			return b
		}
		return a
	case a.At.IsZero() || !b.At.IsZero() && b.At.Before(a.At):
		return b
	}
	return a
}
