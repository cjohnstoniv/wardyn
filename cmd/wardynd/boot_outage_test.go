// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestOutageFlagsRefuseOutOfRangeValues(t *testing.T) {
	d := func(v time.Duration) *time.Duration { return &v }
	for _, tc := range []struct {
		name    string
		flags   outageFlags
		wantErr string
	}{
		{"unregistered", outageFlags{}, ""},
		{"grace off", outageFlags{lapseRecoveryGrace: d(0)}, ""},
		{"grace 25m", outageFlags{lapseRecoveryGrace: d(25 * time.Minute)}, ""},
		{"grace 2h", outageFlags{lapseRecoveryGrace: d(2 * time.Hour)}, ""},
		{"grace negative", outageFlags{lapseRecoveryGrace: d(-time.Second)}, "WARDYN_LAPSE_RECOVERY_GRACE is -1s"},
		{"grace above 2h", outageFlags{lapseRecoveryGrace: d(2*time.Hour + time.Minute)}, "WARDYN_LAPSE_RECOVERY_GRACE is 2h1m0s"},
	} {
		err := tc.flags.validate()
		if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}

// The store-health tracker is built and started before the API server, and the server is handed it.
func TestStoreHealthTrackerIsWired(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	run := strings.Index(src, "go health.Run(")
	api := strings.Index(src, "api.New(")
	if run < 0 || api < 0 || run > api {
		t.Fatalf("go health.Run( must appear before api.New( (at %d and %d)", run, api)
	}
	if !strings.Contains(src, "StoreHealth:") {
		t.Fatal("api.Config must carry StoreHealth:")
	}
}
