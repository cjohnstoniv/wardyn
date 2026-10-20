// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"testing"
)

// Full is true exactly where nextGrant stops issuing usable credit (a record, or the smallest
// request's bytes), on rows and on bytes alike. The byte-bound states are the ones where each granted
// record's envelope eats the byte credit long before the spool's last bytes are used.
func TestFullIsWhereCreditStops(t *testing.T) {
	ctx := context.Background()
	const envelopes = GrantRecords * grantRecordEnvelope // held back while 5000 records are grantable
	for _, tc := range []struct {
		name  string
		last  int64
		bytes int64
		want  bool
	}{
		{"empty", 0, 0, false},
		{"one slot left for rows", MaxRecords - 2, 0, false},
		{"only the loss row's slot left", MaxRecords - 1, 0, true},
		{"rows at the ceiling", MaxRecords, 0, true},
		{"bytes: room for one minimal request", 1, MaxBytes - LossReserveBytes - envelopes - MinRequestReservation, false},
		{"bytes: one byte short of it", 1, MaxBytes - LossReserveBytes - envelopes - MinRequestReservation + 1, true},
		{"bytes: room for one envelope only", 1, MaxBytes - LossReserveBytes - grantRecordEnvelope - 1, true},
		{"bytes: 1 MiB left, 1000 rows", 1000, MaxBytes - LossReserveBytes - (1 << 20), true},
		{"bytes: 2.5 MB left, 20000 rows", 20000, MaxBytes - LossReserveBytes - 2_500_000, true},
		{"bytes at the ceiling", 1, MaxBytes, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := testSpool(t)
			var m manifest
			if err := s.locked(ctx, func() error {
				var err error
				if m, err = s.readManifest(); err != nil {
					return err
				}
				m.Last, m.Bytes = tc.last, tc.bytes
				return s.writeManifest(m)
			}); err != nil {
				t.Fatal(err)
			}
			got, err := s.Full(ctx)
			if err != nil || got != tc.want {
				t.Fatalf("Full = %v, %v; want %v", got, err, tc.want)
			}
			// Agreement with the grant a session would be given: no records, or too few bytes for the
			// smallest request, is exactly a full spool.
			m.Producer = &Producer{}
			g := nextGrant(m)
			if grantFull := g.Through-g.Base < 1 || g.Bytes < MinRequestReservation; grantFull != got {
				t.Fatalf("Full = %v but nextGrant gives %d records and %d bytes", got, g.Through-g.Base, g.Bytes)
			}
		})
	}
}
