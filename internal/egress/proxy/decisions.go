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
	token    string
	client   *http.Client
	out      io.Writer

	ch      chan egress.DecisionLog
	dropped atomic.Uint64

	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
}

func newDecisionSink(controlPlaneURL, token string, bufferSize int, client *http.Client, out io.Writer) *decisionSink {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
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
	// The non-blocking send runs UNDER s.mu so it is mutually exclusive with
	// close(), which also holds s.mu while it close()s the channel. Otherwise a
	// send that already passed the closed-check could race close() and panic on a
	// closed channel (E3). The send never blocks (buffered + default), so holding
	// the lock here adds no latency to the request path.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- log:
	default:
		s.dropped.Add(1)
	}
}

// mirror writes the decision as one JSON line to stdout, masking any secret
// values registered in the process-global registry. The mask is a snapshot
// taken at the time of the write (lazy snapshot per-emit is intentional:
// injection rules are registered before the first emit, so the snapshot is
// always current).
//
// HONEST RESIDUAL: masking catches verbatim byte-identical occurrences only.
func (s *decisionSink) mirror(log egress.DecisionLog) {
	if s.out == nil {
		return
	}
	b, err := json.Marshal(log)
	if err != nil {
		return
	}
	line := maskDecisionBytes(append(b, '\n'))
	// Single Write of the line+newline; concurrent emits may interleave at
	// the OS level but each line is written atomically here.
	_, _ = s.out.Write(line)
}

// dropReportInterval bounds how often the worker flushes a synthetic
// egress.decisions.dropped summary when it is IDLE (drops accrued but no traffic
// to piggyback on). Under active traffic drops surface at the next flush, so
// this only bounds the idle case.
const dropReportInterval = 30 * time.Second

func (s *decisionSink) run() {
	defer s.wg.Done()
	// reported tracks the drop count already summarized to the control plane, so
	// each summary carries only the delta since the last report. Single-goroutine
	// (this loop), so a plain local is safe; s.dropped itself is atomic.
	var reported uint64
	ticker := time.NewTicker(dropReportInterval)
	defer ticker.Stop()
	for {
		select {
		case log, ok := <-s.ch:
			if !ok {
				// Channel drained + closed: emit a final summary of any tail drops
				// so the last window is never silently lost.
				s.reportDropped(&reported)
				return
			}
			// Piggyback: surface accrued drops on the next successful flush so a
			// drop is visible in the audit trail long before shutdown.
			s.reportDropped(&reported)
			_ = s.post(log) // individual decision: best-effort, must never block egress
		case <-ticker.C:
			// Idle path: drops accrued but no traffic to piggyback on.
			s.reportDropped(&reported)
		}
	}
}

// reportDropped posts a synthetic egress.decisions.dropped audit event when the
// drop counter has advanced since *reported, so the auditor learns that N egress
// decisions were NOT individually recorded — rather than only learning the total
// at shutdown. It runs on the worker goroutine and reuses post(), so it never
// adds latency to the agent's request path.
//
// HONEST POSTURE: DB delivery of individual decisions is best-effort under
// flood; when the buffer overflows those specific decisions are lost, but the
// gap is summarized to the control plane (count since last report), not silent.
// The delta is only marked reported once the summary post actually lands, so a
// CP outage (the very cause of the drops) retries rather than losing the count.
func (s *decisionSink) reportDropped(reported *uint64) {
	cur := s.dropped.Load()
	if cur <= *reported {
		return
	}
	n := cur - *reported
	// post() only, never mirror(): the summary's job is to reach the control-plane
	// audit. Mirroring from this worker goroutine would race emit()'s stdout writes
	// on the request path (s.out is single-writer by contract) and would pollute
	// the "one mirrored line per real decision" stdout invariant.
	//
	// Advance *reported ONLY on a delivered summary: the CP being wedged is the very
	// failure that produces these drops, so a failed post must NOT burn the delta —
	// leave it unadvanced so the count accrues and retries on the next flush/tick
	// until it actually lands (otherwise "not silent" would be a lie under CP outage).
	if err := s.post(droppedSummaryLog(n)); err != nil {
		return
	}
	*reported = cur
}

// droppedSummaryLog builds the synthetic DecisionLog summarizing dropped
// decisions. DecisionLog has no dedicated count field (its wire shape is owned
// elsewhere), so the count rides in RuleSource under the extensible
// "egress.decisions.dropped:<n>" marker. Decision is Deny: an unrecorded
// decision is an audit-fidelity gap, surfaced as a fail-closed alert.
func droppedSummaryLog(n uint64) egress.DecisionLog {
	return egress.DecisionLog{
		Request:    egress.Request{Time: time.Now()},
		Decision:   egress.Deny,
		RuleSource: fmt.Sprintf("egress.decisions.dropped:%d", n),
	}
}

// post delivers one decision log to the control-plane audit. It returns an error
// when delivery did NOT land (marshal / build / transport / non-2xx) so callers that
// must not lose the record (reportDropped) can retry; individual per-decision posts
// ignore the error (best-effort — a failed decision post must never block egress).
func (s *decisionSink) post(log egress.DecisionLog) error {
	body, err := json.Marshal(log)
	if err != nil {
		return err
	}
	// Mask secret values from the body before posting to the control plane.
	// HONEST RESIDUAL: verbatim byte-identical masking only.
	body = maskDecisionBytes(body)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)
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
// decision log bytes. A snapshot is taken fresh on each call so that secrets
// registered after startup (e.g. in tests) are visible immediately.
// This is intentionally a package-level function (not a method) so tests can
// exercise it without constructing a full decisionSink.
func maskDecisionBytes(b []byte) []byte {
	// Snapshot with uuid.Nil yields only global secrets (no per-run entries
	// exist in the proxy process — all proxy-side secrets are global).
	snap := procRegistry.Snapshot(uuid.Nil)
	if len(snap) == 0 {
		return b
	}
	return secretmask.NewMasker(snap).Mask(b)
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
