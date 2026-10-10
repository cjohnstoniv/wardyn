// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// WebhookTimeout bounds every delivery attempt, including the final flush
// Fanout.Close awaits on graceful shutdown; wardynd's shutdown grace is sized
// on this constant (internal/api TestShutdownGraceCoversTheBudget).
const WebhookTimeout = 15 * time.Second

// Delivery modes for WebhookConfig. "" is the best-effort fan-out sink this
// config has always built; "acknowledged" is the opt-in mode where a
// leader-only loop reads the stored trail from a durable checkpoint instead
// (see AckedWebhook), so ParseSinks builds no fan-out sink for it.
const (
	DeliveryBestEffort   = "best_effort"
	DeliveryAcknowledged = "acknowledged"
)

// WebhookConfig holds the configuration for a WebhookSink.
type WebhookConfig struct {
	// URL is the HTTP endpoint that receives JSON-lines batches (required).
	URL string `json:"url"`
	// BearerToken is sent as "Authorization: Bearer <token>"; requires https://.
	BearerToken string `json:"bearer_token,omitempty"`
	// Delivery is "" or DeliveryBestEffort for the buffered fan-out sink, or
	// DeliveryAcknowledged for the checkpointed loop; any other value refuses
	// boot. Syslog has no acceptance signal, so it stays best-effort.
	Delivery string `json:"delivery,omitempty"`
	// BatchSize is the maximum number of events per HTTP POST (default 100).
	BatchSize int `json:"batch_size,omitempty"`
	// FlushInterval is how long to wait before flushing a partial batch
	// (default 5s), parsed as a duration string.
	FlushInterval string `json:"flush_interval,omitempty"`
	// BufferSize is the capacity of the in-process event queue (default 4096);
	// events that would overflow it are dropped and counted.
	BufferSize int `json:"buffer_size,omitempty"`
	// MaxRetries is the number of delivery attempts per batch (default 3).
	MaxRetries int `json:"max_retries,omitempty"`
	// RetryBaseDelay is the initial backoff delay (default 200ms).
	RetryBaseDelay string `json:"retry_base_delay,omitempty"`
	// Timeout bounds the HTTP client's per-request wait, including a Close()
	// drain (default and maximum WebhookTimeout). Must be positive and at
	// most WebhookTimeout, which wardynd's shutdown grace is sized on.
	Timeout string `json:"timeout,omitempty"`
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
	if out.Timeout == "" {
		out.Timeout = WebhookTimeout.String()
	}
	return out
}

// WebhookSink buffers audit events and delivers them in JSON-lines batches via
// HTTP POST. It never blocks the Emit caller beyond a non-blocking channel
// send; overflow increments the drop counter.
//
// Start the background flusher with Run(ctx); cancel the context to drain and
// stop, or call Close() to signal the drain and block until the final batch
// has been flushed.
type WebhookSink struct {
	cfg       WebhookConfig
	interval  time.Duration
	baseDelay time.Duration
	queue     chan types.AuditEvent
	drops     atomic.Int64
	client    *http.Client
	// stop is closed by Close() to signal Run to drain and exit independently
	// of the Run ctx; done is closed by Run when it returns so Close can await
	// the final flush.
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewWebhookSink creates a WebhookSink from cfg. Returns an error if cfg.URL
// is empty, durations cannot be parsed, or a bearer_token is configured on a
// non-https URL.
func NewWebhookSink(cfg WebhookConfig) (*WebhookSink, error) {
	cfg = cfg.withDefaults()
	interval, base, timeout, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	return &WebhookSink{
		cfg:       cfg,
		interval:  interval,
		baseDelay: base,
		queue:     make(chan types.AuditEvent, cfg.BufferSize),
		client:    &http.Client{Timeout: timeout},
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}, nil
}

// validate checks cfg and returns the parsed durations its delivery loop
// needs. It runs on a defaulted config (withDefaults first), because an unset
// duration string only parses once the default fills it. Every caller that
// turns a webhook config into a delivery — the fan-out sink and the
// acknowledged loop alike — goes through here, so the refusals cannot drift
// apart.
func (c WebhookConfig) validate() (interval, base, timeout time.Duration, err error) {
	switch c.Delivery {
	case "", DeliveryBestEffort, DeliveryAcknowledged:
	default:
		return 0, 0, 0, fmt.Errorf("sinks.webhook: delivery %q must be %q or %q", c.Delivery, DeliveryBestEffort, DeliveryAcknowledged)
	}
	if c.URL == "" {
		return 0, 0, 0, fmt.Errorf("sinks.webhook: URL is required")
	}
	// The bearer is a long-lived, replayable SIEM credential resent on every
	// POST, so the gate is on the credential, not the scheme: a tokenless
	// http:// collector still boots (matches the syslog sink over plaintext).
	if c.BearerToken != "" {
		u, err := url.Parse(c.URL)
		if err != nil {
			// Do NOT echo the raw URL — it may carry user:pass credentials.
			return 0, 0, 0, fmt.Errorf("sinks.webhook: malformed url (redacted)")
		}
		if !strings.EqualFold(u.Scheme, "https") {
			return 0, 0, 0, fmt.Errorf("sinks.webhook: bearer_token requires an https:// url (scheme %q would send the SIEM credential in cleartext)", u.Scheme)
		}
	}
	interval, err = time.ParseDuration(c.FlushInterval)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("sinks.webhook: invalid flush_interval %q: %w", c.FlushInterval, err)
	}
	base, err = time.ParseDuration(c.RetryBaseDelay)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("sinks.webhook: invalid retry_base_delay %q: %w", c.RetryBaseDelay, err)
	}
	timeout, err = time.ParseDuration(c.Timeout)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("sinks.webhook: invalid timeout %q: %w", c.Timeout, err)
	}
	if timeout <= 0 {
		return 0, 0, 0, fmt.Errorf("sinks.webhook: timeout %q must be positive (zero would never time out a wedged collector)", c.Timeout)
	}
	if timeout > WebhookTimeout {
		return 0, 0, 0, fmt.Errorf("sinks.webhook: timeout %q exceeds %s, the final flush wardynd's shutdown grace waits for", c.Timeout, WebhookTimeout)
	}
	return interval, base, timeout, nil
}

// Name implements audit.Sink.
func (w *WebhookSink) Name() string { return "webhook" }

// Emit enqueues ev for delivery. If the buffer is full the event is dropped
// and the drop counter is incremented. Emit never blocks.
//
// ponytail: at-most-once past the 4096 buffer. The loss is now COUNTED and
// surfaced as wardyn_audit_sink_drops_total{sink} on /metrics, so a SIEM
// falling behind is visible. A per-sink disk spool (reusing api.AuditSpool) that
// drains on recovery is the at-least-once upgrade — deferred: it needs its own
// on-disk lifecycle + backpressure, larger than the metric this POC needs.
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
// into batch and flushes once so the final partial batch is delivered before
// Run exits. It runs under a fresh bounded context rather than the (already
// cancelled) Run ctx, so the last POST can still deliver.
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

// Close signals the background flusher to drain and stop, then blocks until
// the final batch has been flushed. Must only be called after Run has
// started. Safe to call multiple times and concurrently with a ctx-driven
// Run shutdown.
func (w *WebhookSink) Close() error {
	w.closeOnce.Do(func() { close(w.stop) })
	<-w.done
	return nil
}

// Drops returns the number of events dropped due to buffer overflow.
func (w *WebhookSink) Drops() int64 { return w.drops.Load() }

// deliverWithRetry encodes batch as newline-delimited JSON and POSTs it,
// retrying up to cfg.MaxRetries times with exponential backoff. Failures
// after all retries count the lost events in the drop counter; events are
// not re-queued. The backoff sleep honors ctx cancellation and Close()'s
// stop signal so shutdown drains promptly.
func (w *WebhookSink) deliverWithRetry(ctx context.Context, batch []types.AuditEvent) {
	body, err := encodeBatch(batch)
	if err != nil {
		// Encoding failure: count these events as dropped.
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
				// Shutdown via ctx: abandon the batch.
				w.drops.Add(int64(len(batch)))
				return
			case <-w.stop:
				// Shutdown via Close(): abandon the batch.
				w.drops.Add(int64(len(batch)))
				return
			case <-time.After(delay):
			}
			delay *= 2
		} else {
			// Retries exhausted: the batch is lost.
			w.drops.Add(int64(len(batch)))
			slog.Error("sinks.webhook: delivery failed, retries exhausted",
				slog.Int("attempts", w.cfg.MaxRetries),
				slog.Any("err", err),
				slog.Int("batch_size", len(batch)),
				slog.Int64("drops", w.drops.Load()))
		}
	}
}

// post is the sink's half of postNDJSON: the error is all deliverWithRetry
// acts on.
func (w *WebhookSink) post(ctx context.Context, body []byte) error {
	_, _, err := postNDJSON(ctx, w.client, w.cfg, body)
	return err
}

// postNDJSON POSTs body to cfg's URL with the sink's JSON-lines Content-Type
// and bearer rule, returning the collector's status and any Retry-After it
// asked for. A non-2xx status comes back with an error carrying the same text
// the sink has always logged, so its meaning is unchanged for the best-effort
// path while the acknowledged loop can read the status off the error.
//
// The bearer token and any URL userinfo are sent on the wire and never in an
// error, so a caller recording the failure cannot leak either.
func postNDJSON(ctx context.Context, client *http.Client, cfg WebhookConfig, body []byte) (int, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return 0, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("http post: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")),
			fmt.Errorf("http post: unexpected status %d", resp.StatusCode)
	}
	return resp.StatusCode, 0, nil
}

// parseRetryAfter reads a collector's Retry-After in seconds, the only form
// Wardyn documents for it; anything else (an HTTP-date, garbage) reads as 0,
// which backs off on its own schedule instead of guessing.
func parseRetryAfter(h string) time.Duration {
	secs, err := strconv.Atoi(h)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// encodeBatch serialises each event as JSON-lines / NDJSON via marshalEvent,
// so the WARDYN_AUDIT_SOURCE stamp applies as it does for other sinks.
func encodeBatch(batch []types.AuditEvent) ([]byte, error) {
	var buf bytes.Buffer
	for i := range batch {
		b, err := marshalEvent(batch[i])
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}
