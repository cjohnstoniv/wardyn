// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"testing"
	"time"
)

// TestWorseNetPolCheck: the worse verdict wins with its own time; equal verdicts keep the older check;
// an empty or unknown verdict never wins.
func TestWorseNetPolCheck(t *testing.T) {
	t1 := time.Now().UTC().Add(-time.Hour)
	t2 := t1.Add(time.Minute)
	ok1, ok2 := NetPolCheck{NetPolEnforced, t1}, NetPolCheck{NetPolEnforced, t2}
	for _, tc := range []struct {
		name string
		a, b NetPolCheck
		want NetPolCheck
	}{
		{"worse wins with its own time", ok1, NetPolCheck{NetPolRefused, t2}, NetPolCheck{NetPolRefused, t2}},
		{"order does not matter", NetPolCheck{NetPolRefused, t2}, ok1, NetPolCheck{NetPolRefused, t2}},
		{"equal verdicts keep the older", ok2, ok1, ok1},
		{"none never wins", NetPolCheck{}, ok2, ok2},
		{"unknown never wins", NetPolCheck{"newer-word", t1}, ok2, ok2},
		{"not enforced under the override is worst", NetPolCheck{NetPolNotProven, t1}, NetPolCheck{NetPolUnenforced, t2}, NetPolCheck{NetPolUnenforced, t2}},
		{"acknowledged beside not proven is not proven", NetPolCheck{NetPolAcknowledged, t1}, NetPolCheck{NetPolNotProven, t2}, NetPolCheck{NetPolNotProven, t2}},
	} {
		if got := WorseNetPolCheck(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if KnownNetPolVerdict("") || KnownNetPolVerdict("newer-word") || !KnownNetPolVerdict(NetPolNotProven) {
		t.Error("KnownNetPolVerdict")
	}
}

// TestNetPolVerdictChanged: only a move between two known verdicts is a change.
func TestNetPolVerdictChanged(t *testing.T) {
	for _, tc := range []struct {
		prev, next string
		want       bool
	}{
		{NetPolEnforced, NetPolRefused, true},
		{NetPolRefused, NetPolEnforced, true},
		{NetPolEnforced, NetPolAcknowledged, true},
		{NetPolNotProven, NetPolUnenforced, true},
		{NetPolEnforced, NetPolEnforced, false},
		{NetPolRefused, NetPolRefused, false},
		{"", NetPolRefused, false},
		{NetPolEnforced, "", false},
		{"newer-word", NetPolEnforced, false},
		{NetPolEnforced, "newer-word", false},
		{"", "", false},
	} {
		if got := NetPolVerdictChanged(tc.prev, tc.next); got != tc.want {
			t.Errorf("NetPolVerdictChanged(%q, %q) = %v, want %v", tc.prev, tc.next, got, tc.want)
		}
	}
}

// TestNetPolAtLeast: a known verdict ranks against the floor; an empty or unknown one never reaches it.
func TestNetPolAtLeast(t *testing.T) {
	for _, tc := range []struct {
		v    string
		want bool
	}{
		{NetPolEnforced, false},
		{NetPolAcknowledged, false},
		{NetPolNotProven, true},
		{NetPolRefused, true},
		{NetPolUnenforced, true},
		{"", false},
		{"newer-word", false},
	} {
		if got := NetPolAtLeast(tc.v, NetPolNotProven); got != tc.want {
			t.Errorf("NetPolAtLeast(%q, not_proven) = %v, want %v", tc.v, got, tc.want)
		}
	}
	if !NetPolAtLeast(NetPolEnforced, NetPolEnforced) || NetPolAtLeast(NetPolEnforced, NetPolAcknowledged) {
		t.Error("NetPolAtLeast against enforced and acknowledged floors")
	}
}
