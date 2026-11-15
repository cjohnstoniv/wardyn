// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// The stop scrub's bounds for ending the agent account's processes: each pass, each of the two launchd
// sign-outs inside it, and the pause between kills. The host waits 60 s for the whole stop scrub.
const (
	endAgentWait = 20 * time.Second
	bootoutWait  = 5 * time.Second
	endPassGap   = 50 * time.Millisecond
)

// stopScrub runs scrub between two passes of end, which ends every process of the agent account and says
// how many were running (Server.EndAgent). A program alive across the scrub can write back what it
// removed, so the report is done only when the first pass ended them all and the second found none, which
// would have started during the scrub. Otherwise it is failed, and the host records the machine as not
// scrubbed. With no end it is failed too. The scrub runs either way, to remove what it can.
func stopScrub(ctx context.Context, end func(context.Context) (int, error), scrub func(context.Context, string) ScrubReport) ScrubReport {
	pass := func() (int, error) {
		if end == nil {
			return 0, errors.New("this agent cannot end the agent account's processes")
		}
		ctx, cancel := context.WithTimeout(ctx, endAgentWait)
		defer cancel()
		return end(ctx)
	}
	_, before := pass()
	r := scrub(ctx, ScrubAtStop)
	running, after := pass()
	if before != nil || after != nil || running > 0 {
		slog.Warn("guest agent: the stop scrub is failed: the agent account's processes were not all ended around it",
			"before", before, "after", after, "running_after", running)
		r.Outcome = ScrubFailed
	}
	return r
}

// endAccount ends every process of the account uid and says how many were running. When any is, it first
// signs the account out through launchd (bootout of its gui and user domains), which ends the desktop
// session and starts none of its apps or agents again, then kills what list still names, again until it
// names none. A bootout that fails is no failure, since the kills are what count; endAccount fails when
// list fails or ctx ends while a process is left.
func endAccount(ctx context.Context, run CmdRunner, uid uint32, list func() ([]int, error), kill func(pid int) error) (int, error) {
	pids, err := list()
	if err != nil || len(pids) == 0 {
		return 0, err
	}
	n := len(pids)
	id := strconv.FormatUint(uint64(uid), 10)
	for _, domain := range []string{"gui/", "user/"} {
		// Each domain has its own bound: a sign-out that hangs must not cost the other its turn.
		bctx, cancel := context.WithTimeout(ctx, bootoutWait)
		_, err := run.Run(bctx, "/bin/launchctl", "bootout", domain+id)
		cancel()
		if err != nil {
			slog.Info("guest agent: stop scrub sign-out", "domain", domain+id, "err", err)
		}
	}
	for {
		if pids, err = list(); err != nil || len(pids) == 0 {
			return n, err
		}
		if ctx.Err() != nil {
			return n, fmt.Errorf("%d processes of the agent account did not end: %w", len(pids), ctx.Err())
		}
		for _, pid := range pids {
			_ = kill(pid) // one that has ended already is not listed again
		}
		select {
		case <-ctx.Done():
		case <-time.After(endPassGap):
		}
	}
}

// procZombie is SZOMB of <sys/proc.h>: a process that has ended and waits for its parent to collect it.
const procZombie = 5

// ofAccount says whether a process in state stat with these real, effective and saved uids is a live
// process of the account uid. A process the account started through a setuid program keeps its real uid,
// so it counts; a zombie has ended and does not.
func ofAccount(stat int8, ruid, euid, svuid, uid uint32) bool {
	return stat != procZombie && (ruid == uid || euid == uid || svuid == uid)
}

// keepEnded ends the agent account again every endPassGap until ctx ends, which is when the guest goes
// down. The stop scrub has answered by then, so a program that starts after its second pass (a sign-in, a
// launchd domain that would not sign out, a scheduled job) would run until the power-off on a disk
// recorded as scrubbed: it is ended as soon as it is seen.
func keepEnded(ctx context.Context, end func(context.Context) (int, error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(endPassGap):
		}
		pctx, cancel := context.WithTimeout(ctx, endAgentWait)
		n, err := end(pctx)
		cancel()
		switch {
		case n > 0:
			slog.Warn("guest agent: a program of the agent account ran after the stop scrub", "running", n, "err", err)
		case err != nil && ctx.Err() == nil:
			slog.Warn("guest agent: could not check or end the agent account after the stop scrub", "running", n, "err", err)
		}
	}
}
