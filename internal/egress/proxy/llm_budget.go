// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// maxLLMScanBody bounds how much of an LLM request body the proxy will buffer to
// inspect. A body larger than this is forwarded UNSCANNED (fail-open, or refused
// when block+on_scanner_error=block) and recorded as body_oversize — never
// truncated, since that would corrupt the request. A var so tests can exercise
// the oversize path without allocating tens of MiB.
var maxLLMScanBody = 32 << 20 // 32 MiB

// maxConcurrentScans bounds how many request bodies may be BUFFERED AND
// EXTRACTED at once, process-wide. It is a memory bound, not a throughput knob:
// contentscan's extractor re-materialises a body ~5.3x live heap regardless of
// which detectors run, and the wardyn-proxy sidecar has a HARD 256 MiB cgroup
// cap — one in-cap 30 MiB body peaks at ~158 MiB, two concurrent already exceed
// the cap and an OOM-killed sidecar takes the run's only network path with it.
// Waiting (rather than skipping the scan) is deliberate: a queued request is
// still fully inspected, bounded by scanQueueWait below, not by anything above
// this call.
const maxConcurrentScans = 1

// scanSlots is maxConcurrentScans' semaphore. Package-level because the bound
// it enforces is the PROCESS's cgroup memory cap, not any one Proxy's.
var scanSlots = make(chan struct{}, maxConcurrentScans)

// maxRetainedScanBytes bounds the total buffered request bytes inspection may
// hold live at once, for as long as the buffer is REACHABLE, not just while
// being scanned. The scan slot above only bounds the buffer+extract WINDOW:
// scanBufferedBody releases its slot once it hands the caller a re-readable
// copy, so N requests stalled on a slow upstream retain N x maxLLMScanBody with
// no slot held — the same 256 MiB cgroup arithmetic maxConcurrentScans guards
// against, reached around it. 64 MiB leaves room beside one in-flight
// extraction under that cap; it is charged AFTER the scan peak (see
// scanBufferedBody) so the two don't double-count the same request's peak.
var maxRetainedScanBytes = 64 << 20 // 64 MiB

// scanRetained is that budget, a semaphore over BYTES (scanSlots counts
// requests, the wrong unit for a memory bound). Package-level for the same
// reason scanSlots is: the ceiling is the PROCESS's.
var scanRetained = semaphore.NewWeighted(int64(maxRetainedScanBytes))

// retainScanBuffer charges n bytes to scanRetained, waiting (ctx-bounded) for
// room, and returns an idempotent release. false means the caller must FAIL
// CLOSED, exactly as an expired scan-slot wait does. A body larger than the
// whole budget is charged as the whole budget. TryAcquire goes first because
// Acquire refuses an expired ctx even when there is room, and the ctx may have
// already run out during the body read.
func retainScanBuffer(ctx context.Context, n int) (func(), bool) {
	w := int64(min(n, maxRetainedScanBytes))
	if !scanRetained.TryAcquire(w) && scanRetained.Acquire(ctx, w) != nil {
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { scanRetained.Release(w) }) }, true
}

// scanQueueWait bounds how long a request may wait for a scan slot. It must be
// bounded HERE: the agent-facing listener sets ReadTimeout 0 (streaming bodies
// and CONNECT tunnels need it), so with maxConcurrentScans at 1 a single
// slow-loris POST would otherwise park every other inspected request of the
// run in the semaphore send forever. Expiring the wait fails CLOSED (Deny +
// 502), so the bound cannot become a way to get a body forwarded unscanned;
// the value is generous relative to a healthy scan and short relative to a
// hung one, so it fires on abuse rather than on load.
const scanQueueWait = 30 * time.Second
