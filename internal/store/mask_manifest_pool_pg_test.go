// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// Loading a manifest must not hold a pooled connection while the subject key is
// fetched: the key manager asks the same pool, so every caller inside the load
// would hold one connection and wait for another.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
)

// narrowPool opens a second pool of size conns on wide's database, every
// connection already dialled, as a running daemon's pool is.
func narrowPool(t *testing.T, wide *pgxpool.Pool, conns int32) *pgxpool.Pool {
	t.Helper()
	cfg := wide.Config()
	cfg.MaxConns = conns
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open a pool of %d: %v", conns, err)
	}
	t.Cleanup(p.Close)
	release := make(chan struct{})
	ready := make(chan error, conns)
	var workers sync.WaitGroup
	for range conns {
		workers.Go(func() {
			c, err := p.Acquire(t.Context())
			ready <- err
			if err == nil {
				<-release
				c.Release()
			}
		})
	}
	defer workers.Wait()
	defer close(release)
	for range conns {
		if err := <-ready; err != nil {
			t.Fatalf("warm the pool: %v", err)
		}
	}

	return p
}

// coldManifests is a fresh process's Manifests over p, its key manager on the same pool.
func coldManifests(p *pgxpool.Pool, k kek.KEK) *maskmanifest.Manifests {
	return maskmanifest.New(p, subjectkeytest.Manager(p, k), secretmask.NewRegistry())
}

// On a pool of one, a cold Covered answers true inside its deadline.
func TestPG_MaskManifest_LoadsOnAPoolOfOne(t *testing.T) {
	wide := runsPGPoolIsolated(t)
	k := localKEK(t)
	const owner = "alice@example.com"
	run := manifestRun(t, wide, owner)
	dispatched(t, newReplica(t, wide, k), run, owner, "value-on-a-pool-of-one")

	m := coldManifests(narrowPool(t, wide, 1), k)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if !m.Covered(ctx, run) {
		t.Fatalf("Covered on a pool of one = false after %s (deadline: %v)", time.Since(start).Round(time.Millisecond), ctx.Err())
	}
}

// poolWedgeCallers is how many concurrent cold Covered calls wedged a
// 10-connection pool before the key was fetched outside the transaction.
const poolWedgeCallers = 10

// That many concurrent cold Covered calls on a 10-connection pool all answer true.
func TestPG_MaskManifest_ConcurrentColdLoadsOnATenConnectionPool(t *testing.T) {
	wide := runsPGPoolIsolated(t)
	k := localKEK(t)
	const owner = "alice@example.com"
	writer := newReplica(t, wide, k)
	runs := make([]uuid.UUID, poolWedgeCallers)
	for i := range runs {
		runs[i] = manifestRun(t, wide, owner)
		dispatched(t, writer, runs[i], owner, "value-of-run-"+runs[i].String())
	}

	m := coldManifests(narrowPool(t, wide, 10), k)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := make(chan struct{})
	got := make([]bool, len(runs))
	var wg sync.WaitGroup
	for i, run := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ready
			got[i] = m.Covered(ctx, run)
		}()
	}
	close(ready)
	wg.Wait()
	for i, ok := range got {
		if !ok {
			t.Errorf("caller %d of %d: Covered = false (deadline: %v)", i, len(runs), ctx.Err())
		}
	}
}
