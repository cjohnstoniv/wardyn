// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerspool

import (
	"context"
	"testing"
)

// Full is true exactly where nextGrant stops issuing credit: the last record slot and the loss
// reserve are kept back, on rows and on bytes alike.
func TestFullIsWhereCreditStops(t *testing.T) {
	ctx := context.Background()
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
		{"bytes with room for one envelope", 1, MaxBytes - LossReserveBytes - grantRecordEnvelope - 1, false},
		{"bytes at the loss reserve", 1, MaxBytes - LossReserveBytes - grantRecordEnvelope, true},
		{"bytes at the ceiling", 1, MaxBytes, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := testSpool(t)
			if err := s.locked(ctx, func() error {
				m, err := s.readManifest()
				if err != nil {
					return err
				}
				m.Last, m.Bytes = tc.last, tc.bytes
				return s.writeManifest(m)
			}); err != nil {
				t.Fatal(err)
			}
			if got, err := s.Full(ctx); err != nil || got != tc.want {
				t.Fatalf("Full = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
