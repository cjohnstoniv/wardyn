// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
// FINDING (HIGH): the sidecar's embedded identity mints the bearer token with a
// fixed ~1h ceiling. The default source below just re-reads
// WARDYN_GROUNDTRUTH_TOKEN from the process environment on a 401 — but a
// running container's environment is fixed at exec time and can never change,
// so that path is INERT: it always returns the same stale value, and after
// ~1h every POST 401s forever with no actual recovery. The real fix is
// WARDYN_GROUNDTRUTH_TOKEN_FILE: when set, the source re-reads that file from
// disk on every call (including the 401 refresh), so an operator/control-plane
// process rewriting the file DOES rotate the live token and the stream
// survives past the TTL. Use the file form for anything expected to outlive
// ~1h; the plain env form only degrades gracefully (a persistent 401 is
// counted as a drop, never retried forever).
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

// newEventSink builds a sink from a STATIC boot token. If
// WARDYN_GROUNDTRUTH_TOKEN_FILE is set, the source re-reads that file from
// disk on every call (boot seed AND every 401 refresh), so an operator or the
// control plane can rotate the live token by rewriting the file — this is the
// only path that actually survives the ~1h token TTL. Without the file, the
// source just re-reads WARDYN_GROUNDTRUTH_TOKEN, falling back to the boot
// value; since a process's env can't change after exec, that path never
// yields a new token, so a genuine persistent 401 is counted as a drop rather
// than retried forever (it does NOT "recover").
func newEventSink(controlPlaneURL, token string, bufferSize, batchSize int, flushIval time.Duration, client *http.Client) *eventSink {
	src := func() (string, error) {
		if path := strings.TrimSpace(os.Getenv("WARDYN_GROUNDTRUTH_TOKEN_FILE")); path != "" {
			b, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("read %s: %w", path, err)
			}
			if v := strings.TrimSpace(string(b)); v != "" {
				return v, nil
			}
			return "", fmt.Errorf("%s is empty", path)
		}
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

// maxPostAttempts bounds the retry loop for transport errors / 5xx responses
// (e.g. the control plane's 502 means "retry the batch") so a downed control
// plane cannot retry forever; mirrors the shape of the 401-refresh-retry below
// — a small, bounded number of attempts, never unlimited.
const maxPostAttempts = 3

func (s *eventSink) post(batch []types.AuditEvent) {
	body, err := json.Marshal(groundtruthBatch{Events: batch})
	if err != nil {
		return
	}

	backoff := 250 * time.Millisecond
	for attempt := 1; attempt <= maxPostAttempts; attempt++ {
		status := s.doPost(body, s.currentToken())

		// On a 401 (expired/rotated aud token — the embedded identity mints with a
		// fixed ~1h ceiling) refresh the token from the source and retry ONCE. If
		// the refresh yields the same/invalid token the second attempt's 401 falls
		// into the non-retryable 4xx case below and is counted as a drop, so we
		// never loop forever and a genuine credential gap stays visible in the
		// periodic stats line.
		if status == http.StatusUnauthorized {
			refreshed := s.refreshToken()
			log.Printf("wardyn-tetragon-ingest: ground-truth token rejected (401); refreshed and retrying batch (%d)", len(batch))
			status = s.doPost(body, refreshed)
		}

		switch {
		case status == 0 || status >= 500:
			// Transport error or server error: KEEP the batch and retry with
			// bounded exponential backoff — this stream is billed as
			// tamper-proof, so a transient outage must not silently lose events.
			if attempt == maxPostAttempts {
				s.dropped.Add(uint64(len(batch)))
				log.Printf("wardyn-tetragon-ingest: control plane batch failed after %d attempts (%d events): status %d", attempt, len(batch), status)
				return
			}
			log.Printf("wardyn-tetragon-ingest: control plane batch retry %d/%d (%d events) after status %d", attempt, maxPostAttempts, len(batch), status)
			time.Sleep(backoff)
			backoff *= 2
		case status >= 300:
			// Client error (4xx): non-retryable, the batch itself is rejected.
			s.dropped.Add(uint64(len(batch)))
			log.Printf("wardyn-tetragon-ingest: control plane rejected batch (%d): status %d", len(batch), status)
			return
		default:
			s.posted.Add(uint64(len(batch)))
			return
		}
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
