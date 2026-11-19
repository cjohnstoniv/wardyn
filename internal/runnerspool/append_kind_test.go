// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fillManifest writes the manifest's record count and bytes, as a spool that holds them would show.
func fillManifest(t *testing.T, s *Spool, last, bytes int64) {
	t.Helper()
	if err := s.locked(context.Background(), func() error {
		m, err := s.readManifest()
		if err != nil {
			return err
		}
		m.Last, m.Bytes = last, bytes
		return s.writeManifest(m)
	}); err != nil {
		t.Fatal(err)
	}
}

// Only the producer's own loss row takes the spool's last record slot and LossReserveBytes. A record of a
// request that was not sent and finds no room outside them is a refused admission (Drops); one of a request
// that was sent is lost, which ErrFull reports and Drops never counts, or replay would read it as
// builtin:spool-full, a request never sent.
func TestOnlyTheLossRowTakesTheRoomKeptForIt(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		room func(row int64) (last, bytes int64)
	}{
		{"last record slot", func(int64) (int64, int64) { return MaxRecords - 1, 0 }},
		{"loss reserve", func(row int64) (int64, int64) { return 0, MaxBytes - LossReserveBytes - row + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, id := testSpool(t)
			d := decision(id) // one record, so the row measured is the row appended
			m, err := s.readManifest()
			if err != nil {
				t.Fatal(err)
			}
			r := Record{SpoolID: m.SpoolID, Seq: 1, Decision: d}
			r.Hash = r.digest()
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			last, bytes := tc.room(int64(len(b)))
			fillManifest(t, s, last, bytes)
			if err := s.AppendAdmitted(ctx, d); !errors.Is(err, ErrFull) {
				t.Fatalf("sent record = %v, want ErrFull: it may not take the loss row's room", err)
			}
			if _, _, drops, err := s.Stats(ctx); err != nil || drops != 0 {
				t.Fatalf("drops %d, %v after a sent record was lost; want none", drops, err)
			}
			if err := s.Append(ctx, d); !errors.Is(err, ErrFull) {
				t.Fatalf("unsent record = %v, want ErrFull: it may not take the loss row's room", err)
			}
			if err := s.AppendLoss(ctx, d); err != nil {
				t.Fatalf("loss row = %v, want it kept in the room left for it", err)
			}
			depth, _, drops, err := s.Stats(ctx)
			if err != nil || depth != int(last)+1 || drops != 1 {
				t.Fatalf("spool holds %d records and %d refusals, %v; want the loss row added and only the unsent record counted", depth, drops, err)
			}
		})
	}
}

// A sent record too large for any spool row is lost, not refused: ErrFull, and Drops stays empty.
func TestASentRecordTooLargeToSpoolIsNotARefusal(t *testing.T) {
	ctx := context.Background()
	s, _, id := testSpool(t)
	big := decision(id)
	big.Request.Path = strings.Repeat("x", MaxRecordBytes)
	if err := s.AppendAdmitted(ctx, big); !errors.Is(err, ErrFull) {
		t.Fatalf("too-large sent record = %v, want ErrFull", err)
	}
	if over, err := s.Overflow(ctx); err != nil || over != nil {
		t.Fatalf("overflow row %+v, %v; want none for a request that was sent", over, err)
	}
	if err := s.AppendLoss(ctx, big); !errors.Is(err, ErrFull) {
		t.Fatalf("too-large loss row = %v, want ErrFull", err)
	}
	if depth, _, drops, err := s.Stats(ctx); err != nil || depth != 0 || drops != 0 {
		t.Fatalf("spool holds %d records and %d refusals, %v; want neither", depth, drops, err)
	}
}
