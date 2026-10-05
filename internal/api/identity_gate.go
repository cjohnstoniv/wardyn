// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// The owner guard and the sign-in gate adapter: how a deactivated identity is refused when a
// credential, a run or a captured secret is about to be written for it (leaver deprovisioning).

// authFailedIdentityDeactivated is the auth.fail reason of a sign-in refused over a deactivated
// identity (the callback's and a portal exchange's), and of a cookie, token or key refused for the
// same cause at request time.
const authFailedIdentityDeactivated = oidc.DenialIdentityDeactivated

type identityGateStore interface {
	IdentityRefused(ctx context.Context, issuer, tenantID, objectID, principal string) (bool, error)
	IssueLoginIdentity(ctx context.Context, in store.LoginIdentity, now time.Time) (int64, error)
}

type storeIdentityGate struct{ st identityGateStore }

// NewIdentityGate is oidc.IdentityGate over the store's identity rows.
func NewIdentityGate(st identityGateStore) oidc.IdentityGate { return storeIdentityGate{st: st} }

func (g storeIdentityGate) Refused(ctx context.Context, ref oidc.IdentityRef) (bool, error) {
	return g.st.IdentityRefused(ctx, ref.Issuer, ref.TenantID, ref.ObjectID, ref.Principal)
}

func (g storeIdentityGate) Issue(ctx context.Context, f oidc.LoginFacts) (int64, error) {
	epoch, err := g.st.IssueLoginIdentity(ctx, store.LoginIdentity{
		Principal: f.Sub, Issuer: f.Issuer, TenantID: f.TenantID, ObjectID: f.ObjectID, Email: f.Email,
	}, time.Now().UTC())
	switch {
	case errors.Is(err, store.ErrIdentityDeactivated):
		return 0, oidc.ErrIdentityDeactivated
	case errors.Is(err, store.ErrConflict):
		return 0, oidc.ErrIdentityConflict
	case errors.Is(err, store.ErrIdentityBindingMismatch):
		return 0, fmt.Errorf("%w: %w", oidc.ErrIdentityBindingMismatch, err)
	}
	return epoch, err
}

// ownerGuard is the guard for a write made for owner. The request's own authority epoch counts only
// when the request is the owner's own, or is the sign-in being issued for them (no session yet, the
// epoch from oidc.WithAuthorityEpoch): an admin writing for someone else carries an epoch that says
// nothing about them.
func ownerGuard(ctx context.Context, owner string) store.IdentityGuard {
	g := store.IdentityGuard{Principal: owner, Epoch: -1}
	if e, ok := oidc.AuthorityEpochFromContext(ctx); ok {
		if p := oidc.PrincipalFromContext(ctx); p == "" || p == owner {
			g.Epoch = e
		}
	}
	return g
}

// guardOwner makes the store's insert writers (run, API token, SSH key) refuse a deactivated or
// stale-epoch owner inside their own transaction (store.WithIdentityGuard).
func guardOwner(ctx context.Context, owner string) context.Context {
	return store.WithIdentityGuard(ctx, ownerGuard(ctx, owner))
}

type identitySharer interface {
	WithIdentityShared(ctx context.Context, g store.IdentityGuard, fn func() error) error
}

// putOwned runs put, a write of a credential into owner's secret namespace, while the owner's
// identity rows are held and unchanged, so a suspension takes them before it or the write is
// refused with store.ErrIdentityDeactivated. A store with no identity rows writes unguarded.
func (s *Server) putOwned(ctx context.Context, owner string, put func() error) error {
	sh, ok := s.cfg.Store.(identitySharer)
	if !ok || owner == "" {
		return put()
	}
	return sh.WithIdentityShared(ctx, ownerGuard(ctx, owner), put)
}
