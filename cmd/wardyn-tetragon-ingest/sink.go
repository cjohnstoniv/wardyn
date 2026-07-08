// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tokenSource yields the current host-sensor bearer token. It is an injectable
// seam so the refresh-on-401 path is testable and so the token can be re-read or
// re-minted on demand instead of being frozen at boot.
//
// FINDING (HIGH): the sidecar used to read WARDYN_GROUNDTRUTH_TOKEN once at boot
// and reuse it forever; the embedded identity mints with a fixed ~1h ceiling, so
// after ~1h every POST to /api/v1/internal/groundtruth returned 401 and the
// second audit stream silently died with no recovery. eventSink now fetches the
// token through this source and, on a 401, re-fetches and retries once — so a
// rotated/re-minted token is picked up automatically and the stream recovers.
type tokenSource func() (string, error)

// eventSink batches kernel AuditEvents and POSTs them to the control plane's
// POST /api/v1/internal/groundtruth endpoint with the host-sensor bearer token.
// It mirrors internal/egress/proxy.decisionSink: posting is asynchronous and
// buffered; on backpressure (full buffer) events are DROPPED and a counter is
// incremented rather than blocking the tail loop. A dropped ground-truth event
// is a known, counted gap — never a silent one.
type eventSink struct {
	endpoint string
	// token is the cached current bearer; it is refreshed via tokenSrc on 401.
	token  atomic.Value // string
	tokSrc tokenSource
	client *http.Client

	ch        chan types.AuditEvent
	dropped   atomic.Uint64
	posted    atomic.Uint64
	batchSize int
	flushIval time.Duration

	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

// newEventSink builds a sink from a STATIC boot token. The token is still
// re-fetchable on 401 via the default source below, which simply re-reads the
// same env var — so an out-of-band rotation of WARDYN_GROUNDTRUTH_TOKEN (e.g. by
// the embedded identity helper updating the env/secret) is picked up on the next
// 401 without restarting the sidecar. When the env is unchanged this degrades to
// the prior behavior (the same token is returned), so a genuine persistent 401
// is counted as a drop rather than retried forever.
func newEventSink(controlPlaneURL, token string, bufferSize, batchSize int, flushIval time.Duration, client *http.Client) *eventSink {
	// Default source: re-read WARDYN_GROUNDTRUTH_TOKEN, falling back to the boot
	// value. This gives a refresh seam even without an injected minter.
	src := func() (string, error) {
		if v := strings.TrimSpace(os.Getenv("WARDYN_GROUNDTRUTH_TOKEN")); v != "" {
			return v, nil
		}
		return token, nil
	}
	return newEventSinkWithSource(controlPlaneURL, src, bufferSize, batchSize, flushIval, client)
}

// newEventSinkWithSource builds a sink whose bearer token is fetched from src.
// src is invoked once at construction to seed the cached token, and again on a
// 401 to obtain a refreshed/re-minted token before retrying the POST once.
func newEventSinkWithSource(controlPlaneURL string, src tokenSource, bufferSize, batchSize int, flushIval time.Duration, client *http.Client) *eventSink {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if batchSize <= 0 {
		batchSize = 64
	}
	if flushIval <= 0 {
		flushIval = 2 * time.Second
	}
	if src == nil {
		src = func() (string, error) { return "", nil }
	}
	s := &eventSink{
		endpoint:  controlPlaneURL + "/api/v1/internal/groundtruth",
		tokSrc:    src,
		client:    client,
		ch:        make(chan types.AuditEvent, bufferSize),
		batchSize: batchSize,
		flushIval: flushIval,
	}
	// Seed the cached token. A failure here is non-fatal: the first POST will
	// attempt a refresh anyway, and the 401 path will surface a persistent gap.
	if tok, err := src(); err == nil {
		s.token.Store(tok)
	} else {
		s.token.Store("")
	}
	s.wg.Add(1)
	go s.run()
	return s
}

// currentToken returns the cached bearer token.
func (s *eventSink) currentToken() string {
	v, _ := s.token.Load().(string)
	return v
}

// refreshToken re-fetches the token from the source and caches it. It returns
// the new token (or the old cached one on error, so callers always have a value
// to try). Used on a 401 to pick up a rotated/re-minted credential.
func (s *eventSink) refreshToken() string {
	if s.tokSrc == nil {
		return s.currentToken()
	}
	tok, err := s.tokSrc()
	if err != nil || strings.TrimSpace(tok) == "" {
		// Keep the existing token; a persistent 401 will be counted as a drop.
		log.Printf("wardyn-tetragon-ingest: ground-truth token refresh failed: %v", err)
		return s.currentToken()
	}
	s.token.Store(tok)
	return tok
}

// emit queues an event without blocking. On a full buffer the event is dropped
// and the drop counter is incremented (mirrors decisionSink.emit).
func (s *eventSink) emit(ev types.AuditEvent) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	select {
	case s.ch <- ev:
	default:
		s.dropped.Add(1)
	}
}

// run drains the channel into batches and flushes on size or interval.
func (s *eventSink) run() {
	defer s.wg.Done()
	t := time.NewTicker(s.flushIval)
	defer t.Stop()
	batch := make([]types.AuditEvent, 0, s.batchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.post(batch)
		batch = batch[:0]
	}
	for {
		select {
		case ev, ok := <-s.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, ev)
			if len(batch) >= s.batchSize {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

// groundtruthBatch is the POST body shape the control plane accepts.
type groundtruthBatch struct {
	Events []types.AuditEvent `json:"events"`
}

func (s *eventSink) post(batch []types.AuditEvent) {
	body, err := json.Marshal(groundtruthBatch{Events: batch})
	if err != nil {
		return
	}

	// First attempt uses the cached token.
	status := s.doPost(body, s.currentToken())

	// On a 401 (expired/rotated aud token — the embedded identity mints with a
	// fixed ~1h ceiling) refresh the token from the source and retry ONCE. If the
	// refresh yields the same/invalid token the second attempt's 401 is counted
	// as a drop, so we never loop forever and a genuine credential gap stays
	// visible in the periodic stats line.
	if status == http.StatusUnauthorized {
		refreshed := s.refreshToken()
		log.Printf("wardyn-tetragon-ingest: ground-truth token rejected (401); refreshed and retrying batch (%d)", len(batch))
		status = s.doPost(body, refreshed)
	}

	switch {
	case status == 0:
		// Transport error: best-effort drop so the loss is visible; never block
		// the tail loop.
		s.dropped.Add(uint64(len(batch)))
	case status >= 300:
		s.dropped.Add(uint64(len(batch)))
		log.Printf("wardyn-tetragon-ingest: control plane rejected batch (%d): status %d", len(batch), status)
	default:
		s.posted.Add(uint64(len(batch)))
	}
}

// doPost performs a single POST with the given bearer token. It returns the HTTP
// status code, or 0 on a transport error (so the caller can distinguish a
// network failure from an HTTP reject and decide whether to refresh the token).
func (s *eventSink) doPost(body []byte, token string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.client.Do(req)
	if err != nil {
		// Best-effort: a failed post must not block the tail.
		return 0
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	return resp.StatusCode
}

func (s *eventSink) droppedCount() uint64 { return s.dropped.Load() }
func (s *eventSink) postedCount() uint64  { return s.posted.Load() }

// close drains and stops the sink, blocking until the worker exits or ctx done.
func (s *eventSink) close(ctx context.Context) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.ch)
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
