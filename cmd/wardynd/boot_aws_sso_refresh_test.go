// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// The four AWS SSO renewal variables default to today's values, reach api.Config through the
// environment, and boot refuses a value outside its bound or a serve floor not below the skew.
func TestAWSSSORefreshTuningBoot(t *testing.T) {
	envs := []string{"WARDYN_AWS_SSO_REFRESH_SKEW", "WARDYN_AWS_SSO_REFRESH_SERVE_FLOOR",
		"WARDYN_AWS_SSO_REFRESH_RETRY_DELAY", "WARDYN_AWS_SSO_REFRESH_TIMEOUT"}
	for _, e := range envs {
		ensureUnset(t, e)
	}
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"wardynd-test"}

	resetFlags(t)
	want := api.AWSSSORefreshTuning{Skew: 10 * time.Minute, ServeFloor: 2 * time.Minute, RetryDelay: 400 * time.Millisecond, Timeout: 10 * time.Second}
	if got := parseBootFlags().ssoRefresh.tuning(); got != want || got != api.DefaultAWSSSORefreshTuning() {
		t.Fatalf("defaults = %+v, want today's %+v", got, want)
	}
	if got := (awsSSORefreshFlags{}).tuning(); got != want {
		t.Fatalf("unregistered flags = %+v, want the defaults", got)
	}
	t.Setenv("WARDYN_AWS_SSO_REFRESH_SKEW", "20m")
	t.Setenv("WARDYN_AWS_SSO_REFRESH_SERVE_FLOOR", "3m")
	t.Setenv("WARDYN_AWS_SSO_REFRESH_RETRY_DELAY", "1s")
	t.Setenv("WARDYN_AWS_SSO_REFRESH_TIMEOUT", "30s")
	resetFlags(t)
	f := parseBootFlags()
	if got := f.ssoRefresh.tuning(); got != (api.AWSSSORefreshTuning{Skew: 20 * time.Minute, ServeFloor: 3 * time.Minute, RetryDelay: time.Second, Timeout: 30 * time.Second}) {
		t.Fatalf("env = %+v", got)
	}
	if err := validateBootPosture(f, tlsPosture{}); err != nil {
		t.Fatalf("in-bounds values refused at boot: %v", err)
	}

	d := func(v time.Duration) *time.Duration { return &v }
	for _, ok := range []awsSSORefreshFlags{
		{skew: d(5 * time.Minute), serveFloor: d(30 * time.Second)},
		{skew: d(30 * time.Minute), serveFloor: d(10 * time.Minute)},
		{retryDelay: d(100 * time.Millisecond), timeout: d(2 * time.Second)},
		{retryDelay: d(10 * time.Second), timeout: d(time.Minute)},
	} {
		if err := ok.validate(); err != nil {
			t.Errorf("%+v refused: %v", ok.tuning(), err)
		}
	}
	for _, bad := range []struct {
		f    awsSSORefreshFlags
		want string
	}{
		{awsSSORefreshFlags{skew: d(5*time.Minute - time.Second)}, envs[0]},
		{awsSSORefreshFlags{skew: d(30*time.Minute + time.Second)}, envs[0]},
		{awsSSORefreshFlags{skew: d(0)}, envs[0]},
		{awsSSORefreshFlags{serveFloor: d(29 * time.Second)}, envs[1]},
		{awsSSORefreshFlags{skew: d(30 * time.Minute), serveFloor: d(10*time.Minute + time.Second)}, envs[1]},
		{awsSSORefreshFlags{retryDelay: d(99 * time.Millisecond)}, envs[2]},
		{awsSSORefreshFlags{retryDelay: d(10*time.Second + time.Millisecond)}, envs[2]},
		{awsSSORefreshFlags{timeout: d(time.Second)}, envs[3]},
		{awsSSORefreshFlags{timeout: d(time.Minute + time.Second)}, envs[3]},
		{awsSSORefreshFlags{skew: d(5 * time.Minute), serveFloor: d(5 * time.Minute)}, "must be below " + envs[0]},
	} {
		if err := bad.f.validate(); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%+v: error = %v, want one naming %s", bad.f.tuning(), err, bad.want)
		}
	}

	t.Setenv("WARDYN_AWS_SSO_REFRESH_SKEW", "1m")
	resetFlags(t)
	if err := validateBootPosture(parseBootFlags(), tlsPosture{}); err == nil || !strings.Contains(err.Error(), envs[0]) {
		t.Errorf("boot with a 1m skew = %v, want a refusal naming the variable", err)
	}
}
