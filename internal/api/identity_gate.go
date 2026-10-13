// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
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

// beginCredentialWrite is the first step of every write of a person's stored credential: it fences
// the writes made under the returned context to the owner's erase generation
// (secretstore.BeginWrite), so an erase that lands after this call refuses them. Call it before
// reading anything the write depends on; putOwned begins one itself for a write that did not. A store
// with no generation, or the operator namespace, is returned unfenced.
func (s *Server) beginCredentialWrite(ctx context.Context, owner string) (context.Context, error) {
	if owner == "" || s.cfg.Secrets == nil {
		return ctx, nil
	}
	if _, fenced := secretstore.IfGenerationFrom(ctx, owner); fenced {
		return ctx, nil
	}
	ctx, _, err := secretstore.BeginWrite(ctx, owner, s.cfg.Secrets.For(owner))
	return ctx, err
}

// beginRequestWrite is beginCredentialWrite for a handler: it answers the request itself when the
// generation cannot be read, and returns the request carrying the fenced context.
func (s *Server) beginRequestWrite(w http.ResponseWriter, r *http.Request, owner string) (*http.Request, bool) {
	ctx, err := s.beginCredentialWrite(r.Context(), owner)
	if err != nil {
		writeServerError(w, r, "begin credential write", err)
		return r, false
	}
	return r.WithContext(ctx), true
}

// putOwned runs put, a write of a credential into owner's secret namespace, while the owner's
// identity rows are held and unchanged, so a suspension takes them before it or the write is
// refused with store.ErrIdentityDeactivated. A store with no identity rows writes unguarded.
//
// It is the one path every such write takes: put receives a context fenced to the owner's erase
// generation, so a write that began before an erase is refused with secretstore.ErrGenerationChanged
// (TestCredentialWritersRouteThroughPutOwned pins that no writer bypasses it).
//
// The begin here is the fence for a read-free capture (a sign-in's first store, a provider capture):
// it reads nothing before it writes, so this is the only begin it has. A writer that reads the
// credential and writes a changed copy back must begin BEFORE its read (beginCredentialWrite); a
// begin made here, after the read, reads the advanced generation and lets the stale copy land
// (TestCredentialWriteBeginsBeforeItsRead_* pin each such writer).
func (s *Server) putOwned(ctx context.Context, owner string, put func(context.Context) error) error {
	ctx, err := s.beginCredentialWrite(ctx, owner)
	if err != nil {
		return err
	}
	sh, ok := s.cfg.Store.(identitySharer)
	if !ok || owner == "" {
		return put(ctx)
	}
	return sh.WithIdentityShared(ctx, ownerGuard(ctx, owner), func() error { return put(ctx) })
}
