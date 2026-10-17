// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// leaderGo starts a sweeper that must run on one replica. With a leader it runs
// fn with each term's context (fn returns when the lease is lost, and is
// started again by the next term this process wins); with a nil leader it runs
// fn once on ctx, which is every sweeper's behaviour before there was an
// election. goSafe contains a panic, as for every other background goroutine.
func leaderGo(ctx context.Context, leader *db.SweeperLeader, name string, fn func(context.Context)) {
	go goSafe(name, func() {
		if leader == nil {
			fn(ctx)
			return
		}
		leader.Go(ctx, fn)
	})
}

// sweeperHolder names this process in sweeper_leader.holder: the host (the pod
// name under Kubernetes) and a per-process id, as the run watcher's owner does.
func sweeperHolder() string {
	host, _ := os.Hostname()
	return host + "/" + uuid.NewString()
}
