// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"
)

// Recover mode's cadence (#4082): a proxy whose token can no longer renew asks the recover door about once a minute,
// spread by up to half a minute either way so proxies that lost the control plane together do not ask together.
const (
	recoverEvery  = time.Minute
	recoverJitter = 30 * time.Second
)

// recoverToken POSTs /api/v1/internal/token/recover with the held, expired run token, the request shape of
// renewToken: a recovered run gets a fresh token and its expiry. Any non-200 is a *renewStatusError.
func recoverToken(ctx context.Context, base, token string, client *http.Client) (string, time.Time, error) {
	return tokenDoor(ctx, base, "/api/v1/internal/token/recover", token, client)
}

// uniformJitter is a uniform duration in [-d, d].
func uniformJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(2*d+1))) - d
}

// recoverGap is the wait between two recover asks: recoverEvery plus a uniform jitter of up to ±recoverJitter.
func (tune renewerTuning) recoverGap() time.Duration {
	jitter := tune.jitter
	if jitter == nil {
		jitter = uniformJitter
	}
	return tune.recoverEvery + jitter(tune.recoverJitter)
}

// isRecoverStatus reports whether err is a door's answer with status.
func isRecoverStatus(err error, status int) bool {
	var se *renewStatusError
	return errors.As(err, &se) && se.Status == status
}

// recoverFinal reports a recover answer that can never change: 403 (the run may never recover) or 501 (the server
// cannot recover). Every other answer, a status not named here included, is retryable.
func recoverFinal(err error) bool {
	return isRecoverStatus(err, http.StatusForbidden) || isRecoverStatus(err, http.StatusNotImplemented)
}

// recoverAnswer applies one recover answer for the gated renewer: a recovery installs the token and reopens g (a
// paused gate opens again); a 409 asks the caller to renew next, after tune.firstRetry; a final answer lapses g and
// stops the loop; anything else keeps asking after recoverGap. stop is true when the loop must end.
func recoverAnswer(ctx context.Context, ts *tokenSource, g *lapseGate, sent time.Duration, tok string, err error, tune renewerTuning) (next time.Duration, stop bool) {
	switch {
	case err == nil:
		if !g.renewed(sent, func() { ts.Set(tok) }) {
			return 0, true
		}
		slog.InfoContext(ctx, "wardyn-proxy: run token recovered after the control plane came back")
		return 0, false
	case isRecoverStatus(err, http.StatusConflict):
		return tune.firstRetry, false
	case recoverFinal(err):
		slog.ErrorContext(ctx, "wardyn-proxy: run token recovery refused for good, lapsing", slog.Any("err", err))
		g.mu.Lock()
		g.cause = renewCause(err, ts.Get())
		g.lapseLocked(LapseRenewRefused)
		g.mu.Unlock()
		return 0, true
	default:
		next = tune.recoverGap()
		g.failed(renewCause(err, ts.Get()), addSat(g.clock.Now(), next))
		return next, false
	}
}
