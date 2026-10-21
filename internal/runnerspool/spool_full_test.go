// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"testing"
)

const fullTestInstance = "0123456789abcdef0123456789abcdef"

// stageFull drives a spool through the sink's own calls: a session is accepted, the manifest is left
// holding last rows and bytes (the way a long offline stretch leaves it), the sink regrants, and the
// proxy's counted refusals arrive: before the regrant (against the older grant) and after it.
func stageFull(t *testing.T, last, bytes int64, refusedBefore, refusedAfter uint64) bool {
	t.Helper()
	ctx := context.Background()
	s, _, _ := testSpool(t)
	sess, err := s.Accept(ctx, fullTestInstance)
	if err != nil {
		t.Fatal(err)
	}
	if refusedBefore > 0 {
		if err := s.ProducerRefused(ctx, sess.Gen, fullTestInstance, refusedBefore); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.locked(ctx, func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		m.Last, m.Bytes = last, bytes
		return s.writeManifest(m)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Regrant(ctx, sess.Gen, fullTestInstance); err != nil {
		t.Fatal(err)
	}
	if refusedAfter > 0 {
		if err := s.ProducerRefused(ctx, sess.Gen, fullTestInstance, refusedBefore+refusedAfter); err != nil {
			t.Fatal(err)
		}
	}
	full, err := s.Full(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return full
}

// Full says the spool is full when the sink can grant no record, or when the proxy has been refused
// against the outstanding grant with less room left than the largest record a request can reserve.
// Where the stall starts depends on the request (a GET reserves its target; a scanned POST a whole
// record), so the byte states are told apart by the proxy's counted refusal, not by a constant.
func TestFullIsWhereTheProxyIsRefused(t *testing.T) {
	// room is the byte credit a regrant would issue while 5000 records are grantable.
	room := func(r int64) int64 { return MaxBytes - LossReserveBytes - GrantRecords*grantRecordEnvelope - r }
	for _, tc := range []struct {
		name          string
		last, bytes   int64
		before, after uint64
		want          bool
	}{
		{"empty and idle", 0, 0, 0, 0, false},
		{"empty, a burst refused on a big grant", 0, 0, 0, 1, false},
		{"rows: one slot left, none refused", MaxRecords - 2, 0, 0, 0, false},
		{"rows: only the loss row's slot left", MaxRecords - 1, 0, 0, 0, true},
		{"rows at the ceiling", MaxRecords, 0, 0, 0, true},
		{"bytes: tight but nobody refused", 10, room(MinRequestReservation + 100), 0, 0, false},
		{"bytes: tight and a request refused", 10, room(MinRequestReservation + 100), 0, 1, true},
		{"bytes: refused only against the grant before", 10, room(MinRequestReservation + 100), 1, 0, false},
		{"bytes: refused before and again after", 10, room(MinRequestReservation + 100), 1, 1, true},
		{"bytes: a record's worth left, refused", 10, room(MaxRecordBytes), 0, 1, false},
		{"bytes: just under a record's worth left, refused", 10, room(MaxRecordBytes - 1), 0, 1, true},
		{"bytes: nothing left, refused", 10, MaxBytes, 0, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stageFull(t, tc.last, tc.bytes, tc.before, tc.after); got != tc.want {
				t.Fatalf("Full = %v, want %v", got, tc.want)
			}
		})
	}
}
