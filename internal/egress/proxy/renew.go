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
	// renewRetry is the FIRST gap after a failed renew, and the floor on every
	// gap. Short enough that a brief control-plane blip never burns the token's
	// remaining life.
	renewRetry = time.Minute
	// renewMaxInterval caps the gap between renews so a (hypothetically) very long
	// TTL still re-checks authority — revocation and terminal state are only
	// re-evaluated at renew, so this is the ceiling on how stale that check gets.
	// It is also the ceiling the failure backoff grows to.
	renewMaxInterval = 30 * time.Minute
	// renewTimeout bounds one renew request.
	renewTimeout = 10 * time.Second
	// renewGiveUpAfter is how long the loop keeps trying a refused renew before it
	// stops: the lifetime of the last token it successfully held. It MIRRORS the
	// embedded identity provider's tokenTTL (1h — internal/identity/embedded's
	// tokenTTL), and is spelled here rather than imported because this sidecar
	// deliberately never parses the JWT (see renewToken) and so holds no `exp` of
	// its own. Past it the token it is renewing is dead whatever the control plane
	// meant by refusing, so retrying is pure noise: one audit row a minute at the
	// control plane, for a run whose credentials expired an hour ago.
	renewGiveUpAfter = time.Hour
)

// renewStatusError is renewToken's typed refusal, carrying the control plane's
// status so the loop can tell three different failures apart:
//
//   - 403: handleInternalTokenRenew's OWN post-auth refusals — the run is gone
//     or terminal. Permanent, and knowable: give up at once.
//   - 401: internalAuth refused the presented token. NOT proof of revocation —
//     the embedded provider treats any RevocationStore error as revoked, so a
//     Postgres blip answers exactly like a real revocation. Back off and keep
//     trying until the last good token's lifetime has passed.
//   - 5xx / transport: a blip. Same backoff.
type renewStatusError struct {
	Status int
	Body   string
}

func (e *renewStatusError) Error() string {
	return fmt.Sprintf("renew status %d: %s", e.Status, e.Body)
}

// permanent reports whether the control plane has ANSWERED, post-authentication,
// that this run may never renew again (run not found, run terminal). A 401 is
// deliberately NOT permanent — see the type's doc.
func (e *renewStatusError) permanent() bool { return e.Status == http.StatusForbidden }

// renewerTuning is the loop's timing, held in one struct so the tests can drive
// the REAL loop at millisecond scale instead of asserting a 1h horizon by
// reading the code. Production always uses defaultRenewerTuning.
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

// runTokenRenewer keeps ts populated with a FRESH run token for as long as ctx
// lives. It mirrors wardynd's ground-truth token rotator: renew, then sleep half
// the new token's remaining life (clamped to [renewRetry, renewMaxInterval]) so a
// missed or failed tick never leaves an expired token in place.
//
// The control plane — not this loop — decides whether renewal is still allowed:
// a revoked or terminal run is refused there. What the loop decides is how long
// to keep ASKING: retrying forever at a fixed interval on any failure floods the
// audit log with `auth.failed` rows from a run whose token the control plane
// will never renew again, burying real security events outside the console's
// retention window. So:
//
//   - a post-auth 403 (run not found / terminal — handleInternalTokenRenew's own
//     refusals) is PERMANENT and knowable: stop at once;
//   - a 401 or a 5xx/transport failure backs off exponentially from
//     firstRetry to maxInterval, because at this end a revoked token and a
//     store blip are indistinguishable (internalAuth refuses both before the
//     post-auth arms run) and giving up on the first 401 would brick every
//     healthy long run's /internal/* calls the moment a Postgres read flickered;
//   - past giveUpAfter since the last token it actually held, it stops: the
//     token being renewed is dead by then whatever the refusal meant.
//
// Giving up is ONE log line and nothing else. The sidecar has no audit writer,
// and the control plane owns the audit story for this case (internalAuth's
// run.identity.expired row) — the sidecar keeps running on a dead identity,
// visibly, rather than quietly hammering a door that will not open, until the
// control plane's lapsed-token sweep removes it (internal/api/run_lost.go).
//
// renews once immediately at startup rather than decoding the token's
// exp to schedule the first tick. It costs one extra mint per run and, in
// exchange, needs no JWT parsing here and proves the renew path works at startup
// instead of failing an hour in. Decode exp only if that mint ever shows up as a
// real cost.
func runTokenRenewer(ctx context.Context, ts *tokenSource, base string, client *http.Client) {
	runTokenRenewerTuned(ctx, ts, base, client, defaultRenewerTuning())
}

// runTokenRenewerTuned is runTokenRenewer with its timing injected; see
// renewerTuning.
func runTokenRenewerTuned(ctx context.Context, ts *tokenSource, base string, client *http.Client, tune renewerTuning) {
	// The startup token is the first "last good token": its lifetime is what the
	// give-up horizon is measured against until a renew succeeds.
	lastGood := tune.now()
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
			// Do not log the token; the error carries status + body only.
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

// renewToken POSTs /api/v1/internal/token/renew with the CURRENT run token and
// returns the fresh token plus its expiry. Any non-200 (revoked run, terminal
// run, store unavailable) is an error: the caller keeps the old token and decides
// whether to retry rather than dropping to no credential at all. A non-200
// carries its status as a *renewStatusError, which is what lets the caller tell a
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
