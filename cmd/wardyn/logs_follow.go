// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// followSince is the first server release that answers a run's audit trail newest first (?order=desc), which
// `run logs --follow` reads by. An older server answers oldest first, so the follow would print the trail once and
// then nothing more.
const followSince = "0.9.4"

// logsFollowPage is how many events each read after a follow's first asks for. What a later read needs is at the
// front of the trail, newest first, and a page that is all new pages on by itself (logTail.poll), so a quiet read
// costs this many rows, not the server's default page of 1000. Zero sends no limit.
var logsFollowPage = 20

// logsMaxInterval is the longest a follow waits between reads while nothing new arrives: the wait doubles from
// --interval after each quiet read, and drops back to --interval once anything new arrives or the run has ended.
// A --interval above it is kept as it is.
var logsMaxInterval = 10 * time.Second

// logsSleep waits d, or until ctx ends; a var so a test can count the waits without taking them.
var logsSleep = func(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// checkFollowServer refuses a follow against a server too old to answer newest first. A version it cannot read
// (a failed /healthz, a development build) does not refuse: the check explains a known gap, it does not gate.
func checkFollowServer(ctx context.Context, c *sdk.Client) error {
	raw, err := c.Healthz(ctx)
	if err != nil {
		return nil
	}
	var h struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &h) != nil {
		return nil
	}
	release, _, _ := strings.Cut(h.Version, "+")
	if older(release, followSince) {
		return fmt.Errorf("the organisation runs %s, and following a run's log needs %s or later: run 'wardyn run logs --follow=false' to print what is there now", h.Version, followSince)
	}
	return nil
}

// logsPage is the paging of one read: a follow's first read takes the server's default page, every later one
// logsFollowPage.
func logsPage(later bool, offset int) sdk.ListOpts {
	if later {
		return sdk.ListOpts{Limit: logsFollowPage, Offset: offset}
	}
	return sdk.ListOpts{Offset: offset}
}

// nextLogsWait is the wait after a read: --interval when the read printed anything or the run has ended (its last
// lines are being drained), and otherwise twice the last wait, up to logsMaxInterval.
func nextLogsWait(last, interval time.Duration, fresh int, ended bool) time.Duration {
	if fresh > 0 || ended {
		return interval
	}
	return min(2*last, max(interval, logsMaxInterval))
}
