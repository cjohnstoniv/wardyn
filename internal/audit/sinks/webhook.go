// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// WebhookConfig holds the configuration for a WebhookSink.
type WebhookConfig struct {
	// URL is the HTTP endpoint that receives JSON-lines batches (required).
	URL string `json:"url"`
	// BearerToken is included as "Authorization: Bearer <token>" when non-empty.
	BearerToken string `json:"bearer_token,omitempty"`
	// BatchSize is the maximum number of events per HTTP POST (default 100).
	BatchSize int `json:"batch_size,omitempty"`
	// FlushInterval is how long to wait before flushing a partial batch
	// (default 5s). Parsed as a duration string, e.g. "5s".
	FlushInterval string `json:"flush_interval,omitempty"`
	// BufferSize is the capacity of the in-process event queue (default 4096).
	// Events that would overflow the buffer are dropped and counted.
	BufferSize int `json:"buffer_size,omitempty"`
	// MaxRetries is the number of delivery attempts per batch (default 3).
	MaxRetries int `json:"max_retries,omitempty"`
	// RetryBaseDelay is the initial backoff delay (default 200ms).
	RetryBaseDelay string `json:"retry_base_delay,omitempty"`
}

func (c *WebhookConfig) withDefaults() WebhookConfig {
	out := *c
	if out.BatchSize <= 0 {
		out.BatchSize = 100
	}
	if out.FlushInterval == "" {
		out.FlushInterval = "5s"
	}
	if out.BufferSize <= 0 {
		out.BufferSize = 4096
	}
	if out.MaxRetries <= 0 {
		out.MaxRetries = 3
	}
	if out.RetryBaseDelay == "" {
		out.RetryBaseDelay = "200ms"
	}
	return out
}

// WebhookSink buffers audit events and delivers them in JSON-lines batches via
// HTTP POST. It never blocks the Emit caller beyond a non-blocking channel
// send; overflow increments the drop counter which is logged periodically.
//
// Start the background flusher with Run(ctx); cancel the context to drain and
// stop cleanly. Alternatively call Close() to signal the drain and block until
// the final batch has been flushed (used on graceful shutdown so the last batch
// is awaited rather than abandoned).
type WebhookSink struct {
	cfg       WebhookConfig
	interval  time.Duration
	baseDelay time.Duration
	queue     chan types.AuditEvent
	drops     atomic.Int64
	client    *http.Client
	// stop is closed by Close() to signal Run to drain and exit independently of
	// the Run ctx. done is closed by Run when it returns, so Close can await the
	// final flush. closeOnce guards against a double Close.
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewWebhookSink creates a WebhookSink from cfg. Returns an error if cfg.URL
// is empty or durations cannot be parsed.
func NewWebhookSink(cfg WebhookConfig) (*WebhookSink, error) {
	cfg = cfg.withDefaults()
	if cfg.URL == "" {
		return nil, fmt.Errorf("sinks.webhook: URL is required")
	}
	interval, err := time.ParseDuration(cfg.FlushInterval)
	if err != nil {
		return nil, fmt.Errorf("sinks.webhook: invalid flush_interval %q: %w", cfg.FlushInterval, err)
	}
	base, err := time.ParseDuration(cfg.RetryBaseDelay)
	if err != nil {
		return nil, fmt.Errorf("sinks.webhook: invalid retry_base_delay %q: %w", cfg.RetryBaseDelay, err)
	}
	return &WebhookSink{
		cfg:       cfg,
		interval:  interval,
		baseDelay: base,
		queue:     make(chan types.AuditEvent, cfg.BufferSize),
		client:    &http.Client{Timeout: 15 * time.Second},
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}, nil
}

// Name implements audit.Sink.
func (w *WebhookSink) Name() string { return "webhook" }

// Emit enqueues ev for delivery. If the buffer is full the event is dropped
// and the drop counter is incremented. Emit never blocks.
func (w *WebhookSink) Emit(_ context.Context, ev types.AuditEvent) error {
	select {
	case w.queue <- ev:
	default:
		w.drops.Add(1)
		slog.Warn("sinks.webhook: queue overflow", slog.Int64("drops", w.drops.Load()))
	}
	return nil
}

// Run starts the background flush loop. It returns when ctx is cancelled or
// Close() is called, after flushing any events remaining in the buffer
// (best-effort). Run must be called exactly once per WebhookSink.
func (w *WebhookSink) Run(ctx context.Context) {
	defer close(w.done)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	batch := make([]types.AuditEvent, 0, w.cfg.BatchSize)

	flush := func(fctx context.Context) {
		if len(batch) == 0 {
			return
		}
		w.deliverWithRetry(fctx, batch)
		batch = batch[:0]
	}

	for {
		select {
		case ev := <-w.queue:
			batch = append(batch, ev)
			if len(batch) >= w.cfg.BatchSize {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
		case <-ctx.Done():
			w.drainAndFlush(&batch, flush)
			return
		case <-w.stop:
			w.drainAndFlush(&batch, flush)
			return
		}
	}
}

// drainAndFlush pulls every event still buffered in the queue (non-blocking)
// into batch and flushes once, so the final partial batch is delivered before
// Run exits. flush captures and resets batch, so it is shared by pointer.
//
// The final flush runs under a fresh bounded context rather than the Run ctx:
// on the common signal-driven shutdown the Run ctx is already cancelled, which
// would make the last POST fail immediately and abandon the batch. A short
// independent deadline lets the last batch actually deliver while still bounding
// how long Close can block (finding: last batch must be flushed/awaited on
// shutdown, and Close must not block indefinitely).
func (w *WebhookSink) drainAndFlush(batch *[]types.AuditEvent, flush func(context.Context)) {
	for {
		select {
		case ev := <-w.queue:
			*batch = append(*batch, ev)
		default:
			fctx, cancel := context.WithTimeout(context.Background(), w.client.Timeout)
			flush(fctx)
			cancel()
			return
		}
	}
}

// Close signals the background flusher to drain and stop, then blocks until the
// final batch has been flushed. It must only be called after Run has been
// started (the fanout always starts Run for this sink); otherwise the wait on
// done would block forever. It is safe to call Close multiple times and
// concurrently with a ctx-driven Run shutdown.
func (w *WebhookSink) Close() error {
	w.closeOnce.Do(func() { close(w.stop) })
	<-w.done
	return nil
}

// Drops returns the number of events dropped due to buffer overflow.
func (w *WebhookSink) Drops() int64 { return w.drops.Load() }

// deliverWithRetry encodes batch as newline-delimited JSON and POSTs it to the
// configured URL, retrying up to cfg.MaxRetries times with exponential backoff.
// Delivery failures after all retries count the lost events in the drop counter
// (finding: loss after retry exhaustion was logged but invisible to Drops) and
// are logged; events are not re-queued. The backoff sleep honors both ctx
// cancellation and Close()'s stop signal so shutdown drains promptly rather than
// blocking on context.Background() (finding: blocking retry stalled drain).
func (w *WebhookSink) deliverWithRetry(ctx context.Context, batch []types.AuditEvent) {
	body, err := encodeBatch(batch)
	if err != nil {
		// Encoding failure means none of these events can be delivered; count
		// them as dropped so the loss is observable via Drops().
		w.drops.Add(int64(len(batch)))
		slog.Error("sinks.webhook: encode batch failed",
			slog.Any("err", err),
			slog.Int("dropped_events", len(batch)),
			slog.Int64("drops", w.drops.Load()))
		return
	}
	delay := w.baseDelay
	for attempt := 1; attempt <= w.cfg.MaxRetries; attempt++ {
		if err := w.post(ctx, body); err == nil {
			return
		} else if attempt < w.cfg.MaxRetries {
			slog.Warn("sinks.webhook: delivery attempt failed, retrying",
				slog.Int("attempt", attempt),
				slog.Int("max_retries", w.cfg.MaxRetries),
				slog.Any("err", err),
				slog.Duration("retry_in", delay))
			select {
			case <-ctx.Done():
				// Shutdown via ctx: abandon the batch. Count the loss.
				w.drops.Add(int64(len(batch)))
				return
			case <-w.stop:
				// Shutdown via Close(): abandon the batch. Count the loss.
				w.drops.Add(int64(len(batch)))
				return
			case <-time.After(delay):
			}
			delay *= 2
		} else {
			// Retries exhausted: the batch is lost. Count every event so the
			// never-drop-silently invariant is observable via Drops().
			w.drops.Add(int64(len(batch)))
			slog.Error("sinks.webhook: delivery failed, retries exhausted",
				slog.Int("attempts", w.cfg.MaxRetries),
				slog.Any("err", err),
				slog.Int("batch_size", len(batch)),
				slog.Int64("drops", w.drops.Load()))
		}
	}
}

func (w *WebhookSink) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if w.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+w.cfg.BearerToken)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http post: unexpected status %d", resp.StatusCode)
	}
	return nil
}

// encodeBatch serialises each event as a JSON object followed by a newline
// (JSON-lines / NDJSON format).
func encodeBatch(batch []types.AuditEvent) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i := range batch {
		if err := enc.Encode(batch[i]); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}
