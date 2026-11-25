// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	gh "github.com/google/go-github/v88/github"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	githubRevokeEvery      = 30 * time.Second // the backstop tick; a RevokeRun kicks the worker at once
	githubRevokePassBound  = 20 * time.Second // one pass: its claim and its calls
	githubRevokeInFlight   = 4                // DELETE /installation/token calls at once
	githubRevokePage       = 16               // rows one pass claims
	githubRevokeFirstRetry = 30 * time.Second // doubled per failed attempt
	githubRevokeMaxRetry   = 10 * time.Minute
	// githubRevokeWriteBound bounds a row update or audit write after a call, on a context that a spent pass or a
	// shutdown does not cancel.
	githubRevokeWriteBound = 5 * time.Second
)

// The target_revoke a credential.revoke row gives a github_token jti.
const (
	targetRevokeQueued = "queued"
	targetRevokeNone   = "none"
)

// The target_outcome of a github_token.revoke row.
const (
	targetRevoked        = "revoked"
	targetAlreadyInvalid = "already_invalid"
	targetExpired        = "expired"
	targetUnknown        = "unknown"
	targetFailed         = "failed"
	targetUnsealable     = "unsealable"
)

// errSealUnopenable is a sealed copy its key opens but its AAD or bytes do not match: it can never be presented.
var errSealUnopenable = errors.New("broker: the sealed github token cannot be opened")

// githubRevokeStates answers each sealed jti of runID (jtis nil: every one) with its target_revoke: queued (held,
// unexpired), expired (held past its expiry, or ended expired: never called), handed_back or abandoned (ended so). A jti
// with no row is absent; the caller reads that as none. Read only.
func (b *Broker) githubRevokeStates(ctx context.Context, runID uuid.UUID, jtis []string) (map[string]string, error) {
	rows, err := b.tokenSeal.sealedJTIs(ctx, runID)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, j := range jtis {
		want[j] = true
	}
	now := time.Now()
	out := make(map[string]string, len(rows))
	for jti, st := range rows {
		if jtis != nil && !want[jti] {
			continue
		}
		switch {
		case st.outcome != "":
			out[jti] = st.outcome
		case !st.expiresAt.After(now):
			out[jti] = sealExpired
		default:
			out[jti] = targetRevokeQueued
		}
	}
	return out, nil
}

// queueGitHubRevokes asks for the hand-back of runID's held tokens (jtis nil: every one) and wakes the worker. It
// makes no GitHub call.
func (b *Broker) queueGitHubRevokes(ctx context.Context, runID uuid.UUID, jtis []string) error {
	if err := b.tokenSeal.queue(ctx, runID, jtis); err != nil {
		return err
	}
	select {
	case b.revokeKick <- struct{}{}:
	default:
	}
	return nil
}

// RunGitHubTokenRevokes hands queued GitHub installation tokens back to GitHub until ctx ends: one pass at start, on
// each RevokeRun's kick and on each githubRevokeEvery tick. It returns at once on a broker with no token seal. A pass
// that panics is contained; the next runs as usual.
func (b *Broker) RunGitHubTokenRevokes(ctx context.Context) {
	if b.tokenSeal == nil {
		return
	}
	tick := time.NewTicker(githubRevokeEvery)
	defer tick.Stop()
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.ErrorContext(ctx, "broker: PANIC in the github token hand-back worker (contained; it continues next pass)",
						slog.Any("panic", r))
				}
			}()
			b.githubRevokePass(ctx)
		}()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-b.revokeKick:
		}
	}
}

// githubRevokePass prunes rows past expiry (pruneSealsSQL), then claims one page of due rows and presents each held token to GitHub, at most githubRevokeInFlight at
// once, within githubRevokePassBound. No claim or call starts once the pass budget or ctx is spent; a row claimed and
// not called comes back after its claim hold. A call in flight ends within refRevokeTimeout (Revoke detaches from
// cancel), and its outcome is written on a context neither cancels.
func (b *Broker) githubRevokePass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	pctx, cancel := context.WithTimeout(ctx, githubRevokePassBound)
	defer cancel()
	if err := b.tokenSeal.prune(pctx); err != nil {
		slog.WarnContext(ctx, "broker: github token hand-back could not prune expired tokens; it retries next pass", "err", err)
	}
	rows, now, err := b.tokenSeal.claimDue(pctx, githubRevokePage)
	if err != nil {
		slog.WarnContext(ctx, "broker: github token hand-back could not claim due tokens; it retries next pass", "err", err)
		return
	}
	slots := make(chan struct{}, githubRevokeInFlight)
	var wg sync.WaitGroup
	defer wg.Wait()
	warned := false
	for _, r := range rows {
		if !r.expiresAt.After(now) {
			b.endGitHubRevoke(ctx, r, sealExpired, r.attempts, "success", map[string]any{"target_outcome": targetExpired})
			continue
		}
		if b.github == nil {
			if !warned {
				slog.WarnContext(ctx, "broker: github token hand-back has no github app configured; queued tokens wait for their expiry")
				warned = true
			}
			continue
		}
		if pctx.Err() != nil {
			return
		}
		token, err := b.tokenSeal.openRow(pctx, r)
		if errors.Is(err, subjectkey.ErrDataLoss) || errors.Is(err, errSealUnopenable) {
			b.endGitHubRevoke(ctx, r, sealAbandoned, r.attempts, "failure", map[string]any{"target_outcome": targetUnsealable, "abandoned": true})
			continue
		}
		if err != nil {
			slog.WarnContext(ctx, "broker: github token hand-back could not read the run owner's key; it retries after the claim hold",
				"run_id", r.runID, "jti", r.jti, "err", err)
			continue
		}
		select {
		case slots <- struct{}{}:
		case <-pctx.Done():
			return
		}
		if pctx.Err() != nil {
			<-slots // a free slot and a spent budget can be ready together: start nothing
			return
		}
		wg.Add(1)
		go func(r dueRow, token string) {
			defer wg.Done()
			defer func() { <-slots }()
			defer func() {
				if p := recover(); p != nil {
					slog.ErrorContext(ctx, "broker: PANIC in a github token hand-back call (contained; the row comes back after its claim hold)",
						"run_id", r.runID, "jti", r.jti, slog.Any("panic", p))
				}
			}()
			b.recordGitHubRevoke(ctx, r, now, b.github.Revoke(pctx, token))
		}(r, token)
	}
}

// recordGitHubRevoke classifies one call's answer, ends or reschedules its row, and audits it. A 401 is a token
// already dead (success); a 404 is unknown, for example a wrong API base URL (failure, retried).
func (b *Broker) recordGitHubRevoke(ctx context.Context, r dueRow, claimedAt time.Time, err error) {
	outcome := targetRevoked
	if err != nil {
		outcome = targetFailed
		var ge *gh.ErrorResponse
		if errors.As(err, &ge) && ge.Response != nil {
			switch ge.Response.StatusCode {
			case http.StatusUnauthorized:
				outcome = targetAlreadyInvalid
			case http.StatusNotFound:
				outcome = targetUnknown
			}
		}
	}
	attempts := r.attempts + 1
	if outcome == targetRevoked || outcome == targetAlreadyInvalid {
		b.endGitHubRevoke(ctx, r, sealHandedBack, attempts, "success", map[string]any{"target_outcome": outcome})
		return
	}
	slog.WarnContext(ctx, "broker: github token hand-back failed", "run_id", r.runID, "jti", r.jti, "target_outcome", outcome, "err", err)
	at := claimedAt.Add(githubRevokeBackoff(attempts))
	if !at.Before(r.expiresAt) {
		b.endGitHubRevoke(ctx, r, sealAbandoned, attempts, "failure", map[string]any{"target_outcome": outcome, "abandoned": true})
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), githubRevokeWriteBound)
	defer cancel()
	if werr := b.tokenSeal.retryAt(wctx, r.runID, r.jti, attempts, at); werr != nil {
		slog.WarnContext(ctx, "broker: github token hand-back could not record its retry; the row comes back after its claim hold",
			"run_id", r.runID, "jti", r.jti, "err", werr)
	}
	b.auditGitHubRevoke(wctx, r.runID, r.jti, attempts, "failure", map[string]any{"target_outcome": outcome, "retry_at": at.UTC()})
}

// githubRevokeBackoff is the wait after a row's attempts-th failed call: githubRevokeFirstRetry doubled per earlier
// failure, at most githubRevokeMaxRetry.
func githubRevokeBackoff(attempts int) time.Duration {
	d := githubRevokeFirstRetry
	for i := 1; i < attempts && d < githubRevokeMaxRetry; i++ {
		d *= 2
	}
	return min(d, githubRevokeMaxRetry)
}

// endGitHubRevoke makes r a tombstone with ended and audits it as attempt with outcome and extra.
func (b *Broker) endGitHubRevoke(ctx context.Context, r dueRow, ended string, attempt int, outcome string, extra map[string]any) {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), githubRevokeWriteBound)
	defer cancel()
	if err := b.tokenSeal.end(wctx, r.runID, r.jti, ended); err != nil {
		slog.WarnContext(ctx, "broker: github token hand-back could not end its row; it comes back after its claim hold",
			"run_id", r.runID, "jti", r.jti, "err", err)
	}
	b.auditGitHubRevoke(wctx, r.runID, r.jti, attempt, outcome, extra)
}

// auditGitHubRevoke writes one github_token.revoke row. The token value is never in it.
func (b *Broker) auditGitHubRevoke(ctx context.Context, runID uuid.UUID, jti string, attempt int, outcome string, extra map[string]any) {
	d := map[string]any{"jti": jti, "kind": string(types.GrantGitHubToken), "attempt": attempt}
	for k, v := range extra {
		d[k] = v
	}
	data, _ := json.Marshal(d)
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		RunID:     &runID,
		ActorType: types.ActorSystem,
		Actor:     "wardyn-broker",
		Action:    "github_token.revoke",
		Target:    spiffeForRun(b.trustDomain, runID),
		Outcome:   outcome,
		Data:      data,
	}
	if err := b.audit.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
}
