// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
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
		if !b.acquire(ctx, n) {
			return false
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

// acquire takes n bytes of the retained budget, waiting for room at most scanQueueWait.
func (b *scanBudget) acquire(ctx context.Context, n int64) bool {
	if b.retained.TryAcquire(n) {
		return true
	}
	wctx, cancel := context.WithTimeout(ctx, scanQueueWait)
	defer cancel()
	return b.retained.Acquire(wctx, n) == nil
}

// chargeMore charges n more bytes to a request already holding had from readBody, never taking
// it past the whole budget, so it cannot wait on its own bytes. It fails with errScanBusy as
// readBody does; the returned release is idempotent and non-nil.
func (b *scanBudget) chargeMore(ctx context.Context, had, n int64) (func(), error) {
	b.init()
	if n = min(n, b.limit-min(had, b.limit)); n <= 0 {
		return func() {}, nil
	}
	if !b.acquire(ctx, n) {
		return func() {}, errScanBusy
	}
	var once sync.Once
	return func() { once.Do(func() { b.retained.Release(n) }) }, nil
}

// chargedBodyKey marks a request whose body a door already holds charged by readBody:
// scanBufferedBody scans those bytes instead of reading them again, since a second charge
// would wait on the request's own bytes until scanQueueWait.
type chargedBodyKey struct{}

// holdChargedBody makes buf, read and charged by the caller, r's body; the caller keeps
// the charge until r is done.
func holdChargedBody(r *http.Request, buf []byte) {
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(buf)), int64(len(buf))
	*r = *r.WithContext(context.WithValue(r.Context(), chargedBodyKey{}, buf))
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
