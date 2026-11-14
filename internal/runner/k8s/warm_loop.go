// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"context"
	"time"
)

// warmInterval is how often each replica reconciles the warm pools. Replicas need no leader: names are fixed and every
// delete names the UID it read, so two passes at once converge. A var only so tests can shorten it.
var warmInterval = 10 * time.Second

// startWarmPools runs reconcileWarmPools now and every warmInterval until Close. With no pools it runs one pass, which
// removes placeholders an earlier configuration left, and stops.
func (d *Driver) startWarmPools() {
	if d.runnerMode() {
		d.warm.stop = func() {}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d.warm.stop = func() {
		cancel()
		<-done
	}
	interval := warmInterval
	go func() {
		defer close(done)
		d.reconcileWarmPools(ctx)
		if len(d.cfg.WarmPools) == 0 {
			return
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.reconcileWarmPools(ctx)
			}
		}
	}()
}
