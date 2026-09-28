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

// tokenSource holds the per-run token; the renew loop rotates it IN PLACE so every caller reads the
// CURRENT token, not one captured at startup.
//
// SECURITY: deliberately an explicit holder rather than an Authorization-injecting http.RoundTripper —
// the run token must NEVER reach a forward-egress or upstream-corp-proxy dial, and an auto-adding
// transport would make that leak one accidental client reuse away.
type tokenSource struct {
	mu  sync.RWMutex
	tok string
}

func newTokenSource(tok string) *tokenSource { return &tokenSource{tok: tok} }

// Get returns the current token; a nil source yields "" (safe when no token is configured).
func (t *tokenSource) Get() string {
	if t == nil {
		return ""
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.tok
}

// Set installs a freshly renewed token, visible to subsequent Get calls.
func (t *tokenSource) Set(tok string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.tok = tok
	t.mu.Unlock()
}

const (
	renewRetry       = time.Minute      // first gap after a failed renew, and the floor on every gap
	renewMaxInterval = 30 * time.Minute // caps the gap between renews and the failure-backoff ceiling
	renewTimeout     = 10 * time.Second // bounds one renew request
	// renewGiveUpAfter mirrors the identity provider's tokenTTL (1h), spelled here rather than imported
	// since this sidecar never parses the JWT; past it the token being renewed is dead regardless of cause.
	renewGiveUpAfter = time.Hour
)

// renewStatusError is renewToken's typed refusal, carrying the control plane's status so the loop can
// tell apart: 403 (post-auth, run gone/terminal — PERMANENT, give up at once), 401 (refused but NOT proof
// of revocation — a Postgres blip looks identical, so back off and keep trying), and 5xx/transport (blip,
// same backoff).
type renewStatusError struct {
	Status int
	Body   string
}

func (e *renewStatusError) Error() string {
	return fmt.Sprintf("renew status %d: %s", e.Status, e.Body)
}

// permanent reports whether the control plane has ANSWERED, post-auth, that this run may never renew
// again. A 401 is deliberately NOT permanent.
func (e *renewStatusError) permanent() bool { return e.Status == http.StatusForbidden }

// renewerTuning is the loop's timing, held in one struct so tests can drive it at millisecond scale.
type renewerTuning struct {
	firstRetry  time.Duration
	maxInterval time.Duration
	giveUpAfter time.Duration
	now         func() time.Time
}

func defaultRenewerTuning() renewerTuning {
	return renewerTuning{
		firstRetry:  renewRetry,
		maxInterval: renewMaxInterval,
		giveUpAfter: renewGiveUpAfter,
		now:         time.Now,
	}
}

// runTokenRenewer keeps ts populated with a FRESH run token for as long as ctx lives: renew, then sleep
// half the new token's remaining life (clamped to [renewRetry, renewMaxInterval]).
//
// The control plane decides whether renewal is still allowed; the loop decides how long to keep ASKING,
// since retrying forever on any failure would flood the audit log with auth.fail rows from a run that will
// never renew again: a post-auth 403 is PERMANENT (stop at once); a 401 or 5xx/transport failure backs off
// exponentially (a revoked token and a store blip are indistinguishable here, and giving up on the first
// 401 would brick every healthy long run's /internal/* calls on a Postgres flicker); past giveUpAfter since
// the last token actually held, it stops — one log line, since the sidecar has no audit writer, and it
// keeps running on a dead identity until the control plane's lapsed-token sweep removes it.
//
// Renews once immediately at startup rather than decoding the token's exp, trading one extra mint per run
// for no JWT parsing here.
func runTokenRenewer(ctx context.Context, ts *tokenSource, base string, client *http.Client) {
	runTokenRenewerTuned(ctx, ts, base, client, defaultRenewerTuning())
}

// runTokenRenewerTuned is runTokenRenewer with its timing injected; see renewerTuning.
func runTokenRenewerTuned(ctx context.Context, ts *tokenSource, base string, client *http.Client, tune renewerTuning) {
	lastGood := tune.now() // the startup token is the first "last good token"
	backoff := tune.firstRetry
	for {
		rctx, cancel := context.WithTimeout(ctx, renewTimeout)
		tok, exp, err := renewToken(rctx, base, ts.Get(), client)
		cancel()
		var next time.Duration
		if err != nil {
			var se *renewStatusError
			if errors.As(err, &se) && se.permanent() {
				slog.ErrorContext(ctx, "wardyn-proxy: run token renew refused permanently, giving up",
					slog.Int("status", se.Status), slog.Any("err", err))
				return
			}
			if held := tune.now().Sub(lastGood); held >= tune.giveUpAfter {
				slog.ErrorContext(ctx, "wardyn-proxy: run token renew has failed for longer than a token's lifetime, giving up",
					slog.Duration("failing_for", held), slog.Any("err", err))
				return
			}
			next = backoff
			// SECURITY: do not log the token; the error carries status + body only.
			slog.ErrorContext(ctx, "wardyn-proxy: run token renew failed, backing off",
				slog.Duration("retry_in", next), slog.Any("err", err))
			if backoff = backoff * 2; backoff > tune.maxInterval {
				backoff = tune.maxInterval
			}
		} else {
			ts.Set(tok)
			lastGood = tune.now()
			backoff = tune.firstRetry
			next = tune.maxInterval
			if half := time.Until(exp) / 2; half < tune.firstRetry {
				next = tune.firstRetry
			} else if half < tune.maxInterval {
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

// renewToken POSTs /api/v1/internal/token/renew with the CURRENT run token and returns the fresh token
// plus its expiry. Any non-200 becomes a *renewStatusError carrying status, so the caller can tell a
// permanent refusal from a blip.
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
		return "", time.Time{}, &renewStatusError{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
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
