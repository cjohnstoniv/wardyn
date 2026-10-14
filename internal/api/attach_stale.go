// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
)

// The stale-writer probe. A person's reconnecting tab lands read-only behind its
// own dead predecessor, and the 30s ping cycle (attachPingInterval) leaves it
// there for 30-60s. So when a same-principal web client is admitted read-only,
// the writer is pinged on a much shorter clock, and a writer that does not
// answer is evicted through the audited take-over path.
const (
	// attachStaleProbeTimeout is how long one probe ping may go unanswered.
	attachStaleProbeTimeout = 2 * time.Second
	// attachStaleMisses is how many consecutive unanswered pings evict.
	attachStaleMisses = 2
	// attachStaleQuiet is how long the PTY may have sent nothing for ONE
	// unanswered ping to evict.
	attachStaleQuiet = 5 * time.Second
	// attachStaleAttachingPoll is how often a writer that is still attaching is
	// looked at again. It is never pinged: a slow Runner.Attach is not a dead peer.
	attachStaleAttachingPoll = 250 * time.Millisecond
)

// staleWriterVerdict says whether a writer that has missed `misses` consecutive
// probe pings, and has been quiet for `quiet`, is stale.
func staleWriterVerdict(misses int, quiet time.Duration) bool {
	return misses >= attachStaleMisses || (misses >= 1 && quiet >= attachStaleQuiet)
}

// probeStaleWriter runs for one read-only web client until the question is
// settled: the writer answers a ping (alive: nothing is evicted, however slow),
// the writer is gone or is somebody else's, the client itself ended or was
// promoted, or the writer is evicted. Only a ready writer belonging to the same
// principal is ever pinged, so a different person's observer cannot probe a
// writer, and an attaching writer is never judged dead.
func (s *Server) probeStaleWriter(ctx context.Context, runID uuid.UUID, newcomer *attachHolder) {
	misses := 0
	for ctx.Err() == nil && !newcomer.canWrite() && !newcomer.evicted.Load() {
		w := s.attachHolderFor(runID)
		if w == nil || w == newcomer || w.principal != newcomer.principal || w.source != attachSourceWeb || w.ping == nil {
			return
		}
		if !w.ready.isReady() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(attachStaleAttachingPoll):
			}
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, attachStaleProbeTimeout)
		start := time.Now()
		err := w.ping(pctx)
		cancel()
		if err == nil || ctx.Err() != nil {
			return
		}
		misses++
		quiet := time.Since(time.Unix(0, w.lastOutput.Load()))
		if staleWriterVerdict(misses, quiet) {
			s.evictStaleWriter(runID, newcomer, w)
			return
		}
		// A ping that failed at once (a closed socket) must not spin: pace the
		// next one to the probe cadence.
		if wait := attachStaleProbeTimeout - time.Since(start); wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
}

// evictStaleWriter is the take-over path for a writer that stopped answering:
// evict (revoking write authority under the registry lock) and promote the
// newcomer's own socket, then AUDIT, then displace, in the order
// handleAttachTakeover uses, so the row is never lost to the teardown it
// describes. Never a bare promotion.
func (s *Server) evictStaleWriter(runID uuid.UUID, newcomer, stale *attachHolder) {
	prev, promote := s.evictAttachWriter(runID, newcomer.principal, stale)
	if prev == nil {
		return // it left, or was replaced, while it was being probed
	}
	ctx := s.cfg.BaseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if newcomer.via != nil {
		ctx = audit.WithDelegation(ctx, *newcomer.via)
	}
	s.recordTakeover(ctx, runID, newcomer.sandboxRef, newcomer.actorType, newcomer.principal, prev, "stale_writer")
	prev.displace(attachTakeoverReason(newcomer.principal))
	if promote != nil {
		go promote()
	}
}
