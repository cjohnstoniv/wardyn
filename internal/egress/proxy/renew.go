// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenSource holds the per-run token. The renew loop rotates it IN PLACE, so
// every control-plane caller (decision sink, injector, approval client, brokered
// local routes) reads the CURRENT token instead of a string captured at startup.
//
// This is deliberately an explicit holder rather than an Authorization-injecting
// http.RoundTripper: the run token must NEVER reach a forward-egress or
// upstream-corp-proxy dial, and a transport that adds the header for us would
// make that leak one accidental client reuse away. Reading it at the call sites
// that already build control-plane requests keeps the blast radius visible.
type tokenSource struct {
	mu  sync.RWMutex
	tok string
}

func newTokenSource(tok string) *tokenSource { return &tokenSource{tok: tok} }

// Get returns the current token. A nil source yields "" (safe for tests and for
// paths where no token was configured).
func (t *tokenSource) Get() string {
	if t == nil {
		return ""
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.tok
}

// Set installs a freshly renewed token. Subsequent Get calls see it.
func (t *tokenSource) Set(tok string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.tok = tok
	t.mu.Unlock()
}

const (
	// renewRetry is how soon a FAILED renew is retried. Short enough that a brief
	// control-plane blip never burns the token's remaining life.
	renewRetry = time.Minute
	// renewMaxInterval caps the gap between renews so a (hypothetically) very long
	// TTL still re-checks authority — revocation and terminal state are only
	// re-evaluated at renew, so this is the ceiling on how stale that check gets.
	renewMaxInterval = 30 * time.Minute
	// renewTimeout bounds one renew request.
	renewTimeout = 10 * time.Second
)

// runTokenRenewer keeps ts populated with a FRESH run token for as long as ctx
// lives. It mirrors wardynd's ground-truth token rotator: renew, then sleep half
// the new token's remaining life (clamped to [renewRetry, renewMaxInterval]) so a
// missed or failed tick never leaves an expired token in place.
//
// The control plane — not this loop — decides whether renewal is still allowed:
// a revoked or terminal run is refused there, and the loop simply keeps failing
// (loudly, in the log) with the old token until the sidecar is torn down. That
// keeps the authority decision on the trusted side and this loop dumb.
//
// ponytail: renews once immediately at startup rather than decoding the token's
// exp to schedule the first tick. It costs one extra mint per run and, in
// exchange, needs no JWT parsing here and proves the renew path works at startup
// instead of failing an hour in. Decode exp only if that mint ever shows up as a
// real cost.
func runTokenRenewer(ctx context.Context, ts *tokenSource, base string, client *http.Client) {
	for {
		next := renewRetry
		rctx, cancel := context.WithTimeout(ctx, renewTimeout)
		tok, exp, err := renewToken(rctx, base, ts.Get(), client)
		cancel()
		if err != nil {
			// Do not log the token; the error carries status + body only.
			slog.ErrorContext(ctx, "wardyn-proxy: run token renew failed, retrying",
				slog.Duration("retry_in", renewRetry), slog.Any("err", err))
		} else {
			ts.Set(tok)
			next = renewMaxInterval
			if half := time.Until(exp) / 2; half < renewRetry {
				next = renewRetry
			} else if half < renewMaxInterval {
				next = half
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(next):
		}
	}
}

// renewToken POSTs /api/v1/internal/token/renew with the CURRENT run token and
// returns the fresh token plus its expiry. Any non-200 (revoked run, terminal
// run, store unavailable) is an error: the caller keeps the old token and retries
// rather than dropping to no credential at all.
func renewToken(ctx context.Context, base, token string, client *http.Client) (string, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/api/v1/internal/token/renew", nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("renew request: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return "", time.Time{}, fmt.Errorf("renew status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", time.Time{}, fmt.Errorf("decode renew: %w", err)
	}
	if out.Token == "" {
		return "", time.Time{}, errors.New("renew returned an empty token")
	}
	exp, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse renew expires_at: %w", err)
	}
	return out.Token, exp, nil
}
