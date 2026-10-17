// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The background half of mid-run AWS SSO renewal (awssso_refresh.go's header
// says why there are two halves). Each pass lists the live runs' AWS SSO
// grants, re-derives each one's credential exactly as the proxy's resolve does
// (awsSSOScopeAt), and renews those inside the skew window through
// refreshAWSSSOBlob: the same cross-replica owner lock, spent map and
// harness.credential.refresh row, plus `trigger: renewer`.
//
// The schedule exists for the audit log. Every read of a stored session writes
// a secret.read row, so a credential is read when its renewal falls due or its
// stored row changed (the row's revision, a metadata read), not on every pass.

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// awsSSORenewRecheck is how soon the schedule re-reads a credential it could
// not renew, from a secret store that keeps no row revision and so cannot say
// the row changed (a new sign-in, say).
const awsSSORenewRecheck = 10 * time.Minute

// awsSSORenewSchedule is when the renewer next reads each credential.
type awsSSORenewSchedule struct {
	mu   sync.Mutex
	next map[awsSSOScope]awsSSORenewCheck
}

// awsSSORenewCheck is the stored row's revision when it was last read, and when
// its renewal falls due; a zero at waits for the revision to change.
type awsSSORenewCheck struct {
	rev string
	at  time.Time
}

// due reports whether scope's credential must be read: never read, its row
// changed since, or its renewal due.
func (q *awsSSORenewSchedule) due(scope awsSSOScope, rev string, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	c, ok := q.next[scope]
	return !ok || c.rev != rev || (!c.at.IsZero() && !now.Before(c.at))
}

func (q *awsSSORenewSchedule) set(scope awsSSOScope, c awsSSORenewCheck) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.next == nil {
		q.next = map[awsSSOScope]awsSSORenewCheck{}
	}
	q.next[scope] = c
}

// keep forgets every credential no live run holds any more.
func (q *awsSSORenewSchedule) keep(live map[awsSSOScope]bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for sc := range q.next {
		if !live[sc] {
			delete(q.next, sc)
		}
	}
}

type renewerRenewalKey struct{}

// withRenewerRenewal marks ctx as the background renewer's. Like create's
// renewal it has no run to serve a redeemed pair from memory, so
// refreshAWSSSOBlob proves the store takes a write before it redeems, and the
// refresh row names the trigger.
func withRenewerRenewal(ctx context.Context) context.Context {
	return context.WithValue(ctx, renewerRenewalKey{}, true)
}

func renewerRenewal(ctx context.Context) bool {
	v, _ := ctx.Value(renewerRenewalKey{}).(bool)
	return v
}

// renewalWithNoRun reports whether ctx's renewal has no run to serve a
// redeemed pair from memory: create's and the background renewer's.
func renewalWithNoRun(ctx context.Context) bool {
	return createRenewal(ctx) || renewerRenewal(ctx)
}

// awsSSOTokenSpentUnderLock is refreshAWSSSOBlob's spent check under the owner
// lock. The renewer reads the stored mark afresh (awsSSOTokenSpentNow) and keeps
// a mark it finds: this process may have cached "not spent" before another
// replica, under this same lock, marked the token spent. Every other caller
// keeps the cached read.
func (s *Server) awsSSOTokenSpentUnderLock(ctx context.Context, fingerprint string) bool {
	if !renewerRenewal(ctx) {
		return s.awsSSOTokenSpent(fingerprint)
	}
	spent, err := s.awsSSOTokenSpentNow(ctx, fingerprint)
	if err != nil {
		return s.awsSSOTokenSpent(fingerprint)
	}
	if spent {
		s.ssoRefreshMu.Lock()
		if s.ssoRefreshSpent == nil {
			s.ssoRefreshSpent = map[string]bool{}
		}
		s.ssoRefreshSpent[fingerprint] = true
		s.ssoRefreshMu.Unlock()
	}
	return spent
}

// RenewAWSSSOCredentials renews, ahead of expiry, the stored AWS SSO session
// behind every live run's proxy-injected AWS SSO grant, and reports how many it
// renewed. Live is the store's (store.LiveAWSSSOGrantLister): an ended, revoked,
// paused or lost run, or an approval-gated grant that is not APPROVED, is never
// renewed for, and neither is a grant whose run, provider or pin drifted from
// its dispatch-time snapshot. One credential shared by several runs is renewed
// once. An error is a pass that could not list or scope its grants; a renewal
// that fails is logged and audited by refreshAWSSSOBlob and retried next pass.
func (s *Server) RenewAWSSSOCredentials(ctx context.Context) (int, error) {
	lister, ok := s.cfg.Store.(store.LiveAWSSSOGrantLister)
	if !ok || s.cfg.Secrets == nil {
		return 0, nil
	}
	grants, err := lister.ListLiveAWSSSOGrants(ctx, types.AWSSSOAccessTokenSecret)
	if err != nil {
		return 0, err
	}
	if len(grants) == 0 {
		s.ssoRenew.keep(nil)
		return 0, nil
	}
	siteCfg, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return 0, err // a scope is never derived from a read that failed
	}
	rctx := secretstore.WithPurpose(ctx, secretstore.PurposeSSORefresh)
	live := map[awsSSOScope]bool{}
	renewed := 0
	for _, g := range grants {
		if ctx.Err() != nil {
			return renewed, ctx.Err()
		}
		scope, snapshot, ok := s.liveGrantScope(rctx, siteCfg, g)
		if !ok || live[scope] {
			continue
		}
		live[scope] = true
		if s.renewAWSSSOScope(rctx, scope, snapshot) {
			renewed++
		}
	}
	s.ssoRenew.keep(live)
	return renewed, nil
}

// liveGrantScope is the credential g's run resolves through, re-derived for
// the subject its run token carries as the resolve re-derives it; false when
// the grant carries no snapshot, the run cannot be read or has ended, or
// anything drifted. Every grant of one scope carries the same pin, the live
// one, so the first grant's snapshot speaks for the rest.
func (s *Server) liveGrantScope(ctx context.Context, siteCfg types.SiteConfig, g types.CredentialGrant) (awsSSOScope, awsSSOScopeSnapshot, bool) {
	var sc struct {
		Snapshot awsSSOScopeSnapshot `json:"snapshot"`
	}
	if json.Unmarshal(g.Spec.Scope, &sc) != nil || !sc.Snapshot.authored() {
		return awsSSOScope{}, awsSSOScopeSnapshot{}, false
	}
	run, err := s.cfg.Store.GetRun(ctx, g.RunID)
	if err != nil || run.State.IsTerminal() {
		return awsSSOScope{}, awsSSOScopeSnapshot{}, false
	}
	subject := run.CreatedBy // the run token's subject (run_queue_start.go mints it)
	if s.cfg.LocalMode {
		subject = s.localSeat()
	}
	scope, drift := s.awsSSOScopeAt(ctx, siteCfg, run, sc.Snapshot, subject)
	return scope, sc.Snapshot, drift == ""
}

// renewAWSSSOScope renews scope's credential when its renewal is due, and
// reports whether the credential came back renewed. It renews only a token that
// is still valid, retrying a failed renewal each pass until it expires. One that
// has expired, that AWS has retired or refused, or that cannot be renewed waits
// for its row to change: the proxy's next resolve renews it or raises the
// sign-in, as before this renewer. A CLI session captured for another pin is
// left alone: the resolve refuses it.
func (s *Server) renewAWSSSOScope(ctx context.Context, scope awsSSOScope, snapshot awsSSOScopeSnapshot) bool {
	now, tune := s.cfg.Now(), s.awsSSOTuning()
	rev, guarded, err := s.awsSSORevision(ctx, scope)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not read an AWS SSO credential's revision; renewing it on a later pass", slog.Any("err", err))
		return false
	}
	if !s.ssoRenew.due(scope, rev, now) {
		return false
	}
	blob, found, err := s.readAWSSSOBlob(ctx, scope)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not read an AWS SSO credential to renew it; renewing it on a later pass", slog.Any("err", err))
		return false
	}
	eligible := func(b awsSSOBlob, at time.Time) bool {
		return found && (scope.cli == "" || b.bindsSnapshot(snapshot)) && !b.expired(at) && s.awsSSORenewable(b, at)
	}
	renewed := false
	if eligible(blob, now) && blob.needsRefresh(now, tune.Skew) {
		next, failure := s.refreshAWSSSOBlob(secretstore.WithPurpose(withRenewerRenewal(ctx), secretstore.PurposeSSORefresh), scope, blob)
		renewed = failure == "" && next.ExpiresAt.After(blob.ExpiresAt)
		blob = next
		if rev, guarded, err = s.awsSSORevision(ctx, scope); err != nil {
			rev = "" // unknown: read it again next pass
		}
	}
	c := awsSSORenewCheck{rev: rev}
	if eligible(blob, s.cfg.Now()) {
		c.at = blob.ExpiresAt.Add(-tune.Skew)
	}
	if !guarded && c.at.IsZero() {
		c.at = now.Add(awsSSORenewRecheck)
	}
	s.ssoRenew.set(scope, c)
	return renewed
}

// awsSSORenewable reports whether the renewer may redeem b's refresh token: it
// is renewable, and AWS has neither retired nor refused it.
func (s *Server) awsSSORenewable(b awsSSOBlob, now time.Time) bool {
	_, refused := s.awsSSORefusedFor(b)
	return b.renewable(now) && !refused && !s.awsSSOTokenSpentFor(b)
}
