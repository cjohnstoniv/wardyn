// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"
	"time"
)

// TestRunCapInputsAt: the cap is the earlier of the span and the elevation's end, the span on a tie, and the
// end is cut to it; a run with no end of its own ends at the cap only when EndAtCap says so. The span counts
// from the created_at given, so the same inputs at a created_at two minutes later pick the elevation instead.
func TestRunCapInputsAt(t *testing.T) {
	created := time.Date(2025, 1, 2, 12, 0, 0, 0, time.UTC)
	dur := func(d time.Duration) *time.Duration { return &d }
	at := func(d time.Duration) *time.Time { v := created.Add(d); return &v }
	for _, tc := range []struct {
		name     string
		in       RunCapInputs
		created  time.Time
		end, cap *time.Time
		source   LimitSource
	}{
		{"no bound", RunCapInputs{End: dur(time.Hour)}, created, at(time.Hour), nil, ""},
		{"span only", RunCapInputs{End: dur(time.Hour), Span: 2 * time.Hour, SpanSource: LimitSourcePool}, created, at(time.Hour), at(2 * time.Hour), LimitSourcePool},
		{"elevation only", RunCapInputs{End: dur(time.Hour), Elevation: at(30 * time.Minute)}, created, at(30 * time.Minute), at(30 * time.Minute), LimitSourceElevation},
		{"the span first", RunCapInputs{End: dur(3 * time.Hour), Span: 10 * time.Minute, SpanSource: LimitSourceDeployment, Elevation: at(11 * time.Minute)},
			created, at(10 * time.Minute), at(10 * time.Minute), LimitSourceDeployment},
		{"the elevation first", RunCapInputs{End: dur(3 * time.Hour), Span: 10 * time.Minute, SpanSource: LimitSourcePool, Elevation: at(9 * time.Minute)},
			created, at(9 * time.Minute), at(9 * time.Minute), LimitSourceElevation},
		{"a tie names the span", RunCapInputs{Span: 10 * time.Minute, SpanSource: LimitSourcePool, Elevation: at(10 * time.Minute)},
			created, nil, at(10 * time.Minute), LimitSourcePool},
		{"created two minutes later", RunCapInputs{End: dur(3 * time.Hour), Span: 10 * time.Minute, SpanSource: LimitSourceDeployment, Elevation: at(11 * time.Minute)},
			created.Add(2 * time.Minute), at(11 * time.Minute), at(11 * time.Minute), LimitSourceElevation},
		{"an end inside the cap counts from created", RunCapInputs{End: dur(5 * time.Minute), Span: 10 * time.Minute, SpanSource: LimitSourcePool},
			created.Add(2 * time.Minute), at(7 * time.Minute), at(12 * time.Minute), LimitSourcePool},
		{"an end inside the cap is kept", RunCapInputs{End: dur(5 * time.Minute), Span: 10 * time.Minute, SpanSource: LimitSourcePool, Elevation: at(9 * time.Minute)},
			created, at(5 * time.Minute), at(9 * time.Minute), LimitSourceElevation},
		{"No end keeps no end under a cap", RunCapInputs{Span: 10 * time.Minute, SpanSource: LimitSourcePool, Elevation: at(9 * time.Minute)},
			created, nil, at(9 * time.Minute), LimitSourceElevation},
		{"EndAtCap ends at the cap", RunCapInputs{Span: 10 * time.Minute, SpanSource: LimitSourceDeployment, Elevation: at(9 * time.Minute), EndAtCap: true},
			created, at(9 * time.Minute), at(9 * time.Minute), LimitSourceElevation},
		{"EndAtCap with no cap is No end", RunCapInputs{EndAtCap: true}, created, nil, nil, ""},
	} {
		end, capAt, source := tc.in.At(tc.created)
		if !sameInstant(end, tc.end) || !sameInstant(capAt, tc.cap) || source != tc.source {
			t.Errorf("%s: end %v, cap %v (%q); want %v, %v (%q)", tc.name, end, capAt, source, tc.end, tc.cap, tc.source)
		}
	}
}

func sameInstant(a, b *time.Time) bool {
	return (a == nil) == (b == nil) && (a == nil || a.Equal(*b))
}
