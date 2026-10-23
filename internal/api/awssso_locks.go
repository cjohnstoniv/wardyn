// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// lockAWSSSOOwner takes one person's AWS SSO lock, across replicas, and
// returns the context the guarded work must use (cancelled if the lock is lost)
// and its release. Owner is the secret-store namespace: "" is the operator-wide
// one (a `shared` credential), and under a per_user row it is the principal's
// own. A lock that cannot be taken is an error, never a pass-through.
//
// Taken by the login-capture upload (handleUploadSSOToken), around that
// credential's own lock, and by the whole-person erase
// (lockAWSSSOEveryCredential), so no new credential is captured while an
// erase is choosing the credential locks it takes. A renewal does not take it:
// it takes only its own credential's lock (#2137).
func (s *Server) lockAWSSSOOwner(ctx context.Context, owner string) (context.Context, func(), error) {
	return s.lock(ctx, db.AWSSSOLockClass, owner)
}

// lockAWSSSOCredential takes the renewal single-flight of ONE stored AWS SSO
// credential (its namespace and secret name). Every read-modify-write of that
// credential holds it: the renewal, the capture upload and the whole-person
// erase. Two credentials of one person (two bedrock_sso providers, a provider
// and the AWS CLI) never wait on each other; two renewals of the same one
// always do, since a renewal without it can spend one rotating refresh token
// twice.
func (s *Server) lockAWSSSOCredential(ctx context.Context, scope awsSSOScope) (context.Context, func(), error) {
	return s.lock(ctx, db.AWSSSOCredentialLockClass, scope.owner, scope.ssoSecret())
}

// lockAWSSSOCapture is the capture upload's hold: the person's lock, then the
// credential's.
func (s *Server) lockAWSSSOCapture(ctx context.Context, scope awsSSOScope) (context.Context, func(), error) {
	ctx, unlockOwner, err := s.lockAWSSSOOwner(ctx, scope.owner)
	if err != nil {
		return ctx, nil, err
	}
	lctx, unlock, err := s.lockAWSSSOCredential(ctx, scope)
	if err != nil {
		unlockOwner()
		return ctx, nil, err
	}
	return lctx, func() { unlock(); unlockOwner() }, nil
}

// tryLockAWSSSOCredential is lockAWSSSOCredential's NON-BLOCKING form: ok=false rather
// than queueing behind a renewal that is already in flight. A lock that could
// not be tried at all (no lock connection to spare, the database not answering)
// is err, never ok=false: it says nothing about a renewal in flight.
//
// Why that is a different question from "who renews". Dispatch is
// synchronous with POST /runs, and the renewal starts a whole skew window
// (awsSSORefreshSkew, 10 min) AHEAD of expiry — so the common case is N people
// launching runs against a token that still works perfectly well. When the
// SSO-OIDC endpoint is unresponsive the in-flight renewal costs
// 2 x awsSSORefreshTimeout + the retry delay before it gives up and serves the
// token it holds; every OTHER create queued behind this lock used to pay that
// same bill again, serially, inside its own create request. N creates, N times
// the stall, for a credential none of them needed renewed.
//
// So: a caller whose token is still valid takes the lock only if it is free.
// If someone else is already renewing, it serves the token in hand and lets
// that flight finish — the next dispatch reads whatever the renewal persisted.
// A caller whose token is EXPIRED still BLOCKS, because for that caller the
// renewal is the difference between a working run and a dead one, and the
// in-flight renewal is very likely about to produce exactly what it needs.
func (s *Server) tryLockAWSSSOCredential(ctx context.Context, scope awsSSOScope) (context.Context, func(), bool, error) {
	return s.locker().TryLock(ctx, db.NewLockKey(db.AWSSSOCredentialLockClass, scope.owner, scope.ssoSecret()))
}

// lockAWSSSOEveryCredential is the whole-person erase's AWS SSO hold: owner's
// person lock, so no capture lands meanwhile, then the lock of every AWS SSO
// credential stored in owner's own namespace, in ascending key order (the one
// order db.LockOrder admits for several of them), so every renewal in flight
// finishes first and none starts until the release. Over-matching a name only
// takes a lock nobody else wants.
func (s *Server) lockAWSSSOEveryCredential(ctx context.Context, owner string) (context.Context, func(), error) {
	ctx, unlockOwner, err := s.lockAWSSSOOwner(ctx, owner)
	if err != nil {
		return ctx, nil, err
	}
	var keys []db.LockKey
	if s.cfg.Secrets != nil {
		names, err := s.cfg.Secrets.For(owner).List(ctx)
		if err != nil {
			unlockOwner()
			return ctx, nil, fmt.Errorf("list the stored AWS SSO credentials: %w", err)
		}
		for _, name := range names {
			if strings.HasSuffix(name, "-"+providerSSOPart) || name == harnessCredSecretName(awsSSOProvider) {
				keys = append(keys, db.NewLockKey(db.AWSSSOCredentialLockClass, owner, name))
			}
		}
	}
	slices.SortFunc(keys, func(a, b db.LockKey) int { return cmp.Compare(a.Obj, b.Obj) })
	unlocks := []func(){unlockOwner}
	unlockAll := func() {
		for _, u := range slices.Backward(unlocks) {
			u()
		}
	}
	for _, k := range keys {
		lctx, unlock, err := s.locker().Lock(ctx, k, db.LockWait)
		if err != nil {
			unlockAll()
			return ctx, nil, err
		}
		ctx, unlocks = lctx, append(unlocks, unlock)
	}
	return ctx, unlockAll, nil
}

// lockAWSSSORenewal takes the credential's renewal lock for refreshAWSSSOBlob. mayServe
// says the token in hand can still carry a run, so the caller only tries
// (tryLockAWSSSOCredential): got=false with no error then means a renewal is already
// in flight. Any other caller waits (lockAWSSSOCredential). err means the lock could
// not be tried or taken at all (renewalLockUntaken). The returned context is the
// guarded work's.
func (s *Server) lockAWSSSORenewal(ctx context.Context, scope awsSSOScope, mayServe bool) (lctx context.Context, unlock func(), got bool, err error) {
	if mayServe {
		return s.tryLockAWSSSOCredential(ctx, scope)
	}
	lctx, unlock, err = s.lockAWSSSOCredential(ctx, scope)
	return lctx, unlock, err == nil, err
}
