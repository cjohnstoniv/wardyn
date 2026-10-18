// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// awsSSORefreshFlags are the AWS SSO renewal tuning (docs/ENV.md), api.Config.AWSSSORefresh. A bootFlags
// built without registering them, as a test's is, reads every default.
type awsSSORefreshFlags struct {
	skew, serveFloor, retryDelay, timeout *time.Duration
}

func registerAWSSSORefreshFlags() awsSSORefreshFlags {
	d := api.DefaultAWSSSORefreshTuning()
	return awsSSORefreshFlags{
		skew:       flagDuration("aws-sso-refresh-skew", "WARDYN_AWS_SSO_REFRESH_SKEW", d.Skew, "renew a captured AWS SSO session this long before its access token expires, at dispatch and for a live run (duration, 5m to 30m)"),
		serveFloor: flagDuration("aws-sso-refresh-serve-floor", "WARDYN_AWS_SSO_REFRESH_SERVE_FLOOR", d.ServeFloor, "the validity an AWS SSO access token must keep to be used while another renewal of it is in flight (duration, 30s to 10m, below the skew)"),
		retryDelay: flagDuration("aws-sso-refresh-retry-delay", "WARDYN_AWS_SSO_REFRESH_RETRY_DELAY", d.RetryDelay, "the pause before an AWS SSO renewal's one retry after a throttle or a network failure (duration, 100ms to 10s)"),
		timeout:    flagDuration("aws-sso-refresh-timeout", "WARDYN_AWS_SSO_REFRESH_TIMEOUT", d.Timeout, "the bound on one AWS SSO token request (duration, 2s to 60s)"),
	}
}

// tuning is the flags as api.Config.AWSSSORefresh; an unregistered flag keeps its default.
func (f awsSSORefreshFlags) tuning() api.AWSSSORefreshTuning {
	d := api.DefaultAWSSSORefreshTuning()
	or := func(p *time.Duration, def time.Duration) time.Duration {
		if p == nil {
			return def
		}
		return *p
	}
	return api.AWSSSORefreshTuning{Skew: or(f.skew, d.Skew), ServeFloor: or(f.serveFloor, d.ServeFloor),
		RetryDelay: or(f.retryDelay, d.RetryDelay), Timeout: or(f.timeout, d.Timeout)}
}

// awsSSORefreshBound is one variable's accepted range.
type awsSSORefreshBound struct {
	env      string
	value    func(api.AWSSSORefreshTuning) time.Duration
	min, max time.Duration
}

// awsSSORefreshBounds: the skew stays at or above the proxy's 5-minute re-resolve margin and the
// JavaScript SDK's own 5-minute refresh window, and well under a one-hour access token, past which every
// renewal would fall due again at once; the floor covers a sandbox create and a first model call; the
// retry and the timeout keep a renewal from stalling a launch for long.
var awsSSORefreshBounds = []awsSSORefreshBound{
	{"WARDYN_AWS_SSO_REFRESH_SKEW", func(t api.AWSSSORefreshTuning) time.Duration { return t.Skew }, 5 * time.Minute, 30 * time.Minute},
	{"WARDYN_AWS_SSO_REFRESH_SERVE_FLOOR", func(t api.AWSSSORefreshTuning) time.Duration { return t.ServeFloor }, 30 * time.Second, 10 * time.Minute},
	{"WARDYN_AWS_SSO_REFRESH_RETRY_DELAY", func(t api.AWSSSORefreshTuning) time.Duration { return t.RetryDelay }, 100 * time.Millisecond, 10 * time.Second},
	{"WARDYN_AWS_SSO_REFRESH_TIMEOUT", func(t api.AWSSSORefreshTuning) time.Duration { return t.Timeout }, 2 * time.Second, time.Minute},
}

// validate refuses a value outside its bound, and a serve floor that is not below the skew: every
// caller that weighs the floor is already inside the skew, so a floor at or above it would make every
// such caller wait on a renewal in flight.
func (f awsSSORefreshFlags) validate() error {
	t := f.tuning()
	for _, b := range awsSSORefreshBounds {
		if v := b.value(t); v < b.min || v > b.max {
			return fmt.Errorf("refusing to start: %s is %s; it must be between %s and %s", b.env, v, b.min, b.max)
		}
	}
	if t.ServeFloor >= t.Skew {
		return fmt.Errorf("refusing to start: WARDYN_AWS_SSO_REFRESH_SERVE_FLOOR (%s) must be below WARDYN_AWS_SSO_REFRESH_SKEW (%s)", t.ServeFloor, t.Skew)
	}
	return nil
}
