// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The sign-in gate and the owner guard: how a suspended identity is refused at issuance and how
// a credential minted during a suspension is refused at its insert (migration 0113, 0118).
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrIdentityDeactivated is the refusal of anything that would give a deactivated, purged or
// stale-epoch identity a new session, credential or run.
var ErrIdentityDeactivated = errors.New("store: identity deactivated")

// IdentityGuard names the owner a write acts for and the authority epoch its caller was admitted
// under. Epoch < 0 means the caller carries no epoch (an API token, a request-free caller): only a
// deactivation refuses.
type IdentityGuard struct {
	Principal string
	Epoch     int64
}

type identityGuardKey struct{}

// WithIdentityGuard makes the insert writers that honour it (CreateRun, CreateRunUnderCap,
// CreateAPIToken, AddSSHKey) read the owner's identity rows FOR SHARE in their own transaction.
// Suspension takes the same rows FOR UPDATE, so a write either commits before the suspension's
// first step, where the sweep after it finds it, or reads the deactivation and is refused.
func WithIdentityGuard(ctx context.Context, g IdentityGuard) context.Context {
	return context.WithValue(ctx, identityGuardKey{}, g)
}

func identityGuardFrom(ctx context.Context) (IdentityGuard, bool) {
	g, ok := ctx.Value(identityGuardKey{}).(IdentityGuard)
	return g, ok && g.Principal != ""
}

// checkIdentityGuard locks every identity row bound to g.Principal FOR SHARE and refuses when one
// is deactivated, purged, or past g.Epoch. A principal with no row passes: only a sign-in or a
// SCIM suspension writes one.
func checkIdentityGuard(ctx context.Context, tx pgx.Tx, g IdentityGuard) error {
	rows, err := tx.Query(ctx, `
		SELECT deactivated_at IS NOT NULL OR purged_at IS NOT NULL, authority_epoch
		  FROM principal_identities WHERE principal = $1 FOR SHARE`, g.Principal)
	if err != nil {
		return fmt.Errorf("store: identity guard: %w", err)
	}
	defer rows.Close()
	refused := false
	for rows.Next() {
		var dead bool
		var epoch int64
		if err := rows.Scan(&dead, &epoch); err != nil {
			return fmt.Errorf("store: identity guard: %w", err)
		}
		refused = refused || dead || (g.Epoch >= 0 && epoch > g.Epoch)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: identity guard: %w", err)
	}
	if refused {
		return ErrIdentityDeactivated
	}
	return nil
}

type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// guarded runs fn on the pool, or, when ctx carries an IdentityGuard, on a transaction that holds
// the guarded owner's identity rows FOR SHARE until it commits.
func (s PG) guarded(ctx context.Context, fn func(q queryRower) error) error {
	g, ok := identityGuardFrom(ctx)
	if !ok {
		return fn(s.Pool)
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store: begin guarded write: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := checkIdentityGuard(ctx, tx, g); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WithIdentityShared runs fn while g's identity rows are held FOR SHARE and unchanged: the write a
// sign-in's captured credential makes lands before a suspension takes the rows, or is refused
// with ErrIdentityDeactivated. fn may write through another connection (the secret store).
func (s PG) WithIdentityShared(ctx context.Context, g IdentityGuard, fn func() error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store: begin guarded write: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := checkIdentityGuard(ctx, tx, g); err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// IssueLoginIdentity is the issuance half of the sign-in gate: in one transaction it upserts and
// binds the identity row (a failure refuses the sign-in), re-checks deactivation on every row
// bound to the principal and reads the authority epoch the session cookie will carry. The rows
// stay locked until the commit, so a suspension either precedes it, and the sign-in is refused
// with ErrIdentityDeactivated, or follows it, and bumps the epoch past the one returned. A
// refused sign-in writes nothing.
func (s PG) IssueLoginIdentity(ctx context.Context, in LoginIdentity, now time.Time) (epoch int64, err error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("store: issue login identity: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	row, err := upsertLoginIdentityTx(ctx, tx, in, now)
	if err != nil {
		return 0, err
	}
	if row.DeactivatedAt != nil || row.PurgedAt != nil {
		return 0, ErrIdentityDeactivated
	}
	if err = checkIdentityGuard(ctx, tx, IdentityGuard{Principal: in.Principal, Epoch: -1}); err != nil {
		return 0, err
	}
	epoch = row.AuthorityEpoch
	var top int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(authority_epoch), 0) FROM principal_identities WHERE principal = $1`,
		in.Principal).Scan(&top); err != nil {
		return 0, fmt.Errorf("store: read authority epoch: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: commit login identity: %w", err)
	}
	return max(epoch, top), nil
}

// IdentityRefused is the admission half of the sign-in gate, read-only: whether the identity a
// sign-in names is deactivated or purged. An Entra sign-in is named by (issuer, tenant, object
// id), and every sign-in by its resolved principal; either match refuses.
func (s PG) IdentityRefused(ctx context.Context, issuer, tenantID, objectID, principal string) (bool, error) {
	var refused bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM principal_identities
			 WHERE (deactivated_at IS NOT NULL OR purged_at IS NOT NULL)
			   AND ((object_id <> '' AND issuer = $1 AND tenant_id = $2 AND object_id = $3)
			        OR ($4 <> '' AND principal = $4)))`, issuer, tenantID, objectID, principal).Scan(&refused)
	if err != nil {
		return false, fmt.Errorf("store: identity refused: %w", err)
	}
	return refused, nil
}

// IdentityBlocked reports whether principal's identity rows refuse a credential admitted under
// epoch (epoch < 0: only a deactivation refuses). It is the read the cookie, API-token and SSH-key
// lanes make at authentication, as one statement.
func (s PG) IdentityBlocked(ctx context.Context, principal string, epoch int64) (bool, error) {
	var blocked bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM principal_identities
			 WHERE principal = $1
			   AND (deactivated_at IS NOT NULL OR purged_at IS NOT NULL OR ($2 >= 0 AND authority_epoch > $2)))`,
		principal, epoch).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("store: identity blocked: %w", err)
	}
	return blocked, nil
}
