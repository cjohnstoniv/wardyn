// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// maxLLMScanBody bounds how much of an LLM body the proxy buffers to inspect; oversize
// forwards unscanned (or refuses under block+on_scanner_error=block) rather than
// truncating, which would corrupt the request.
var maxLLMScanBody = 32 << 20 // 32 MiB

// maxConcurrentScans bounds concurrent extract+scan per engine — a memory bound, not
// throughput: extraction re-materializes a body ~5.3x live heap against the sidecar's hard
// 256 MiB cgroup cap (one 30 MiB body peaks ~158 MiB; two would OOM-kill the sidecar and its
// run's only network path). Waiting rather than skipping keeps a queued request fully
// inspected, bounded by scanQueueWait instead.
const maxConcurrentScans = 1

// maxRetainedScanBytes bounds the request bytes inspection holds live per engine, from
// before the read until the caller is done with the buffer: N slow or stalled requests
// would otherwise retain N x maxLLMScanBody. 64 MiB leaves room beside one in-flight
// extraction under the 256 MiB cap.
var maxRetainedScanBytes = 64 << 20 // 64 MiB

// scanBudget is one engine's inspection bounds: the scan slots (maxConcurrentScans) and the
// retained-bytes budget (maxRetainedScanBytes). Each Proxy has its own, so a via_org engine
// inside wardynd bounds only its own run; a sidecar runs one Proxy, so there it is the
// process's bound. The zero value is ready.
type scanBudget struct {
	once     sync.Once
	slots    chan struct{}
	retained *semaphore.Weighted
	limit    int64
}

func (b *scanBudget) init() *scanBudget {
	b.once.Do(func() {
		b.slots = make(chan struct{}, maxConcurrentScans)
		b.limit = int64(maxRetainedScanBytes)
		b.retained = semaphore.NewWeighted(b.limit)
	})
	return b
}

// errScanBusy means the retained budget had no room within scanQueueWait; the caller FAILS
// CLOSED, same as an expired scan-slot wait.
var errScanBusy = errors.New("timed out waiting for inspection memory")

// readBody buffers at most limit+1 bytes of body before any scan slot is taken, so a body that
// arrives slowly holds no slot. The bytes are charged to the retained budget before the read:
// the declared length when there is one, else limit+1, and never more than the whole budget,
// so an oversize body waits only for an empty one. What the read did not use is handed back at
// once. A wait for room is bounded by scanQueueWait and fails with errScanBusy; TryAcquire goes
// first since Acquire refuses an already-expired ctx even with room. The returned release gives
// the rest back; it is idempotent and non-nil.
func (b *scanBudget) readBody(ctx context.Context, body io.Reader, declared, limit int64) ([]byte, func(), error) {
	b.init()
	var charged int64
	charge := func(n int64) bool {
		if n = min(n, b.limit-charged); n <= 0 {
			return true
		}
		if !b.retained.TryAcquire(n) {
			wctx, cancel := context.WithTimeout(ctx, scanQueueWait)
			defer cancel()
			if b.retained.Acquire(wctx, n) != nil {
				return false
			}
		}
		charged += n
		return true
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if charged > 0 {
				b.retained.Release(charged)
			}
		})
	}
	want := limit + 1
	if declared >= 0 {
		want = min(want, declared)
	}
	if !charge(want) {
		return nil, func() {}, errScanBusy
	}
	buf, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		release()
		return nil, func() {}, err
	}
	if n := int64(len(buf)); n < charged {
		b.retained.Release(charged - n)
		charged = n
	} else if !charge(n - charged) {
		release()
		return nil, func() {}, errScanBusy
	}
	return buf, release, nil
}

// takeSlot waits, bounded by ctx and scanQueueWait, for one of b's scan slots. An error means
// the caller FAILS CLOSED; otherwise the caller defers free.
func (b *scanBudget) takeSlot(ctx context.Context) (free func(), err error) {
	b.init()
	ctx, cancel := context.WithTimeout(ctx, scanQueueWait)
	defer cancel()
	select {
	case b.slots <- struct{}{}:
		return func() { <-b.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// scanQueueWait bounds how long a request waits for a scan slot or for inspection memory;
// must be bounded HERE since the agent-facing listener has no ReadTimeout (streaming/CONNECT
// need it), so a request whose bytes stay charged would otherwise park every other one forever.
// Expiring fails CLOSED (Deny + 502) — generous relative to a healthy scan, short relative
// to a hung one, so it fires on abuse not load.
const scanQueueWait = 30 * time.Second
