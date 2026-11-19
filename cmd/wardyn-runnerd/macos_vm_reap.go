// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// reapSignal and reapGrace are the reap's signal and its wait between SIGTERM and SIGKILL (tests).
var (
	reapSignal = syscall.Kill
	reapGrace  = 10 * time.Second
)

// orphanHelper is a helper process the reap ends: its pid and start, and the entry and sub-command its
// argv names.
type orphanHelper struct {
	pid        int
	start      int64
	entry, sub string
}

// reapOrphanHelpers runs once at start, before the first recompute. It ends the job helpers an earlier
// runnerd of this runner started that never claimed their ledger entry. Such a helper builds no VM, but
// while it lives the ledger releases no dead entry of this user (it may be that entry's VM), so those
// entries keep both VM slots. A process is ended only when all of these hold:
//   - it is this user's, named as the helper, and started before this runnerd;
//   - its argv is update-tools or prepare with this runner's --runner-id and a --ledger-entry (an argv that
//     cannot be read keeps it: the ledger's fail-closed count stays);
//   - that entry is gone, or is not claimed by this very process (pid and start) and its owner is gone.
//
// A helper that claimed its entry is the VM's and is adopted, as before. Each one gets SIGTERM; after
// reapGrace the process table and the ledger are read again: a helper that claimed its entry meanwhile
// is kept, one that is no longer the same process is left alone, and the rest get SIGKILL. A read that
// fails kills none. Then the finished entries are released.
func (m *macOSVMs) reapOrphanHelpers(ctx context.Context) {
	if m == nil || m.ledger == nil {
		return
	}
	procs, err := m.host.Processes()
	if err != nil {
		slog.Warn("macOS VM: read the process table for helpers a previous runner left", "error", err.Error())
		return
	}
	self := slices.IndexFunc(procs, func(p admission.Proc) bool { return p.PID == os.Getpid() })
	counted, err := m.ledger.Counted()
	if err != nil {
		slog.Warn("macOS VM: no helpers a previous runner left were looked for: the ledger could not be read", "error", err.Error())
		return
	}
	if self < 0 {
		slog.Warn("macOS VM: no helpers a previous runner left were looked for: this process is not in the process table")
		return
	}
	var ended []orphanHelper
	for _, p := range procs {
		o, ok := m.orphan(p, procs[self], procs, counted)
		if !ok {
			continue
		}
		if err := reapSignal(o.pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			slog.Warn("macOS VM: end a helper a previous runner left", "pid", o.pid, "error", err.Error())
			continue
		}
		ended = append(ended, o)
	}
	if len(ended) == 0 {
		return
	}
	m.waitOrphans(ctx, ended)
	// SIGKILL only on a fresh read of both: the process table (the same pid, start and argv, so a pid
	// reused meanwhile is never hit) and the ledger (still unclaimed). Either read failing kills none;
	// the next start looks again.
	procs, err = m.host.Processes()
	if err == nil {
		counted, err = m.ledger.Counted()
	}
	if err != nil {
		slog.Warn("macOS VM: helpers a previous runner left were not killed: they could not be checked again", "error", err.Error())
		m.reconcilePrepare()
		return
	}
	for _, o := range ended {
		live, err := m.stillOrphan(procs, o)
		switch {
		case err != nil:
			slog.Warn("macOS VM: a helper a previous runner left was not killed: it could not be checked again", "pid", o.pid, "error", err.Error())
			continue
		case !live:
		case claimedBy(counted, o):
			slog.Info("macOS VM: kept a helper a previous runner left: it took its ledger entry", "pid", o.pid, "entry", o.entry)
			continue
		default:
			if err := reapSignal(o.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				slog.Warn("macOS VM: kill a helper a previous runner left", "pid", o.pid, "error", err.Error())
				continue
			}
		}
		slog.Warn("macOS VM: ended a helper a previous runner left", "pid", o.pid, "entry", o.entry, "subcommand", o.sub)
	}
	m.reconcilePrepare()
}

// orphan is reapOrphanHelpers' test for one process p, given this runnerd's own process self.
func (m *macOSVMs) orphan(p, self admission.Proc, procs []admission.Proc, counted []admission.Entry) (orphanHelper, bool) {
	if p.UID != m.uid || p.PID == self.PID || !strings.HasPrefix(p.Name, admission.HelperProcess) || !startedBefore(p, self) {
		return orphanHelper{}, false
	}
	argv, err := admission.Args(m.host, p.PID)
	if err != nil {
		return orphanHelper{}, false
	}
	sub, runner, entry := parseJobArgv(argv)
	if (sub != macvm.UpdateToolsCommand && sub != macvm.PrepareCommand) || runner != m.runnerID || entry == "" {
		return orphanHelper{}, false
	}
	o := orphanHelper{pid: p.PID, start: p.Start, entry: entry, sub: sub}
	if i := slices.IndexFunc(counted, func(e admission.Entry) bool { return e.ID == entry }); i >= 0 {
		// HelperPID 0 is never claimed; another HelperPID is another process's claim, so this one is
		// still unclaimed. Either way the entry's owner must be gone.
		e := counted[i]
		if !m.mine(e) || claimedBy(counted, o) || procAlive(procs, e.OwnerPID, e.OwnerStart) {
			return orphanHelper{}, false
		}
	}
	return o, true
}

// startedBefore: p was created before self, by mach absolute time when both are known (no clock change
// moves it), else by start time.
func startedBefore(p, self admission.Proc) bool {
	if p.MonoStart != 0 && self.MonoStart != 0 {
		return p.MonoStart < self.MonoStart
	}
	return p.Start < self.Start
}

// claimedBy: o's entry records o itself (pid and start) as its helper.
func claimedBy(counted []admission.Entry, o orphanHelper) bool {
	return slices.ContainsFunc(counted, func(e admission.Entry) bool {
		return e.ID == o.entry && e.HelperPID == o.pid && e.HelperStart == o.start
	})
}

func procAlive(procs []admission.Proc, pid int, start int64) bool {
	return slices.ContainsFunc(procs, func(p admission.Proc) bool { return p.PID == pid && p.Start == start })
}

// waitOrphans waits until reapGrace ends, ctx ends, or a read of the process table shows none of the
// helpers. A read that fails proves nothing, so the wait goes on.
func (m *macOSVMs) waitOrphans(ctx context.Context, hs []orphanHelper) {
	deadline := time.Now().Add(reapGrace)
	for {
		if procs, err := m.host.Processes(); err == nil &&
			!slices.ContainsFunc(hs, func(o orphanHelper) bool { return procAlive(procs, o.pid, o.start) }) {
			return
		}
		if !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(min(50*time.Millisecond, reapGrace/4)):
		}
	}
}

// stillOrphan: o is, in procs, still the same process (pid and start), this user's, named as the
// helper, with the argv it was ended for. An error when its argv cannot be read again.
func (m *macOSVMs) stillOrphan(procs []admission.Proc, o orphanHelper) (bool, error) {
	i := slices.IndexFunc(procs, func(p admission.Proc) bool { return p.PID == o.pid && p.Start == o.start })
	if i < 0 || procs[i].UID != m.uid || !strings.HasPrefix(procs[i].Name, admission.HelperProcess) {
		return false, nil
	}
	argv, err := admission.Args(m.host, o.pid)
	if err != nil {
		return false, err
	}
	sub, runner, entry := parseJobArgv(argv)
	return sub == o.sub && runner == m.runnerID && entry == o.entry, nil
}

// parseJobArgv reads a helper's sub-command (argv[1]) and its --runner-id and --ledger-entry values, in
// each form Go's flag package takes (-name value, --name value, -name=value, --name=value).
func parseJobArgv(argv []string) (sub, runner, entry string) {
	if len(argv) < 2 {
		return "", "", ""
	}
	for i := 2; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
		if !strings.HasPrefix(a, "-") || (name != macvm.FlagRunnerID && name != macvm.FlagLedgerEntry) {
			continue
		}
		if !hasVal {
			if i+1 >= len(argv) {
				break
			}
			i++
			val = argv[i]
		}
		if name == macvm.FlagRunnerID {
			runner = val
		} else {
			entry = val
		}
	}
	return argv[1], runner, entry
}
