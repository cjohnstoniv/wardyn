// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runScheduleFlags are `wardyn run --start-at` and `--time-zone`: a one-time start later.
type runScheduleFlags struct {
	startAt, zone string
}

func (f *runScheduleFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.startAt, "start-at", "", "start the run later, once: a date and time such as 2026-10-21T02:00 read in --time-zone, or an RFC 3339 time, in the future and at most 7 days ahead. The run waits as QUEUED until then; cancel it with 'wardyn run kill'. Needs WARDYN_SCHEDULED_RUNS on the server. Not with --wait")
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
