// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"
)

// The values that used to panic after boot are refused with the setting named; every value that
// worked before, a zero buffer included, still passes.
func TestCheckTuningRefusesOnlyWhatWouldCrash(t *testing.T) {
	ok := func(h, r, s time.Duration, b int) error { return checkTuning(h, r, s, b) }
	if err := ok(30*time.Second, 15*time.Second, time.Minute, 4096); err != nil {
		t.Errorf("defaults refused: %v", err)
	}
	if err := ok(time.Nanosecond, time.Nanosecond, time.Nanosecond, 0); err != nil {
		t.Errorf("small positive intervals and a zero buffer refused: %v", err)
	}
	for _, tc := range []struct {
		name string
		err  error
		env  string
	}{
		{"zero heartbeat", ok(0, time.Second, time.Second, 1), "WARDYN_GROUNDTRUTH_HEARTBEAT"},
		{"negative refresh", ok(time.Second, -time.Second, time.Second, 1), "WARDYN_GROUNDTRUTH_REFRESH"},
		{"zero stats", ok(time.Second, time.Second, 0, 1), "WARDYN_GROUNDTRUTH_STATS"},
		{"negative buffer", ok(time.Second, time.Second, time.Second, -1), "WARDYN_GROUNDTRUTH_BUFFER"},
	} {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.env) {
			t.Errorf("%s: err = %v, want a refusal naming %s", tc.name, tc.err, tc.env)
		}
	}
}
