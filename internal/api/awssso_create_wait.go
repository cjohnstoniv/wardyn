// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Create's wait for a renewal another flight already has in flight.
//
// Create only tries the owner lock (tryLockAWSSSOOwner): N launches must not each
// wait out one stalled renewal, and a blocking wait would hold lock-pool slots.
// But a create that served the token in hand while another flight was renewing
// could insert its run row just before that flight ended invalid_grant, and
// dispatch would then refuse a run that already exists. So create watches the
// STORE, never the lock (every lock try opens its own connection through the
// small lock pool), for what that flight leaves behind.
//
// The wait applies only to a secret store that keeps row revisions
// (secretstore.Revisioned): the revision is what tells a new pair from the old
// one without reading it. A store without them has nothing to watch but the pair
// itself, and every read of it decrypts it and writes a secret.read row, so
// there create serves the token in hand at once, as it did before this wait.
//
// What this closes, stated narrowly: the window where the other flight's token
// exchange ends inside the budget. It is a best-effort wait chosen for
// availability. A holder still persisting after the budget (the persist has its
// own 30 s, awsSSOPersistBudget) can still mark the pair spent after the run
// exists; that needs a failed persist, so it is rare.
// A holder that releases its lock without changing the row is invisible to
// this metadata-only watch; an unchanged row still waits out the budget.

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// awsSSOCreateWaitBudget is how long create watches: the holder's whole token
// exchange (two attempts and the delay between them) and a second, so it follows
// awsSSORefreshTimeout if that is ever raised. A var for the tests.
var awsSSOCreateWaitBudget = 2*awsSSORefreshTimeout + awsSSORefreshRetryDelay + time.Second

// awsSSOCreateWaitTick is the pause between two polls, and each poll's deadline.
var awsSSOCreateWaitTick = 250 * time.Millisecond

type awsSSORenewalBaselineKey struct{ scope awsSSOScope }

type awsSSORenewalBaseline struct {
	rev     string
	guarded bool
	err     error
}

// The baseline must precede the door's value read: a rotation between that
// read and the first poll must not become the revision we wait for changes to.
// Keep it request-local and scoped to the exact credential the door reads.
func (s *Server) readAWSSORenewalBaseline(ctx context.Context, scope awsSSOScope) awsSSORenewalBaseline {
	rctx, cancel := context.WithTimeout(ctx, awsSSOCreateWaitTick)
	defer cancel()
	rev, guarded, err := s.awsSSORevision(rctx, scope)
	return awsSSORenewalBaseline{rev, guarded, err}
}

// renewalInFlight is refreshAWSSSOBlob's answer when another flight holds the
// owner lock and the token in hand can still carry a run. Create has no later
// chance to refuse, so it waits for that flight's result rather than make a run
// on a pair the flight may be about to spend; any other caller serves the token.
func (s *Server) renewalInFlight(ctx context.Context, scope awsSSOScope, blob awsSSOBlob) (awsSSOBlob, string) {
	if createRenewal(ctx) {
		return s.awaitAWSSSORenewalInFlight(ctx, scope, blob)
	}
	slog.InfoContext(ctx, "wardynd: a renewal of this AWS SSO credential is already in flight; serving the still-valid token",
		slog.String("credential_source", awsSSOCredentialSourceLabel(scope)))
	return blob, ""
}

// awaitAWSSSORenewalInFlight is a create-time renewal's answer when another
// flight holds the owner lock. Each poll reads metadata only, under a deadline
// derived from the request's context: the spent mark, and the stored row's
// revision. Spent, or the row gone: the spent sentence, so no run row is made.
// The revision changed: the pair is read once and, when it can carry a run,
// served. A read that fails or does not answer inside its tick, a client that
// went away, or a budget spent with nothing changed: the token in hand, as
// before this wait, never the unavailable sentence. A store that keeps no row
// revision ends the wait at its first poll, on the token in hand.
func (s *Server) awaitAWSSSORenewalInFlight(ctx context.Context, scope awsSSOScope, blob awsSSOBlob) (awsSSOBlob, string) {
	slog.InfoContext(ctx, "wardynd: a renewal of this AWS SSO credential is already in flight; waiting for its result",
		slog.String("credential_source", awsSSOCredentialSourceLabel(scope)))
	baseline, _ := ctx.Value(awsSSORenewalBaselineKey{scope}).(awsSSORenewalBaseline)
	w := awsSSORenewalWatch{s: s, scope: scope, blob: blob, fingerprint: awsSSOTokenFingerprint(blob.RefreshToken), baseline: baseline}
	stop := time.Now().Add(awsSSOCreateWaitBudget)
	tick := time.NewTicker(awsSSOCreateWaitTick)
	defer tick.Stop()
	for {
		pctx, cancel := context.WithTimeout(ctx, awsSSOCreateWaitTick)
		next, failure, done, err := w.poll(pctx)
		cancel()
		if err != nil {
			slog.WarnContext(ctx, "wardynd: stopped waiting for the AWS SSO renewal in flight; serving the still-valid token", slog.Any("err", err))
			return blob, ""
		}
		if done {
			return next, failure
		}
		select {
		case <-ctx.Done():
			return blob, ""
		case <-tick.C:
		}
		if time.Now().After(stop) {
			return blob, ""
		}
	}
}

// awsSSORenewalWatch is one create's view of the store while it waits.
type awsSSORenewalWatch struct {
	s           *Server
	scope       awsSSOScope
	blob        awsSSOBlob
	fingerprint string
	baseline    awsSSORenewalBaseline
}

func (w *awsSSORenewalWatch) poll(ctx context.Context) (awsSSOBlob, string, bool, error) {
	spent, err := w.s.awsSSOTokenSpentNow(ctx, w.fingerprint)
	if err != nil {
		return w.blob, "", false, err
	}
	if spent {
		return w.blob, awsSSORefreshSpentSentence, true, nil
	}
	if w.baseline.err != nil || !w.baseline.guarded {
		return w.blob, "", true, w.baseline.err
	}
	rev, guarded, err := w.s.awsSSORevision(ctx, w.scope)
	if err != nil {
		return w.blob, "", false, err
	}
	switch {
	case !guarded:
		// Nothing to watch but the pair itself, and every read of it decrypts it
		// and writes a secret.read row: the token in hand, as before this wait.
		slog.InfoContext(ctx, "wardynd: this secret store keeps no row revision to watch; serving the still-valid token")
		return w.blob, "", true, nil
	case rev == "":
		return w.blob, awsSSORefreshSpentSentence, true, nil
	case rev == w.baseline.rev:
		return w.blob, "", false, nil
	}
	cur, found, err := w.s.readAWSSSOBlob(ctx, w.scope)
	switch {
	case err != nil:
		return w.blob, "", false, err
	case !found:
		return w.blob, awsSSORefreshSpentSentence, true, nil
	case cur.servableFor(w.s.cfg.Now(), awsSSORefreshServeFloor):
		cur.maskGeneration = w.blob.maskGeneration
		return cur, "", true, nil
	}
	return w.blob, "", true, nil
}

// awsSSOTokenSpentNow is awsSSOTokenSpent for a caller that must see a mark
// another replica writes later: it reads under ctx and caches nothing. A mark
// this process holds is final, so it is answered from the map.
func (s *Server) awsSSOTokenSpentNow(ctx context.Context, fingerprint string) (bool, error) {
	s.ssoRefreshMu.Lock()
	marked := s.ssoRefreshSpent[fingerprint]
	s.ssoRefreshMu.Unlock()
	if marked {
		return true, nil
	}
	st, ok := s.cfg.Store.(store.AWSSSOSpentTokenStore)
	if !ok {
		return false, nil
	}
	return st.AWSSSOTokenSpent(ctx, fingerprint)
}
