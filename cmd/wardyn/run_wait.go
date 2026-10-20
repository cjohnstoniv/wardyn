// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runScheduleFlags are `wardyn run --start-at` and `--time-zone`: a one-time start later.
type runScheduleFlags struct {
	startAt, zone string
}

func (f *runScheduleFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.startAt, "start-at", "", "start the run later, once: a date and time such as 2026-10-21T02:00 read in --time-zone, or an RFC 3339 time, in the future and at most 6 days ahead (6 days 23 hours for an interactive environment), so the run keeps its wait before the 7-day cap. The run waits as QUEUED until then; cancel it with 'wardyn run kill'. Needs WARDYN_SCHEDULED_RUNS on the server. Not with --wait")
	cmd.Flags().StringVar(&f.zone, "time-zone", "", "the IANA time zone --start-at is given in, such as Europe/London or UTC; required with --start-at")
}

// apply copies the flags into body after any --file document, so a document can be scheduled too.
func (f runScheduleFlags) apply(body *sdk.CreateRunRequest, wait bool) error {
	if f.startAt == "" && f.zone == "" {
		return nil
	}
	if wait {
		return errors.New("--start-at and --wait are mutually exclusive (a scheduled run starts later; follow it with 'wardyn run get')")
	}
	body.StartAt, body.StartTimeZone = f.startAt, f.zone
	return nil
}

// applyQueueWait sets the request's max_wait_sec from --max-wait or, with --wait, from --timeout. A
// --max-wait under a second is refused; a document's own max_wait_sec stands unless --max-wait is given.
func applyQueueWait(body *sdk.CreateRunRequest, given bool, maxWait time.Duration, wait bool, timeout time.Duration) error {
	if given && maxWait < time.Second {
		return fmt.Errorf("--max-wait must be at least 1s, got %s", maxWait)
	}
	if sec := queueWaitSec(maxWait, wait, timeout); sec != nil && (given || body.MaxWaitSec == nil) {
		body.MaxWaitSec = sec
	}
	return nil
}

// queueWaitSec is the max_wait_sec to send: --max-wait when given, else with --wait the --timeout minus
// 60s for a --timeout of 2m or more (so never under 60s), and half of --timeout (at least 1s) for a
// shorter one. The aim is for the server to end a run that waits too long (queue_timeout, exit 1)
// before the CLI gives up on it (exit 124); it is only the limit asked of the server. nil sends
// nothing and leaves the server's default.
func queueWaitSec(maxWait time.Duration, wait bool, timeout time.Duration) *int {
	d := maxWait
	switch {
	case d > 0:
	case wait:
		d = max(timeout-time.Minute, time.Minute)
		if timeout > 0 && timeout < 2*time.Minute {
			d = max(timeout/2, time.Second)
		}
	default:
		return nil
	}
	sec := int(d / time.Second)
	return &sec
}
