// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// relayRecheckEvery is how often a running Docker runner asks the engine again whether it reaches the
// run relay. A relay found at startup can stop being reachable (a firewall rule, a changed Docker
// network); a run started then would fail inside its proxy.
const (
	relayRecheckEvery = 5 * time.Minute
	// relayRecheckBound bounds one re-check.
	relayRecheckBound = 2 * time.Minute
)

// relayWatch re-checks the run relay while serve runs. A change is recorded once (changed), and then
// sent (send) on each check until a send succeeds, so a report that failed is not lost and an unchanged
// state is not resent. check is the startup probe on the pair serve chose.
type relayWatch struct {
	check func(context.Context) error
	every time.Duration
	// proves says an error from check proves the relay is down; any other error means the probe could
	// not run, which says nothing about the relay.
	proves  func(error) bool
	changed func(ctx context.Context, reachable bool, err error)
	// send tells the organisation the current state; an error leaves it pending for the next check.
	send func(ctx context.Context) error
	lost bool
	// pending is a change not yet sent.
	pending bool
}

// run checks every w.every, and again whenever kick fires (a session came up), until ctx ends. The
// startup probe is the first check, so none runs at once.
func (w *relayWatch) run(ctx context.Context, kick <-chan struct{}) {
	tick := time.NewTicker(w.every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-kick:
		}
		w.once(ctx)
	}
}

// watchRelay starts w beside run and stops it with ctx.
func watchRelay(run func(context.Context) error, w relayWatch, kick <-chan struct{}) func(context.Context) error {
	return func(ctx context.Context) error {
		go w.run(ctx, kick)
		return run(ctx)
	}
}

func (w *relayWatch) once(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, relayRecheckBound)
	defer cancel()
	err := w.check(ctx)
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		return // shutting down is not a lost relay
	}
	if err != nil && !w.proves(err) {
		slog.WarnContext(ctx, "could not check whether the container engine reaches the run relay", "err", err)
	} else if lost := err != nil; lost != w.lost {
		w.lost, w.pending = lost, true
		w.changed(ctx, !lost, err)
	}
	if w.pending && w.send(ctx) == nil {
		w.pending = false
	}
}

// logRelayChange is relayWatch.report's log half: one named line per change.
func logRelayChange(ctx context.Context, reachable bool, err error, pair relayPair) {
	if reachable {
		slog.InfoContext(ctx, "run relay reachable again", "listen", pair.bind, "target", pair.target)
		return
	}
	slog.WarnContext(ctx, "run relay unreachable from the container engine; new runs cannot reach this organisation until it is back", "listen", pair.bind, "target", pair.target, "err", err)
}
