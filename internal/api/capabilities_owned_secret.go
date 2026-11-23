// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
)

// The owned-secret seam: one request's memo of which secrets a principal owns. Split out of
// capabilities.go for scripts/check-file-size.sh.

// the owned-secret seam

// ownedSecretMemoKey carries one request's owned-secret memo. A pointer holder
// rather than the value, so a memo installed once at the top of a resolution is
// shared by every site further down without re-threading a parameter through
// three signatures two other packages already call.
type ownedSecretMemoKey struct{}

// ownedSecretMemo is one request's answer to "which secrets does this principal
// own", resolved at most once PER OWNER.
//
// It exists for the same reason capBatch does, on the list capBatch did not
// cover. capBatch made the egress loop flat in len(allowed_domains) and
// maxAllowedDomainsPerSpec capped that list — but the member pipeline's OTHER
// caller-sized list, spec.eligible_grants, has no count cap at all and bought an
// unmemoized For(owner).List per grant at THREE sites in one request:
// filterUserGrants' 6c own-key arm, narrowUserInlinePolicy's ownership
// exemption (twice per grant, secret_ref and known_hosts_ref) and
// validateInlineSecretRefs' unknown-name arm. Measured on this tree with a
// counting store double: 3N+1 owner-scoped reads for N grants, N chosen entirely
// by the request body under maxJSONBody, on POST /runs/preflight — a route that
// persists nothing and is repeatable for free. After: one read per owner per
// request, flat in N.
//
// Not a cache, on the same terms capBatch states: it lives for ONE resolution
// and is discarded with the request, so a secret created or deleted between
// requests is seen by the next one.
//
// A store ERROR is memoized as itself, never as an empty set: every later ask
// for that owner within the request answers the same fault with no second read,
// and a caller that takes the error (ownsSecretChecked) refuses with the fixed
// retryable 503 rather than "unknown secret". Re-reading per value on the error
// path would leave exactly the amplification this closes, on the path a
// struggling store is least able to absorb.
type ownedSecretMemo struct {
	byOwner map[string]ownedSecretAnswer
}

// ownedSecretAnswer is one owner's answer: the names they hold, or the fault that
// kept the store from answering.
type ownedSecretAnswer struct {
	names map[string]bool
	err   error
}

// withOwnedSecretMemo installs the memo for a resolution. Idempotent: a nested
// call keeps the outer memo, so the whole request shares one answer.
func withOwnedSecretMemo(ctx context.Context) context.Context {
	if _, ok := ctx.Value(ownedSecretMemoKey{}).(*ownedSecretMemo); ok {
		return ctx
	}
	return context.WithValue(ctx, ownedSecretMemoKey{}, &ownedSecretMemo{byOwner: map[string]ownedSecretAnswer{}})
}

// ownsSecretChecked is ownsSecret through the request's memo; its error is
// errSecretStoreUnavailable. Without a memo in ctx it IS ownsSecret, byte for
// byte, so a door with no memo reads the store once per call. It is ownsSecret's
// only caller.
//
// Single-goroutine by construction: the member pipeline is one sequential
// resolution inside one handler, and the memo is installed per resolution rather
// than per process, so there is no second writer to guard against. If a caller
// ever fans out over one memo, this needs a mutex — say so here rather than
// discovering it as a race.
func (s *Server) ownsSecretChecked(ctx context.Context, owner, name string) (bool, error) {
	if _, ok := ctx.Value(ownedSecretMemoKey{}).(*ownedSecretMemo); !ok || owner == "" || name == "" || s.cfg.Secrets == nil {
		return s.ownsSecret(ctx, owner, name)
	}
	read := s.ownedSecretRead(ctx, owner)
	return read.names[name], read.err
}

// ownedSecretNames is every name owner holds in their own namespace (none on a
// store fault, which ownedSecretRead logs), through the request's memo when one
// is installed. The set is the memo's own: callers read it, never write it.
func (s *Server) ownedSecretNames(ctx context.Context, owner string) map[string]bool {
	return s.ownedSecretRead(ctx, owner).names
}

// ownedSecretRead reads owner's own names once per owner per request when a memo
// is installed, and fresh otherwise.
func (s *Server) ownedSecretRead(ctx context.Context, owner string) ownedSecretAnswer {
	memo, _ := ctx.Value(ownedSecretMemoKey{}).(*ownedSecretMemo)
	if memo != nil {
		if read, loaded := memo.byOwner[owner]; loaded {
			return read
		}
	}
	read := ownedSecretAnswer{names: map[string]bool{}}
	// Names only, own rows only — the same read ownsSecret makes, and the
	// same reason: an ownership claim must be provable, never merely
	// unrefuted.
	names, err := s.cfg.Secrets.For(owner).List(ctx)
	if err != nil {
		// Not silently: a store fault otherwise reads as "you hold no such
		// secret". The driver text stays in the log.
		slog.WarnContext(ctx, "api: list a person's own secrets for an ownership check", slog.Any("err", err))
		read.err = errSecretStoreUnavailable
	}
	for _, n := range names {
		read.names[n] = true
	}
	if memo != nil {
		memo.byOwner[owner] = read
	}
	return read
}
