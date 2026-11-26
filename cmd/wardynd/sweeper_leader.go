// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// sweepFamilies names the sweep family (db.SweeperFamilies) of every sweep started with leaderGo. The sweeps in
// internal/api gated by the sweeper lease (beginLeaderSweep) are the runs family's: the lease is the leader's Join
// and Current, which are that family's.
var sweepFamilies = map[string]string{
	"run_queue.sweeper":        db.SweeperFamilyRuns,
	"run_output.sweeper":       db.SweeperFamilyRuns,
	"egress.reconcile":         db.SweeperFamilyRuns,
	"portal.lifecycle.sweeper": db.SweeperFamilyRuns,
	"credential.sweeper":       db.SweeperFamilyCredentials,
	"aws_sso.renewer":          db.SweeperFamilyCredentials,
	"service.grant.cleanup":    db.SweeperFamilyCredentials,
	"scim.purge.sweeper":       db.SweeperFamilyCredentials,
	"audit.retention":          db.SweeperFamilyAudit,
	"audit.delivery":           db.SweeperFamilyAudit,
	"approval.sweeper":         db.SweeperFamilyRecords,
	"recording.sweeper":        db.SweeperFamilyRecords,
	"governance.units.walk":    db.SweeperFamilyRecords,
}

// sweepFamily is the sweep family of the leader sweep name.
func sweepFamily(name string) (string, bool) {
	f, ok := sweepFamilies[name]
	return f, ok
}

// leaderGo starts a sweeper that must run on one replica. With a leader it runs
// fn with each term's context of the sweep's family (fn returns when the family
// is lost, and is started again by the next term this process wins); with a nil
// leader it runs fn once on ctx, which is every sweeper's behaviour before there
// was an election. A sweep with no family is refused and never runs. goSafe
// contains a panic, as for every other background goroutine.
func leaderGo(ctx context.Context, leader *db.SweeperLeader, name string, fn func(context.Context)) {
	go goSafe(name, func() {
		if leader == nil {
			fn(ctx)
			return
		}
		family, ok := sweepFamily(name)
		if !ok {
			slog.ErrorContext(ctx, "wardynd: a leader sweep has no sweep family; it does not run", slog.String("sweep", name))
			return
		}
		leader.Family(family).Go(ctx, fn)
	})
}

// sweeperHolder names this process in sweeper_families.holder: the host (the pod
// name under Kubernetes) and a per-process id, as the run watcher's owner does.
func sweeperHolder() string {
	host, _ := os.Hostname()
	return host + "/" + uuid.NewString()
}
