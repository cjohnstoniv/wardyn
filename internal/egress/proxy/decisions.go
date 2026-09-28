// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// decisionSink streams egress.DecisionLog records to the control plane's
// POST /api/v1/internal/decisions endpoint. Posting is asynchronous and
// buffered: on backpressure (full buffer) records are dropped and a counter
// is incremented rather than blocking the request path. Every record is also
// mirrored to stdout as a JSON line for local observability.
type decisionSink struct {
	endpoint string
	token    *tokenSource
	client   *http.Client
	out      io.Writer

	ch      chan egress.DecisionLog
	dropped atomic.Uint64

	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool

	// outMu serialises writes to out: a single Write over PIPE_BUF is not
	// atomic at the OS level, so concurrent mirror() calls could interleave
	// into a corrupted record without it.
	outMu sync.Mutex
}

func newDecisionSink(controlPlaneURL string, token *tokenSource, bufferSize int, client *http.Client, out io.Writer) *decisionSink {
	s := &decisionSink{
		endpoint: controlPlaneURL + "/api/v1/internal/decisions",
		token:    token,
		client:   client,
		out:      out,
		ch:       make(chan egress.DecisionLog, bufferSize),
	}
	s.wg.Add(1)
	go s.run()
	return s
}

// emit queues a decision log without blocking. If the buffer is full the
// record is dropped and the drop counter is incremented. The record is
// always mirrored to stdout synchronously (cheap, non-blocking enough).
func (s *decisionSink) emit(log egress.DecisionLog) {
	s.mirror(log)
	// Runs UNDER s.mu, mutually exclusive with close() (which also holds s.mu
	// while closing the channel) — otherwise a send past the closed-check
	// could race close() and panic on a closed channel. Never blocks (buffered
	// + default), so no latency added to the request path.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		// Counted, not silently returned: MITM tunnels run on hijacked
		// http.Servers that Shutdown never stops, so a run's last
		// credential-injecting requests are exactly the ones still emitting
		// while the sink drains; close() reads this counter to report the gap.
		s.dropped.Add(1)
		return
	}
	select {
	case s.ch <- log:
	default:
		s.dropped.Add(1)
	}
}

// mirror writes the decision as one JSON line to stdout, masking any secret
// values registered in the process-global registry (verbatim byte-identical
// occurrences only). The mask snapshot is taken fresh per call.
func (s *decisionSink) mirror(log egress.DecisionLog) {
	if s.out == nil {
		return
	}
	b, err := json.Marshal(log)
	if err != nil {
		return
	}
	line := maskDecisionBytes(append(b, '\n'))
	s.outMu.Lock()
	_, _ = s.out.Write(line)
	s.outMu.Unlock()
}

// dropReportInterval bounds how often the worker flushes a synthetic
// egress:dropped-decisions-<n> summary when IDLE; under active traffic drops
// surface at the next flush instead.
const dropReportInterval = 30 * time.Second

func (s *decisionSink) run() {
	defer s.wg.Done()
	// Tracks the drop count already summarized, so each summary carries only
	// the delta since last report. Single-goroutine, so a plain local is safe.
	var reported uint64
	ticker := time.NewTicker(dropReportInterval)
	defer ticker.Stop()
	for {
		select {
		case log, ok := <-s.ch:
			if !ok {
				s.reportDropped(&reported) // drained + closed: final tail summary
				return
			}
			s.reportDropped(&reported) // piggyback on the next successful flush
			// A decision the control plane REFUSED (e.g. 413 over maxJSONBody)
			// counts as dropped too, so it isn't silently lost from the audit trail.
			if err := s.post(log); err != nil {
				s.dropped.Add(1)
			}
		case <-ticker.C:
			s.reportDropped(&reported) // idle: drops accrued, no traffic to piggyback on
		}
	}
}

// reportDropped posts a synthetic egress.deny summary when the drop counter
// has advanced since *reported, so drops surface as they happen rather than
// only at shutdown. Runs on the worker goroutine, reusing post(), so it adds
// no request-path latency. *reported only advances once the post lands, so a
// CP outage retries rather than losing the count.
func (s *decisionSink) reportDropped(reported *uint64) {
	cur := s.dropped.Load()
	if cur <= *reported {
		return
	}
	n := cur - *reported
	// post() only, never mirror(): mirroring here would race emit()'s stdout
	// writes and break the "one mirrored line per real decision" invariant.
	if err := s.post(droppedSummaryLog(n)); err != nil {
		return
	}
	*reported = cur
}

// droppedSummaryLog builds the synthetic DecisionLog summarizing dropped
// decisions. DecisionLog has no dedicated count field, so the count rides in
// RuleSource under the "egress:dropped-decisions-<n>" marker. Decision is
// Deny: an unrecorded decision is surfaced as a fail-closed alert.
func droppedSummaryLog(n uint64) egress.DecisionLog {
	return egress.DecisionLog{
		Request:    egress.Request{Time: time.Now()},
		Decision:   egress.Deny,
		RuleSource: fmt.Sprintf("egress:dropped-decisions-%d", n),
	}
}

// post delivers one decision log to the control-plane audit. It returns an
// error when delivery did NOT land, so callers that must not lose the record
// (reportDropped) can retry; individual per-decision posts ignore the error
// (best-effort — must never block egress).
func (s *decisionSink) post(log egress.DecisionLog) error {
	body, err := json.Marshal(log)
	if err != nil {
		return err
	}
	body = maskDecisionBytes(body)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token.Get())
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("decision post: status %d", resp.StatusCode)
	}
	return nil
}

// maskDecisionBytes applies the process-global secret mask to the serialised
// decision log bytes, taking a fresh snapshot each call so secrets registered
// after startup are visible immediately. Package-level, not a method, so
// tests can exercise it standalone.
func maskDecisionBytes(b []byte) []byte {
	// uuid.Nil yields only global secrets: no per-run entries exist in the
	// proxy process, all proxy-side secrets are global.
	snap := procRegistry.Snapshot(uuid.Nil)
	if len(snap) == 0 {
		return b
	}
	// SECURITY: both callers hand this JSON, not plain text — json.Marshal
	// escapes \n, \", \\ and HTML-escapes & < >, so a raw-value masker would
	// miss a registered secret carrying those bytes. JSONEscapedVariants
	// covers that expansion (shared with internal/api/recording.go and
	// cmd/wardynd's audit maskingRecorder).
	return secretmask.NewMasker(secretmask.JSONEscapedVariants(snap)).Mask(b)
}

// droppedCount reports how many decision logs were dropped on backpressure.
func (s *decisionSink) droppedCount() uint64 { return s.dropped.Load() }

// close drains and stops the sink. It blocks until the worker exits or ctx
// is done.
func (s *decisionSink) close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.ch)
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		if d := s.droppedCount(); d > 0 {
			return fmt.Errorf("decision sink closed with %d dropped records", d)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
