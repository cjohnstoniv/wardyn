// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runner_resources.go — `wardyn runner list`'s resource columns: what a runner's runs hold now over its
// total, and the most one run may use. CPUs are milli-CPU / 1000, memory and disk GB are MiB / 1024.

const resourceHeads = "CPU (use/total, per run)\tMEMORY GB\tDISK GB"

// resourceCells are RUNS, CPU, MEMORY GB and DISK GB for one runner, "-" in all four while it has no
// limits (it takes no run, so there is nothing to divide by).
func resourceCells(l *sdk.RunnerLimits, u sdk.RunnerUsage) [4]string {
	if l == nil {
		return [4]string{"-", "-", "-", "-"}
	}
	cell := func(use, total, per, unit int64) string {
		return amount(use, unit) + "/" + amount(total, unit) + ", " + amount(per, unit)
	}
	in := u.InUse
	return [4]string{fmt.Sprintf("%d/%d", in.Runs, l.RunsAtOnce),
		cell(in.CPUMillis, l.Total.CPUMillis, l.PerRun.CPUMillis, 1000),
		cell(in.MemoryMiB, l.Total.MemoryMiB, l.PerRun.MemoryMiB, 1024),
		cell(in.DiskMiB, l.Total.DiskMiB, l.PerRun.DiskMiB, 1024)}
}

// printOwnRunners is your own list: your self-hosted runners, then the remote-hosted ones you may add to
// your own user pools.
func printOwnRunners(w io.Writer, own []sdk.RunnerView, remote []sdk.RemoteRunnerView) error {
	tw := newTab(w)
	fmt.Fprintln(tw, "ID\tNAME\tKIND\tSTATE\tLAST SEEN\tFINGERPRINT\tRUNS\t"+resourceHeads)
	row := func(id, name, kind, state, lastSeen, fingerprint string, c [4]string) {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, name, kind, state, lastSeen, fingerprint, strings.Join(c[:], "\t"))
	}
	for _, r := range own {
		lastSeen := "never"
		if r.LastSeenAt != nil {
			lastSeen = r.LastSeenAt.Format(time.RFC3339)
		}
		row(r.ID.String(), r.Name, "self-hosted", runnerStateWord(r), lastSeen, r.KeyFingerprint, resourceCells(r.Limits, r.Usage))
	}
	for _, rr := range remote {
		state := "offline"
		if rr.Online {
			state = "online"
		}
		row(rr.ID, rr.Name, "remote-hosted", state, "-", "-", resourceCells(rr.Limits, rr.Usage))
	}
	return tw.Flush()
}

// remoteRunnersToAdd are the remote-hosted runners the caller may add to their pools. A server without
// pools answers 4xx or 501: your own runners still list, so that is no error.
func remoteRunnersToAdd(ctx context.Context, c *sdk.Client) ([]sdk.RemoteRunnerView, error) {
	out, err := c.ListMyRemoteRunners(ctx)
	var api *sdk.APIError
	if errors.As(err, &api) && (api.Status >= 400 && api.Status < 500 || api.Status == 501) {
		return nil, nil
	}
	return out, err
}
