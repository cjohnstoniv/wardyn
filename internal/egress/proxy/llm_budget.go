// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// maxLLMScanBody bounds how much of an LLM request body the proxy will buffer in
// order to inspect it. A body larger than this is forwarded UNSCANNED (fail-open,
// or refused when block+on_scanner_error=block) and recorded as body_oversize — we
// never truncate (that would corrupt the request). A var (not const) so tests can
// exercise the oversize path without allocating tens of MiB.
var maxLLMScanBody = 32 << 20 // 32 MiB

// maxConcurrentScans bounds how many request bodies may be BUFFERED AND
// EXTRACTED at once, process-wide (one proxy per sidecar process).
//
// It is a memory bound, not a throughput knob. The buffer+extract path
// does not cost one body: contentscan's extractor re-materialises it several
// times over (content array -> []json.RawMessage, each block -> a struct with
// its own string, a generic body -> interface{} boxing), measured at ~5.3x live
// heap and essentially independent of which detectors are on — it is the
// extractor, not the scanning. Meanwhile the wardyn-proxy sidecar runs under a
// HARD 256 MiB cgroup cap with swap pinned equal (internal/runner/docker's
// proxyMemoryMiB, internal/runner/k8s). One in-cap 30 MiB body peaks at ~158
// MiB of live heap; TWO concurrent peak at ~274 MiB — already over the cap, and
// an OOM-killed proxy sidecar takes the run's only network path with it. The
// agent inside the sandbox picks both the body sizes and the concurrency, so
// nothing else bounds this.
//
// Waiting (rather than skipping the scan) is deliberate: a queued request is
// still fully inspected, so load can never turn into unscanned egress. The wait
// is bounded by scanQueueWait below — NOT by anything above this call.
const maxConcurrentScans = 1

// scanSlots is maxConcurrentScans' semaphore. Package-level because the bound
// it enforces is the PROCESS's cgroup memory cap, not any one Proxy's.
var scanSlots = make(chan struct{}, maxConcurrentScans)

// maxRetainedScanBytes bounds the total buffered request bytes inspection may
// hold live at once, counted for as long as the buffer is REACHABLE — not just
// while it is being scanned.
//
// Trust boundary: the scan slot above bounds the buffer+extract
// WINDOW; it says nothing about the buffer's LIFETIME. scanBufferedBody hands
// its caller a re-readable copy of the whole body and the caller then streams
// it through RoundTrip, so the slot was already released while up to
// maxLLMScanBody (32 MiB) stayed live per in-flight request. N requests stalled
// on a slow upstream therefore retained N x 32 MiB with NO slot held: one 32
// MiB body extracting (~170 MiB at the measured 5.3x) plus three already-scanned
// ones waiting on the upstream (96 MiB) is ~266 MiB against the sidecar's hard
// 256 MiB cgroup cap — the very arithmetic maxConcurrentScans exists to
// prevent, reached around it.
//
// 64 MiB leaves room beside one in-flight extraction under that cap, and the
// budget is charged AFTER the scan peak (see scanBufferedBody) so the two do
// not double-count the same request's peak. A var so tests can shrink it
// instead of allocating tens of MiB.
var maxRetainedScanBytes = 64 << 20 // 64 MiB

// scanRetained is that budget, a semaphore over BYTES (scanSlots counts
// requests, the wrong unit for a memory bound). Package-level for the same
// reason scanSlots is: the ceiling is the PROCESS's.
var scanRetained = semaphore.NewWeighted(int64(maxRetainedScanBytes))

// retainScanBuffer charges n bytes to scanRetained, waiting (ctx-bounded) for
// room, and returns an idempotent release. false means the caller must FAIL
// CLOSED, exactly as an expired scan-slot wait does.
//
// Callers charge while still holding their scan slot, and maxConcurrentScans is
// 1, so there is never more than one acquirer and a partial charge cannot
// deadlock against another. A body larger than the whole budget is charged as
// the whole budget, so it waits for an empty budget instead of forever on room
// only it could free. TryAcquire goes first because Acquire refuses an expired
// ctx even when there is room, and the ctx may have run out during the body
// read: only an actual wait is bounded by it.
func retainScanBuffer(ctx context.Context, n int) (func(), bool) {
	w := int64(min(n, maxRetainedScanBytes))
	if !scanRetained.TryAcquire(w) && scanRetained.Acquire(ctx, w) != nil {
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { scanRetained.Release(w) }) }, true
}

// scanQueueWait bounds how long a request may wait for a scan slot.
//
// It has to be bounded HERE, because nothing above this call bounds it: the
// agent-facing listener that serves handlePlain and /wardyn/llm/* sets
// ReadTimeout 0 — streaming bodies and CONNECT tunnels need it (NewServer,
// server.go) — and only the inner MITM server carries a whole-request deadline.
// With maxConcurrentScans at 1 and the slot held across io.ReadAll of a
// SANDBOX-controlled body, a single slow-loris POST would otherwise park every
// other inspected request of the run in the semaphore send forever, each
// retaining a goroutine and a socket in a 256 MiB sidecar (a retention class
// reached through the inspection path and triggerable by the
// untrusted sandbox).
//
// Expiring the wait fails CLOSED — Deny + 502, like the read-error arm beside
// it — so the bound cannot become a way to get a body forwarded unscanned. The
// value is generous relative to a healthy scan (an in-cap body extracts and
// scans in well under a second) and short relative to a hung one, so it fires
// on abuse rather than on load.
const scanQueueWait = 30 * time.Second
