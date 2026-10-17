// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"
)

// Unregistered (a test's bootFlags) and zero both read as off; boot refuses a renewal cap above 24h
// or below zero, and a negative idle timeout.
func TestSessionLifeFlagsValidate(t *testing.T) {
	d := func(v time.Duration) *time.Duration { return &v }
	for _, tc := range []struct {
		name    string
		flags   sessionLifeFlags
		wantErr string
	}{
		{"unregistered", sessionLifeFlags{}, ""},
		{"off", sessionLifeFlags{renewCap: d(0), idleTimeout: d(0)}, ""},
		{"a 24h cap and an idle timeout", sessionLifeFlags{renewCap: d(24 * time.Hour), idleTimeout: d(time.Minute)}, ""},
		{"a cap above 24h", sessionLifeFlags{renewCap: d(24*time.Hour + time.Second)}, "WARDYN_OIDC_SESSION_RENEW_CAP is 24h0m1s"},
		{"a negative cap", sessionLifeFlags{renewCap: d(-time.Second)}, "WARDYN_OIDC_SESSION_RENEW_CAP"},
		{"a negative idle timeout", sessionLifeFlags{idleTimeout: d(-time.Second)}, "WARDYN_OIDC_SESSION_IDLE_TIMEOUT"},
	} {
		err := tc.flags.validate()
		if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.wantErr)
		}
	}
	var unset sessionLifeFlags
	if orZero(unset.silentReauth) || orZero(unset.renewCap) != 0 {
		t.Error("an unregistered flag did not read as off")
	}
}
