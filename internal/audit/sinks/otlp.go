// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// otlpLogsPath is the OTLP/HTTP path logs go to, appended to the configured
// endpoint unless it already ends there.
const otlpLogsPath = "/v1/logs"

// OTLPConfig holds the configuration for an OTLPSink. The delivery knobs
// (batching, buffer, retries, timeout) mean what they mean on the webhook sink.
type OTLPConfig struct {
	// Endpoint is the collector's base URL, for example https://otel.example.com:4318;
	// "/v1/logs" is added when the path does not already end in it (required).
	Endpoint string `json:"endpoint"`
	// BearerToken is sent as "Authorization: Bearer <token>"; requires https://.
	BearerToken string `json:"bearer_token,omitempty"`
	// Headers are extra request headers, for example a tenant id. They are sent
	// as written, so a secret in one travels in cleartext over an http:// endpoint;
	// put a credential in BearerToken, which refuses http://.
	Headers map[string]string `json:"headers,omitempty"`
	// BatchSize is the maximum number of log records per POST (default 100).
	BatchSize int `json:"batch_size,omitempty"`
	// FlushInterval is how long to wait before flushing a partial batch (default 5s).
	FlushInterval string `json:"flush_interval,omitempty"`
	// BufferSize is the capacity of the in-process event queue (default 4096);
	// events that would overflow it are dropped and counted.
	BufferSize int `json:"buffer_size,omitempty"`
	// MaxRetries is the number of delivery attempts per batch (default 3).
	MaxRetries int `json:"max_retries,omitempty"`
	// RetryBaseDelay is the initial backoff delay (default 200ms).
	RetryBaseDelay string `json:"retry_base_delay,omitempty"`
	// Timeout bounds each HTTP request, including the final flush on shutdown
	// (default and maximum WebhookTimeout, the budget wardynd's shutdown grace is sized on).
	Timeout string `json:"timeout,omitempty"`
	// Traces, when true, also sends one span per run-scoped audit row to
	// "<endpoint>/v1/traces", and puts the span's trace and span ids on the row's
	// log record. Off by default: a collector that takes logs only sees no change.
	Traces bool `json:"traces,omitempty"`
}

func (c OTLPConfig) withDefaults() OTLPConfig {
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.FlushInterval == "" {
		c.FlushInterval = "5s"
	}
	if c.BufferSize <= 0 {
		c.BufferSize = 4096
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if c.RetryBaseDelay == "" {
		c.RetryBaseDelay = "200ms"
	}
	if c.Timeout == "" {
		c.Timeout = WebhookTimeout.String()
	}
	return c
}

// validate checks a defaulted config and returns the collector URL to POST to
// and the parsed durations the delivery loop needs. Its errors are plain
// sentences and never echo the endpoint, which may carry credentials.
func (c OTLPConfig) validate() (target string, interval, base, timeout time.Duration, err error) {
	if c.Endpoint == "" {
		return "", 0, 0, 0, fmt.Errorf("sinks.otlp: endpoint is required")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("sinks.otlp: malformed endpoint (redacted)")
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "", 0, 0, 0, fmt.Errorf("sinks.otlp: endpoint must be an http:// or https:// url (scheme %q)", u.Scheme)
	}
	if u.Hostname() == "" {
		return "", 0, 0, 0, fmt.Errorf("sinks.otlp: endpoint has no host")
	}
	if c.BearerToken != "" && !strings.EqualFold(u.Scheme, "https") {
		return "", 0, 0, 0, fmt.Errorf("sinks.otlp: bearer_token requires an https:// endpoint (scheme %q would send the credential in cleartext)", u.Scheme)
	}
	if err := c.validateHeaders(); err != nil {
		return "", 0, 0, 0, err
	}
	interval, err = time.ParseDuration(c.FlushInterval)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("sinks.otlp: invalid flush_interval %q: %w", c.FlushInterval, err)
	}
	base, err = time.ParseDuration(c.RetryBaseDelay)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("sinks.otlp: invalid retry_base_delay %q: %w", c.RetryBaseDelay, err)
	}
	timeout, err = time.ParseDuration(c.Timeout)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("sinks.otlp: invalid timeout %q: %w", c.Timeout, err)
	}
	if interval <= 0 {
		return "", 0, 0, 0, fmt.Errorf("sinks.otlp: flush_interval %q must be positive (zero would poll in a tight loop)", c.FlushInterval)
	}
	if base <= 0 {
		return "", 0, 0, 0, fmt.Errorf("sinks.otlp: retry_base_delay %q must be positive (zero would retry a failing collector without pause)", c.RetryBaseDelay)
	}
	if timeout <= 0 {
		return "", 0, 0, 0, fmt.Errorf("sinks.otlp: timeout %q must be positive (zero would never time out a wedged collector)", c.Timeout)
	}
	if timeout > WebhookTimeout {
		return "", 0, 0, 0, fmt.Errorf("sinks.otlp: timeout %q exceeds %s, the final flush wardynd's shutdown grace waits for", c.Timeout, WebhookTimeout)
	}
	if ep := u.EscapedPath(); !strings.HasSuffix(ep, otlpLogsPath) {
		setEscapedPath(u, strings.TrimRight(ep, "/")+otlpLogsPath)
	}
	return u.String(), interval, base, timeout, nil
}

// validateHeaders refuses a header the sink would otherwise silently fail to
// send for every batch: an empty or malformed name, a value with a line break,
// or one the sink sets itself.
func (c OTLPConfig) validateHeaders() error {
	for name, value := range c.Headers {
		if name == "" || strings.ContainsAny(name, " \t\r\n:") {
			return fmt.Errorf("sinks.otlp: header name %q is not a valid header name", name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("sinks.otlp: header %q has a line break in its value", name)
		}
		switch strings.ToLower(name) {
		case "authorization":
			return fmt.Errorf("sinks.otlp: set the credential in bearer_token, not in a header named %q", name)
		case "content-type", "content-length", "host":
			return fmt.Errorf("sinks.otlp: header %q is set by the sink and cannot be overridden", name)
		}
	}
	return nil
}

// OTLPSink buffers audit events and delivers them to an OpenTelemetry collector
// as OTLP/HTTP JSON log records. Like the webhook sink it never blocks Emit
// beyond a non-blocking channel send; overflow and exhausted retries add to the
// drop counter (wardyn_audit_sink_drops_total{sink="otlp"}). Delivery is
// best-effort, not lossless.
//
// Start the background flusher with Run(ctx), or call Close() to drain and
// block until the final batch has been flushed.
type OTLPSink struct {
	cfg       OTLPConfig
	target    string
	traces    string // "<endpoint>/v1/traces", or "" when cfg.Traces is off
	interval  time.Duration
	baseDelay time.Duration
	queue     chan types.AuditEvent
	drops     atomic.Int64
	client    *http.Client
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewOTLPSink creates an OTLPSink from cfg, refusing a config that is missing
// an endpoint, has unparseable durations, or sends a bearer over http://.
func NewOTLPSink(cfg OTLPConfig) (*OTLPSink, error) {
	cfg = cfg.withDefaults()
	target, interval, base, timeout, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	var traces string
	if cfg.Traces {
		traces = tracesTarget(target)
	}
	return &OTLPSink{
		cfg:       cfg,
		target:    target,
		traces:    traces,
		interval:  interval,
		baseDelay: base,
		queue:     make(chan types.AuditEvent, cfg.BufferSize),
		client:    &http.Client{Timeout: timeout, CheckRedirect: keepPOST},
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}, nil
}

// Name implements audit.Sink.
func (o *OTLPSink) Name() string { return "otlp" }

// Emit enqueues ev for delivery; a full buffer drops it and counts the drop.
// Emit never blocks.
func (o *OTLPSink) Emit(_ context.Context, ev types.AuditEvent) error {
	select {
	case o.queue <- ev:
	default:
		// A shed row loses its log record and, with traces on, its span too.
		lost := int64(1)
		if o.traces != "" && hasTrace(ev) {
			lost = 2
		}
		o.drops.Add(lost)
		slog.Warn("sinks.otlp: queue overflow", slog.Int64("drops", o.drops.Load()))
	}
	return nil
}

// Drops returns the number of events lost to buffer overflow or failed delivery.
func (o *OTLPSink) Drops() int64 { return o.drops.Load() }

// Run starts the background flush loop and returns when ctx is cancelled or
// Close() is called, after a best-effort flush of what is still buffered. Run
// must be called exactly once per OTLPSink.
func (o *OTLPSink) Run(ctx context.Context) {
	defer close(o.done)

	ticker := time.NewTicker(o.interval)
	defer ticker.Stop()

	batch := make([]types.AuditEvent, 0, o.cfg.BatchSize)
	flush := func(fctx context.Context) {
		if len(batch) == 0 {
			return
		}
		o.deliverWithRetry(fctx, batch)
		batch = batch[:0]
	}

	for {
		select {
		case ev := <-o.queue:
			batch = append(batch, ev)
			if len(batch) >= o.cfg.BatchSize {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
		case <-ctx.Done():
			o.drainAndFlush(&batch, flush)
			return
		case <-o.stop:
			o.drainAndFlush(&batch, flush)
			return
		}
	}
}

// drainAndFlush sends everything still queued, BatchSize records at a time,
// under a fresh bounded context because Run's own may already be cancelled.
func (o *OTLPSink) drainAndFlush(batch *[]types.AuditEvent, flush func(context.Context)) {
	fctx, cancel := context.WithTimeout(context.Background(), o.client.Timeout)
	defer cancel()
	for {
		select {
		case ev := <-o.queue:
			*batch = append(*batch, ev)
			if len(*batch) >= o.cfg.BatchSize {
				flush(fctx)
			}
		default:
			flush(fctx)
			return
		}
	}
}

// Close signals the flusher to drain and stop, and blocks until the final
// batch has been flushed. Call it only after Run has started; it is safe to
// call more than once.
func (o *OTLPSink) Close() error {
	o.closeOnce.Do(func() { close(o.stop) })
	<-o.done
	return nil
}

// deliverWithRetry sends batch as logs and, when traces are on, its run-scoped
// rows as spans. Each signal is delivered and counted on its own.
func (o *OTLPSink) deliverWithRetry(ctx context.Context, batch []types.AuditEvent) {
	body, err := encodeOTLPLogs(batch, o.traces != "")
	if err != nil {
		o.dropEncode(len(batch), err)
	} else {
		o.sendWithRetry(ctx, o.target, body, len(batch))
	}
	if o.traces == "" {
		return
	}
	spans, n, err := encodeOTLPSpans(batch)
	switch {
	case err != nil:
		o.dropEncode(n, err)
	case n > 0:
		o.sendWithRetry(ctx, o.traces, spans, n)
	}
}

func (o *OTLPSink) dropEncode(n int, err error) {
	o.drops.Add(int64(n))
	slog.Error("sinks.otlp: encode batch failed",
		slog.Any("err", err), slog.Int("dropped_events", n), slog.Int64("drops", o.drops.Load()))
}

// sendWithRetry POSTs body to target, which carries n records, retrying up to
// MaxRetries times with doubling backoff. A request that still fails, or is
// abandoned by shutdown mid-backoff, adds n to the drop counter and is not
// re-queued.
func (o *OTLPSink) sendWithRetry(ctx context.Context, target string, body []byte, n int) {
	delay := o.baseDelay
	for attempt := 1; attempt <= o.cfg.MaxRetries; attempt++ {
		rejected, err := o.postTo(ctx, target, body, target == o.traces)
		if err == nil {
			o.countRejected(rejected, n)
			return
		}
		if attempt == o.cfg.MaxRetries {
			o.drops.Add(int64(n))
			slog.Error("sinks.otlp: delivery failed, retries exhausted",
				slog.Int("attempts", o.cfg.MaxRetries), slog.Any("err", err),
				slog.Int("batch_size", n), slog.Int64("drops", o.drops.Load()))
			return
		}
		slog.Warn("sinks.otlp: delivery attempt failed, retrying",
			slog.Int("attempt", attempt), slog.Int("max_retries", o.cfg.MaxRetries),
			slog.Any("err", err), slog.Duration("retry_in", delay))
		select {
		case <-ctx.Done():
			o.drops.Add(int64(n))
			return
		case <-o.stop:
			o.drops.Add(int64(n))
			return
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// countRejected adds the records a collector refused inside an accepted request
// (a 2xx with partialSuccess) to the drop counter. The accepted records are not
// retried. Only the count is logged: the collector's errorMessage is free text
// from outside Wardyn and stays out of the log.
func (o *OTLPSink) countRejected(rejected int64, batchLen int) {
	if rejected <= 0 {
		return
	}
	rejected = min(rejected, int64(batchLen))
	o.drops.Add(rejected)
	slog.Warn("sinks.otlp: collector rejected records",
		slog.Int64("rejected", rejected), slog.Int64("drops", o.drops.Load()))
}

// post sends one OTLP/HTTP JSON request and returns how many records the
// collector reports rejecting in a 2xx reply's partialSuccess. A malformed or
// empty reply body counts as none. Neither the bearer token nor the
// endpoint (net/http's *url.Error embeds it, query included) appears in the
// returned error.
func (o *OTLPSink) post(ctx context.Context, body []byte) (int64, error) {
	return o.postTo(ctx, o.target, body, false)
}

// postTo is post to an explicit collector URL: the logs target, or the traces one when spans is true.
func (o *OTLPSink) postTo(ctx context.Context, target string, body []byte, spans bool) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", withoutURL(err))
	}
	for name, value := range o.cfg.Headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+o.cfg.BearerToken)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("http post: %w", withoutURL(err))
	}
	// Read what a small reply carries before closing, so keep-alive reuses the connection.
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("http post: unexpected status %d", resp.StatusCode)
	}
	return rejectedRecords(reply, spans), nil
}

// rejectedRecords reads partialSuccess.rejectedSpans from an
// ExportTraceServiceResponse when spans is true, else
// partialSuccess.rejectedLogRecords from an ExportLogsServiceResponse. The
// protocol's JSON mapping writes the int64 as a string; a number is read too.
// Anything unparseable reads as 0.
func rejectedRecords(reply []byte, spans bool) int64 {
	var r struct {
		PartialSuccess struct {
			RejectedLogs  json.Number `json:"rejectedLogRecords"`
			RejectedSpans json.Number `json:"rejectedSpans"`
		} `json:"partialSuccess"`
	}
	if json.Unmarshal(reply, &r) != nil {
		return 0
	}
	rejected := r.PartialSuccess.RejectedLogs
	if spans {
		rejected = r.PartialSuccess.RejectedSpans
	}
	n, err := rejected.Int64()
	if err != nil {
		return 0
	}
	return n
}
