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
