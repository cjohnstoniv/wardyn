// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// TokenBindingEraser deletes a person's spire token bindings: the person erasure's credentials step calls
// it, never an identity provider.
type TokenBindingEraser interface {
	DeleteTokenBindingsOf(ctx context.Context, person string) (int64, error)
}

// PutTokenBinding writes b into identity_token_bindings. A second write of one jti fails on the primary
// key: a binding is never overwritten.
func (p PG) PutTokenBinding(ctx context.Context, b identity.TokenBinding) error {
	_, err := p.Pool.Exec(ctx, `INSERT INTO identity_token_bindings
	    (jti, run_id, spiffe_id, sub, sponsor, operator_owned, expires_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		b.JTI, b.RunID, b.SPIFFEID, b.Sub, b.Sponsor, b.OperatorOwned, b.Expiry)
	return err
}

// GetTokenBinding reads jti's binding, or identity.ErrBindingNotFound.
func (p PG) GetTokenBinding(ctx context.Context, jti string) (identity.TokenBinding, error) {
	b := identity.TokenBinding{JTI: jti}
	err := p.Pool.QueryRow(ctx, `SELECT run_id, spiffe_id, sub, sponsor, operator_owned, expires_at
	    FROM identity_token_bindings WHERE jti = $1`, jti).
		Scan(&b.RunID, &b.SPIFFEID, &b.Sub, &b.Sponsor, &b.OperatorOwned, &b.Expiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity.TokenBinding{}, identity.ErrBindingNotFound
	}
	if err != nil {
		return identity.TokenBinding{}, err
	}
	return b, nil
}

// PruneTokenBindings deletes at most identity.PruneBatch rows, of any run, whose expires_at is before
// cutoff.
func (p PG) PruneTokenBindings(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := p.Pool.Exec(ctx, `DELETE FROM identity_token_bindings WHERE jti IN
	    (SELECT jti FROM identity_token_bindings WHERE expires_at < $1 LIMIT $2)`, cutoff, identity.PruneBatch)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteTokenBindingsOf deletes the rows whose sub or sponsor is person, compared lower-cased. A sponsored
// token goes too: it must not outlive its sponsor's erasure.
func (p PG) DeleteTokenBindingsOf(ctx context.Context, person string) (int64, error) {
	tag, err := p.Pool.Exec(ctx, `DELETE FROM identity_token_bindings
	    WHERE lower(sub) = lower($1) OR lower(sponsor) = lower($1)`, person)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
