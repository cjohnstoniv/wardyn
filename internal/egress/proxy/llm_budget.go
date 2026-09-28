// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// maxLLMScanBody bounds how much of an LLM body the proxy buffers to inspect; oversize
// forwards unscanned (or refuses under block+on_scanner_error=block) rather than
// truncating, which would corrupt the request.
var maxLLMScanBody = 32 << 20 // 32 MiB

// maxConcurrentScans bounds concurrent buffer+extract, process-wide — a memory bound, not
// throughput: extraction re-materializes a body ~5.3x live heap against the sidecar's hard
// 256 MiB cgroup cap (one 30 MiB body peaks ~158 MiB; two would OOM-kill the sidecar and its
// run's only network path). Waiting rather than skipping keeps a queued request fully
// inspected, bounded by scanQueueWait instead.
const maxConcurrentScans = 1

// scanSlots is maxConcurrentScans' semaphore; package-level since the bound is the
// process's cgroup cap, not any one Proxy's.
var scanSlots = make(chan struct{}, maxConcurrentScans)

// maxRetainedScanBytes bounds buffered request bytes inspection holds live while reachable,
// not just while scanning — scanBufferedBody releases its slot on handing back a re-readable
// copy, so N stalled requests retain N x maxLLMScanBody unguarded by maxConcurrentScans. 64
// MiB leaves room beside one in-flight extraction under the 256 MiB cap, charged after the
// scan peak so the two don't double-count.
var maxRetainedScanBytes = 64 << 20 // 64 MiB

// scanRetained is that budget as a byte semaphore (scanSlots counts requests, the wrong
// unit); package-level for the same reason.
var scanRetained = semaphore.NewWeighted(int64(maxRetainedScanBytes))

// retainScanBuffer charges n bytes to scanRetained, waiting (ctx-bounded) for room, and
// returns an idempotent release; false means the caller must FAIL CLOSED, same as an expired
// scan-slot wait. TryAcquire goes first since Acquire refuses an already-expired ctx even with room.
func retainScanBuffer(ctx context.Context, n int) (func(), bool) {
	w := int64(min(n, maxRetainedScanBytes))
	if !scanRetained.TryAcquire(w) && scanRetained.Acquire(ctx, w) != nil {
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { scanRetained.Release(w) }) }, true
}

// scanQueueWait bounds how long a request waits for a scan slot; must be bounded HERE since
// the agent-facing listener has no ReadTimeout (streaming/CONNECT need it) and
// maxConcurrentScans=1 would let a slow-loris POST park every other request forever.
// Expiring fails CLOSED (Deny + 502) — generous relative to a healthy scan, short relative
// to a hung one, so it fires on abuse not load.
const scanQueueWait = 30 * time.Second
