// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
)

// sweeperWatchdogEvery is how often the watchdog judges the sweep families this replica leads.
const sweeperWatchdogEvery = 30 * time.Second

// watchedSweeps names, per sweep family, the sweeps whose success only that family's leader records, so a stalled
// leader cannot be kept fresh by a follower's tick. The audit family has none: audit.retention and audit.delivery
// register no sweep health name, so the audit family has no watchdog. Not watched: RunSecret, whose followers record
// successes too, and IdleReaper, TerminalSandbox and OrphanedBuild, which tick on every replica under their own
// locks, in no family.
var watchedSweeps = map[string][]string{
	// run_queue.sweeper ticks RunQueue and run_output.sweeper ticks RunOutput, on the leader only. RunWatcher ticks on
	// every replica, but a follower's tick records its attempt only.
	db.SweeperFamilyRuns:        {sweephealth.RunQueue, sweephealth.RunOutput, sweephealth.RunWatcher},
	db.SweeperFamilyCredentials: {sweephealth.CredentialExpiry, sweephealth.AWSSSORenewal},
	db.SweeperFamilyRecords:     {sweephealth.ApprovalExpiry, sweephealth.RecordingRetention},
}

// watchedFamily is the part of a sweep family (db.SweeperFamily) the watchdog uses.
type watchedFamily interface {
	Held() (acquiredAt time.Time, ok bool)
	Join() (ctx context.Context, epoch int64, end func(), ok bool)
	Resign()
}

// startSweeperWatchdog gives up each sweep family this replica leads whose watched sweeps have all stalled, so
// another replica can take it while the rest keep running here. Only with a leader and a sweep health tracker.
func startSweeperWatchdog(ctx context.Context, leader *db.SweeperLeader, ticks *sweephealth.Tracker) {
	if leader == nil || ticks == nil {
		return
	}
	fams := map[string]watchedFamily{}
	for name := range watchedSweeps {
		fams[name] = leader.Family(name)
	}
	go goSafe("sweeper.watchdog", func() {
		t := time.NewTicker(sweeperWatchdogEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			sweeperWatchdogPass(ctx, fams, ticks, time.Now())
		}
	})
}

// sweeperWatchdogPass resigns each family of fams this replica leads whose every watched, registered sweep is
// stalled at now. A status that cannot be read is "cannot tell", and nothing is resigned on it.
func sweeperWatchdogPass(ctx context.Context, fams map[string]watchedFamily, ticks *sweephealth.Tracker, now time.Time) {
	statuses, err := ticks.Status(ctx)
	if err != nil {
		return
	}
	byName := map[string]sweephealth.Status{}
	for _, s := range statuses {
		byName[s.Name] = s
	}
	for name, f := range fams {
		acquired, ok := f.Held()
		if !ok || !familyStalled(watchedSweeps[name], byName, acquired, now) {
			continue
		}
		var epoch int64
		if _, e, end, ok := f.Join(); ok {
			epoch = e
			end()
		}
		slog.WarnContext(ctx, "wardynd: sweeper family stalled; giving it up", slog.String("family", name), slog.Int64("epoch", epoch))
		f.Resign()
	}
}

// familyStalled reports whether every one of watched that is registered has stalled: no success for
// StaleAfterIntervals of its interval, counted from the later of its last success and acquired, when this replica
// took the family. AttemptedAt is never read: followers write it too. With none registered nothing is judged.
func familyStalled(watched []string, statuses map[string]sweephealth.Status, acquired, now time.Time) bool {
	judged := 0
	for _, name := range watched {
		s, ok := statuses[name]
		if !ok || s.Interval <= 0 {
			continue
		}
		judged++
		ref := s.SucceededAt
		if acquired.After(ref) {
			ref = acquired
		}
		if now.Sub(ref) < sweephealth.StaleAfterIntervals*s.Interval {
			return false
		}
	}
	return judged > 0
}
