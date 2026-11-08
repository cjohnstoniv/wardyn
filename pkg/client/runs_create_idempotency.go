// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// IdempotencyKeyHeader is the request header that makes a run create safe to repeat.
const IdempotencyKeyHeader = "Idempotency-Key"

const (
	reasonKeyInFlight = "idempotency_key_in_flight"

	// createRunAttempts is the most requests one retrying CreateRunWith makes, the first included.
	createRunAttempts = 3
	// maxRetryAfter caps how long a 409 in-flight answer can make the client wait.
	maxRetryAfter = 5 * time.Second
)

// createRunBackoff is the wait before the second and third attempts. A variable so tests do not sleep.
var createRunBackoff = []time.Duration{time.Second, 2 * time.Second}

// CreateRunOptions tunes one CreateRunWith call.
type CreateRunOptions struct {
	// IdempotencyKey is sent as the Idempotency-Key header. Empty means a fresh UUID. Use your
	// own stable value (a CI build and job id) when a re-run of the same job must not start a
	// second run. 1 to 255 visible ASCII characters.
	IdempotencyKey string

	// Retry makes up to three attempts in all, with the same key and the same bytes, when the answer
	// was lost: a transport error, a 409 idempotency_key_in_flight, or a 502, 503 or 504 that carries
	// no Wardyn reason. Off by default, so a call is one request.
	//
	// A retry is safe only when every control-plane replica runs 0.10.0 or later: an older replica
	// ignores the key and makes a second run. Before the first retry that follows a transport
	// error or a proxy 5xx, the client reads /healthz and does not retry unless the server reports
	// idempotent_run_create. That check sees the replica that answers it, not the others behind
	// a load balancer, so a mixed pair is still unsafe.
	Retry bool
}

// CreateRunWith is CreateRun with a chosen key and optional retries. The body is encoded once and
// every attempt sends the same bytes and key. CreateRunResult.Replayed reports an answer replayed
// from an earlier create with the key.
//
// The retry waits 1 s then 2 s, and a 409 in-flight answer waits for its Retry-After (at most
// 5 s). A create still being answered after the third attempt returns the 409. All waits end with ctx.
func (c *Client) CreateRunWith(ctx context.Context, req CreateRunRequest, opts CreateRunOptions) (CreateRunResult, error) {
	var zero CreateRunResult
	body, err := json.Marshal(req)
	if err != nil {
		return zero, fmt.Errorf("marshal request body: %w", err)
	}
	key := opts.IdempotencyKey
	if key == "" {
		key = uuid.NewString()
	}
	probed, supported := false, false
	for attempt := 1; ; attempt++ {
		out, header, err := c.createRunOnce(ctx, body, key)
		if err == nil {
			out.Replayed = header.Get("Idempotent-Replayed") == "true"
			return out, nil
		}
		if !opts.Retry || attempt >= createRunAttempts || ctx.Err() != nil {
			return zero, err
		}
		wait := createRunBackoff[attempt-1]
		var apiErr *APIError
		var te *transportError
		switch {
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict && apiErr.Reason == reasonKeyInFlight:
			// The server knows the key, so there is nothing to probe.
			wait = retryAfter(header, wait)
		case errors.As(err, &te), errors.As(err, &apiErr) && proxyFailure(apiErr):
			if !probed {
				probed, supported = true, c.honoursRunCreateKey(ctx)
			}
			if !supported {
				return zero, fmt.Errorf("%w (the server does not report idempotent_run_create, so the create was not repeated; the run may have been created, so list your runs before launching again)", err)
			}
		default:
			return zero, err
		}
		select {
		case <-ctx.Done():
			return zero, err
		case <-time.After(wait):
		}
	}
}

// transportError marks a failure to get any answer: a timeout, reset or EOF.
type transportError struct{ err error }

func (e *transportError) Error() string { return "http: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// proxyFailure is a 502, 503 or 504 with no Wardyn reason: a gateway's answer, not the server's refusal.
func proxyFailure(e *APIError) bool {
	switch e.Status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return e.Reason == ""
	}
	return false
}

func retryAfter(h http.Header, fallback time.Duration) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || secs < 0 {
		return fallback
	}
	return min(time.Duration(secs)*time.Second, maxRetryAfter)
}

// honoursRunCreateKey reads /healthz for idempotent_run_create. Any failure reads as false.
func (c *Client) honoursRunCreateKey(ctx context.Context) bool {
	raw, err := c.Healthz(ctx)
	if err != nil {
		return false
	}
	var h struct {
		IdempotentRunCreate bool `json:"idempotent_run_create"`
	}
	return json.Unmarshal(raw, &h) == nil && h.IdempotentRunCreate
}

// createRunOnce sends one POST /api/v1/runs. It returns the response header on success and on a
// non-2xx answer, so the caller can read Retry-After.
func (c *Client) createRunOnce(ctx context.Context, body []byte, key string) (CreateRunResult, http.Header, error) {
	var out CreateRunResult
	req, err := c.newRequest(ctx, http.MethodPost, "/api/v1/runs", bytes.NewReader(body), true)
	if err != nil {
		return out, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set(IdempotencyKeyHeader, key)
	// The key would make net/http call this POST replayable, and on a reused connection that resets before the
	// headers the transport would send it again by itself, even with Retry off or against a server that
	// ignores the key. Without GetBody it cannot: every repeat is made by the loop in CreateRunWith.
	req.GetBody = nil
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return out, nil, &transportError{err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return out, resp.Header, NewAPIError(resp.StatusCode, raw)
	}
	// A body cut off after the 201 headers is a lost answer, the same as a reset before them.
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return out, resp.Header, &transportError{err: err}
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return out, resp.Header, fmt.Errorf("decode response: %w", err)
		}
	}
	return out, resp.Header, nil
}
