// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// A bench of 50 concurrent run start locks on distinct runs, each held 500 ms, through a locker of 8 slots: how many
// are refused, how many holds end lost, and the take's p50 and p99. It uses only names the dialled path already had,
// so the same file runs before and after the fence path for a before and after figure.
//
// Guarded by WARDYN_TEST_PG, like every other *_pg_test.go here.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

func BenchmarkRunStartLocks50(b *testing.B) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		b.Skip("WARDYN_TEST_PG not set; skipping the Postgres-backed start lock bench")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, dsn)
	if err != nil {
		b.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	nonce := time.Now().String()
	for n := range b.N {
		l := NewPGLocker(pool, 8)
		var mu sync.Mutex
		var takes []time.Duration
		refused, noCapacity, lost := 0, 0, 0
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				k := NewLockKey(RunStartLockClass, nonce, fmt.Sprint(n), fmt.Sprint(i))
				start := time.Now()
				hctx, release, err := l.Lock(ctx, k, 2*time.Second)
				took := time.Since(start)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					refused++
					if errors.Is(err, ErrLockNoCapacity) {
						noCapacity++
					}
					return
				}
				takes = append(takes, took)
				mu.Unlock()
				time.Sleep(500 * time.Millisecond)
				ended := hctx.Err() != nil && errors.Is(context.Cause(hctx), ErrLockLost)
				release()
				mu.Lock()
				if ended {
					lost++
				}
			}()
		}
		wg.Wait()
		slices.Sort(takes)
		pct := func(p int) time.Duration {
			if len(takes) == 0 {
				return 0
			}
			return takes[(len(takes)-1)*p/100]
		}
		b.Logf("run start locks: 50 concurrent, cap 8: refused=%d (no capacity %d) lost=%d take p50=%s p99=%s",
			refused, noCapacity, lost, pct(50), pct(99))
		b.ReportMetric(float64(refused), "refused")
		b.ReportMetric(float64(lost), "lost")
		b.ReportMetric(float64(pct(99).Microseconds()), "p99-us")
	}
}
