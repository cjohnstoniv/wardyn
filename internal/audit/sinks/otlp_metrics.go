// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// otlpMetricsPath is the OTLP/HTTP path metrics go to; it sits beside the logs path.
const otlpMetricsPath = "/v1/metrics"

// minOTLPMetricsInterval is the shortest push interval accepted: each push
// composes the whole /metrics body, store ping and health reads included.
const minOTLPMetricsInterval = time.Second

// OTLPMetricsConfig is the otlp_metrics block. It shares no field with the otlp
// block, so metrics can go to a collector that never receives audit logs.
type OTLPMetricsConfig struct {
	// Endpoint is the collector's base URL; the push goes to "<endpoint>/v1/metrics" (required).
	Endpoint string `json:"endpoint"`
	// BearerToken is sent as "Authorization: Bearer <token>"; requires https://.
	BearerToken string `json:"bearer_token,omitempty"`
	// Headers are extra request headers, with the otlp block's rules.
	Headers map[string]string `json:"headers,omitempty"`
	// Timeout bounds each push's HTTP request (default and maximum WebhookTimeout).
	Timeout string `json:"timeout,omitempty"`
	// Interval is how often the series are pushed (default 60s, at least 1s).
	Interval string `json:"interval,omitempty"`
}

// otlpMetricsSettings is a checked otlp_metrics block.
type otlpMetricsSettings struct {
	post     OTLPConfig // the endpoint, credential and headers, checked by the otlp block's rules
	target   string
	interval time.Duration
	timeout  time.Duration
}

// settings checks the block with the otlp block's endpoint, credential, header
// and timeout rules, so its errors never echo the endpoint either.
func (c OTLPMetricsConfig) settings() (otlpMetricsSettings, error) {
	post := OTLPConfig{Endpoint: c.Endpoint, BearerToken: c.BearerToken, Headers: c.Headers, Timeout: c.Timeout}.withDefaults()
	logs, _, _, timeout, err := post.validate()
	if err != nil {
		return otlpMetricsSettings{}, err
	}
	raw := c.Interval
	if raw == "" {
		raw = "60s"
	}
	interval, err := time.ParseDuration(raw)
	if err != nil {
		return otlpMetricsSettings{}, fmt.Errorf("sinks.otlp_metrics: invalid interval %q: %w", raw, err)
	}
	if interval < minOTLPMetricsInterval {
		return otlpMetricsSettings{}, fmt.Errorf("sinks.otlp_metrics: interval %q is below %s (each push reads the whole /metrics body)", raw, minOTLPMetricsInterval)
	}
	return otlpMetricsSettings{post: post, target: signalTarget(logs, otlpMetricsPath), interval: interval, timeout: timeout}, nil
}

// OTLPMetrics pushes the /metrics body to "<endpoint>/v1/metrics" every
// interval, translated to OTLP/HTTP JSON (encodeOTLPMetrics). It reads the body
// a scrape reads, so the two never count separately. A failed push is logged,
// counted in Failures and not retried: the next push carries the same running totals.
type OTLPMetrics struct {
	post     *OTLPSink // the checked headers, credential and HTTP client; its queue is never used
	target   string
	interval time.Duration
	failures atomic.Int64
}

// NewOTLPMetrics returns the exporter when cfgJSON has an otlp_metrics block,
// and nil otherwise. A block that fails its checks is an error, as ParseSinks
// reports it.
func NewOTLPMetrics(cfgJSON []byte) (*OTLPMetrics, error) {
	if len(bytes.TrimSpace(cfgJSON)) == 0 {
		return nil, nil
	}
	cfg, err := decodeConfig(cfgJSON)
	if err != nil {
		return nil, fmt.Errorf("sinks.NewOTLPMetrics: invalid JSON: %w", err)
	}
	if cfg.OTLPMetrics == nil {
		return nil, nil
	}
	s, err := cfg.OTLPMetrics.settings()
	if err != nil {
		return nil, err
	}
	return &OTLPMetrics{
		post:     &OTLPSink{cfg: s.post, client: &http.Client{Timeout: s.timeout, CheckRedirect: keepPOST}},
		target:   s.target,
		interval: s.interval,
	}, nil
}

// Failures is the number of pushes that did not reach the collector: an encode
// error, a transport error, a non-2xx reply or a panic. A push cut short by
// shutdown is not counted.
func (m *OTLPMetrics) Failures() int64 { return m.failures.Load() }

// Run pushes the text collect returns once per interval until ctx is done.
// There is no last push at shutdown: as with a scrape, increments after the
// last push are not sent.
func (m *OTLPMetrics) Run(ctx context.Context, collect func(context.Context) []byte) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.push(ctx, collect)
		}
	}
}

// push sends one export. A panic while the body is composed or translated is
// logged and counted, and the next tick tries again, as a panicking scrape is
// one failed scrape.
func (m *OTLPMetrics) push(ctx context.Context, collect func(context.Context) []byte) {
	defer func() {
		if r := recover(); r != nil {
			m.failures.Add(1)
			slog.Error("sinks.otlp_metrics: push panicked", slog.Any("panic", r))
		}
	}()
	text := collect(ctx)
	body, n, err := encodeOTLPMetrics(text, strconv.FormatInt(time.Now().UnixNano(), 10))
	if err != nil {
		m.failures.Add(1)
		slog.Error("sinks.otlp_metrics: encode failed", slog.Any("err", err))
		return
	}
	if n == 0 {
		return
	}
	rejected, err := m.post.postTo(ctx, m.target, body, signalMetrics)
	if err != nil && ctx.Err() != nil {
		return // shutdown cut the push short; that is not a failed push
	}
	if err != nil {
		m.failures.Add(1)
		slog.Warn("sinks.otlp_metrics: push failed; the next push carries the same totals", slog.Any("err", err))
		return
	}
	if rejected > 0 {
		slog.Warn("sinks.otlp_metrics: collector rejected metric data points", slog.Int64("rejected", rejected))
	}
}
