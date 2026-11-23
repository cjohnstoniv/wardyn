// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var (
	ErrInboundFederationOff         = errors.New("store: inbound federation is off")
	ErrInboundTrustExists           = errors.New("store: an inbound trust with that name, or with that issuer, audience and subject, exists")
	ErrInboundTrustAudienceReserved = errors.New("store: the audience is a live portal's client id or audience")
	ErrInboundTrustCap              = errors.New("store: the inbound trust holds the most live exchanged tokens")
	ErrInboundTrustInvalid          = errors.New("store: an inbound trust field is out of its bounds")
)

// InboundTrustStore holds inbound federation: the switch, the trusts, and the tokens exchanged under them.
// Lock order, for every writer here and for a sponsor change, a retire and a person's erasure: a pending turn-on
// change's row, then the switch row, then service identity rows, then trust rows in id order, then token rows.
// Each writer takes that order, skipping the locks it does not need: the exchange the switch (shared), the identity,
// its trust (shared), then tokens; a switch-off the pending turn-on, the switch, every trust in id order, then
// tokens; a decision on a held turn-on the change row (the decision locks it first), then the switch; a sponsor
// change and a retire the identity, its trusts in id order, then tokens; a confirm the identity, then its trust; a
// delete the identity when the sponsor asks, the trust, then tokens; an erasure the switch when it names the person,
// identities, then trusts in id order. Every token row a writer revokes is linked to a trust it already holds, so
// two writers never meet first on token rows.
type InboundTrustStore interface {
	GetInboundFederation(ctx context.Context) (types.InboundFederation, error)
	SetInboundFederation(ctx context.Context, enabled bool, by string) (InboundFederationSet, error)
	CreateInboundTrust(ctx context.Context, t types.InboundTrust) (types.InboundTrust, string, error)
	ConfirmInboundTrust(ctx context.Context, c InboundTrustConfirm) (types.InboundTrust, error)
	ListInboundTrusts(ctx context.Context, service string) ([]types.InboundTrust, error)
	GetInboundTrust(ctx context.Context, id uuid.UUID) (types.InboundTrust, error)
	DeleteInboundTrust(ctx context.Context, d InboundTrustDelete) (types.InboundTrust, int, error)
	MatchInboundTrust(ctx context.Context, issuer, audience, subject string) (types.InboundTrust, error)
	CreateExchangedToken(ctx context.Context, trust types.InboundTrust, t types.APIToken, raw string) (types.APIToken, error)
}

var _ InboundTrustStore = PG{}

// InboundFederationSet is what one switch write did: the new state, the exchanged tokens it revoked, and the pending
// turn-on changes it cancelled (a switch-off only).
type InboundFederationSet struct {
	State            types.InboundFederation
	TokensRevoked    int
	ChangesCancelled []uuid.UUID
}

// InboundTrustDelete names the trust to delete. Service and Sponsor, when set, limit it to a trust on that
// identity while that person sponsors it, checked under the identity's row lock; anything else is ErrNotFound.
type InboundTrustDelete struct {
	ID               uuid.UUID
	Service, Sponsor string
}

// InboundTrustConfirm is the sponsor's confirm of one trust: At and Seq are the issue the API read before its late
// check of the sponsor's session.
type InboundTrustConfirm struct {
	ID               uuid.UUID
	Service, Sponsor string
	At               time.Time
	Seq              int64
}

const inboundTrustCols = `id, name, issuer, internal_issuer, audience, subject, required_claim, required_claim_value,
	service_name, token_ttl_seconds, max_token_age_seconds, max_live, created_at, created_by, confirmed_at,
	confirmed_seq, confirmed_by`

func scanInboundTrust(row pgx.Row) (types.InboundTrust, error) {
	var t types.InboundTrust
	err := row.Scan(&t.ID, &t.Name, &t.Issuer, &t.InternalIssuer, &t.Audience, &t.Subject, &t.RequiredClaim,
		&t.RequiredClaimValue, &t.ServiceIdentity, &t.TokenTTLSeconds, &t.MaxTokenAgeSeconds, &t.MaxLive, &t.CreatedAt,
		&t.CreatedBy, &t.ConfirmedAt, &t.ConfirmedSeq, &t.ConfirmedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.InboundTrust{}, ErrNotFound
	}
	if err != nil {
		return types.InboundTrust{}, fmt.Errorf("store: scan inbound trust: %w", err)
	}
	return t, nil
}

func (s PG) GetInboundFederation(ctx context.Context) (types.InboundFederation, error) {
	var f types.InboundFederation
	err := s.Pool.QueryRow(ctx, `SELECT enabled, updated_at, updated_by FROM inbound_federation WHERE id`).
		Scan(&f.Enabled, &f.UpdatedAt, &f.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.InboundFederation{}, nil
	}
	if err != nil {
		return types.InboundFederation{}, fmt.Errorf("store: read inbound federation: %w", err)
	}
	return f, nil
}

func (s PG) SetInboundFederation(ctx context.Context, enabled bool, by string) (InboundFederationSet, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return InboundFederationSet{}, fmt.Errorf("store: set inbound federation: %w", err)
	}
	defer db.Rollback(ctx, tx)
	out, err := SetInboundFederationQ(ctx, tx, enabled, by)
	if err != nil {
		return InboundFederationSet{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return InboundFederationSet{}, fmt.Errorf("store: commit inbound federation: %w", err)
	}
	return out, nil
}

// SetInboundFederationQ is SetInboundFederation on q.
func SetInboundFederationQ(ctx context.Context, q Querier, enabled bool, by string) (InboundFederationSet, error) {
	var out InboundFederationSet
	if !enabled {
		// A pending turn-on is cancelled before the switch row is locked: a decision locks its change row first and
		// the switch row in its apply, so both writers take the change row, then the switch.
		ids, err := collect(ctx, q, "cancel", "pending inbound federation turn-ons",
			`UPDATE governance_changes SET state = 'stale', decided_at = now(), decided_by = $1, reason = 'Inbound federation was turned off' WHERE target_kind = $2 AND target_key = $3 AND op = 'update' AND state = 'pending' RETURNING id`,
			[]any{by, types.GovernanceTargetInboundFederation, types.InboundFederationSwitchKey},
			func(row pgx.Row) (uuid.UUID, error) {
				var id uuid.UUID
				err := row.Scan(&id)
				return id, err
			})
		if err != nil {
			return InboundFederationSet{}, err
		}
		out.ChangesCancelled = ids
	}
	var was bool
	err := q.QueryRow(ctx, `SELECT enabled FROM inbound_federation WHERE id FOR UPDATE`).Scan(&was)
	if errors.Is(err, pgx.ErrNoRows) {
		return InboundFederationSet{}, errors.New("store: the inbound federation row is missing")
	}
	if err != nil {
		return InboundFederationSet{}, fmt.Errorf("store: lock inbound federation: %w", err)
	}
	if err = q.QueryRow(ctx, `UPDATE inbound_federation SET enabled = $1, updated_at = clock_timestamp(), updated_by = $2 WHERE id RETURNING enabled, updated_at, updated_by`,
		enabled, by).Scan(&out.State.Enabled, &out.State.UpdatedAt, &out.State.UpdatedBy); err != nil {
		return InboundFederationSet{}, fmt.Errorf("store: set inbound federation: %w", err)
	}
	if enabled {
		return out, nil
	}
	// Every trust is held, in id order, before a token is touched: each other writer that revokes a linked token
	// holds that token's trust first, so this revoke never meets one first on token rows.
	if _, err = q.Exec(ctx, `SELECT id FROM inbound_trusts ORDER BY id FOR UPDATE`); err != nil {
		return InboundFederationSet{}, fmt.Errorf("store: lock inbound trusts: %w", err)
	}
	// An exchange in flight holds the switch row shared, so the lock above waited for it to commit; this later
	// statement's snapshot sees its token.
	tag, err := q.Exec(ctx, `UPDATE api_tokens SET revoked_at = clock_timestamp() WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now()) AND id IN (SELECT token_id FROM inbound_trust_tokens)`)
	if err != nil {
		return InboundFederationSet{}, fmt.Errorf("store: revoke exchanged tokens: %w", err)
	}
	out.TokensRevoked = int(tag.RowsAffected())
	return out, nil
}

func (s PG) CreateInboundTrust(ctx context.Context, t types.InboundTrust) (types.InboundTrust, string, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.InboundTrust{}, "", fmt.Errorf("store: create inbound trust: %w", err)
	}
	defer db.Rollback(ctx, tx)
	out, sponsor, err := CreateInboundTrustQ(ctx, tx, t)
	if err != nil {
		return types.InboundTrust{}, "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return types.InboundTrust{}, "", fmt.Errorf("store: commit inbound trust: %w", err)
	}
	return out, sponsor, nil
}

// CreateInboundTrustQ is CreateInboundTrust on q.
func CreateInboundTrustQ(ctx context.Context, q Querier, t types.InboundTrust) (types.InboundTrust, string, error) {
	if t.ConfirmedAt.IsZero() || t.ConfirmedBy == "" {
		return types.InboundTrust{}, "", errors.New("store: an inbound trust records where it was confirmed")
	}
	if t.CreatedBy == "" {
		return types.InboundTrust{}, "", errors.New("store: an inbound trust names who added it")
	}
	si, err := lockServiceIdentity(ctx, q, t.ServiceIdentity)
	if err != nil {
		return types.InboundTrust{}, "", err
	}
	if !si.Usable() {
		return types.InboundTrust{}, "", ErrServiceIdentityInactive
	}
	var reserved bool
	if err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM delegates WHERE revoked_at IS NULL AND (idp_client_id = $1 OR audience = $1))`,
		t.Audience).Scan(&reserved); err != nil {
		return types.InboundTrust{}, "", fmt.Errorf("store: create inbound trust: %w", err)
	}
	if reserved {
		return types.InboundTrust{}, "", ErrInboundTrustAudienceReserved
	}
	// Checked before the INSERT so a taken name or triple is a plain refusal, not a failed statement that would end
	// the caller's transaction; the unique violation below is left for two creates racing.
	var taken bool
	if err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM inbound_trusts WHERE name = $1 OR (issuer = $2 AND audience = $3 AND subject = $4))`,
		t.Name, t.Issuer, t.Audience, t.Subject).Scan(&taken); err != nil {
		return types.InboundTrust{}, "", fmt.Errorf("store: create inbound trust: %w", err)
	}
	if taken {
		return types.InboundTrust{}, "", ErrInboundTrustExists
	}
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	out, err := scanInboundTrust(q.QueryRow(ctx, `INSERT INTO inbound_trusts (id, name, issuer, internal_issuer, audience, subject, required_claim,
		required_claim_value, service_name, token_ttl_seconds, max_token_age_seconds, max_live, created_by, confirmed_at,
		confirmed_seq, confirmed_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING `+inboundTrustCols,
		t.ID, t.Name, t.Issuer, t.InternalIssuer, t.Audience, t.Subject, t.RequiredClaim, t.RequiredClaimValue,
		t.ServiceIdentity, t.TokenTTLSeconds, t.MaxTokenAgeSeconds, t.MaxLive, t.CreatedBy, t.ConfirmedAt, t.ConfirmedSeq,
		t.ConfirmedBy))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return types.InboundTrust{}, "", ErrInboundTrustExists
	}
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		return types.InboundTrust{}, "", ErrInboundTrustInvalid
	}
	if err != nil {
		return types.InboundTrust{}, "", fmt.Errorf("store: create inbound trust: %w", err)
	}
	return out, si.Sponsor, nil
}

func (s PG) ConfirmInboundTrust(ctx context.Context, c InboundTrustConfirm) (types.InboundTrust, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.InboundTrust{}, fmt.Errorf("store: confirm inbound trust: %w", err)
	}
	defer db.Rollback(ctx, tx)
	si, err := lockServiceIdentity(ctx, tx, c.Service)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrServiceIdentityInactive) {
		return types.InboundTrust{}, ErrNotFound
	}
	if err != nil {
		return types.InboundTrust{}, err
	}
	if si.Sponsor != c.Sponsor {
		return types.InboundTrust{}, ErrNotFound
	}
	if !si.Usable() {
		return types.InboundTrust{}, ErrServiceIdentityInactive
	}
	out, err := scanInboundTrust(tx.QueryRow(ctx, `UPDATE inbound_trusts SET confirmed_at = $3, confirmed_seq = $4, confirmed_by = $5 WHERE id = $1 AND service_name = $2 RETURNING `+inboundTrustCols,
		c.ID, c.Service, c.At, c.Seq, c.Sponsor))
	if err != nil {
		return types.InboundTrust{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return types.InboundTrust{}, fmt.Errorf("store: commit inbound trust confirm: %w", err)
	}
	return out, nil
}

func (s PG) ListInboundTrusts(ctx context.Context, service string) ([]types.InboundTrust, error) {
	q, args := `SELECT `+inboundTrustCols+` FROM inbound_trusts ORDER BY created_at DESC, id`, []any(nil)
	if service != "" {
		q, args = `SELECT `+inboundTrustCols+` FROM inbound_trusts WHERE service_name = $1 ORDER BY created_at DESC, id`, []any{service}
	}
	return collect(ctx, s.Pool, "list", "inbound trusts", q, args, scanInboundTrust)
}

func (s PG) GetInboundTrust(ctx context.Context, id uuid.UUID) (types.InboundTrust, error) {
	return scanInboundTrust(s.Pool.QueryRow(ctx, `SELECT `+inboundTrustCols+` FROM inbound_trusts WHERE id = $1`, id))
}

func (s PG) MatchInboundTrust(ctx context.Context, issuer, audience, subject string) (types.InboundTrust, error) {
	return scanInboundTrust(s.Pool.QueryRow(ctx, `SELECT `+inboundTrustCols+` FROM inbound_trusts WHERE issuer = $1 AND audience = $2 AND subject = $3`,
		issuer, audience, subject))
}

func (s PG) DeleteInboundTrust(ctx context.Context, d InboundTrustDelete) (types.InboundTrust, int, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.InboundTrust{}, 0, fmt.Errorf("store: delete inbound trust: %w", err)
	}
	defer db.Rollback(ctx, tx)
	if d.Sponsor != "" {
		si, err := lockServiceIdentity(ctx, tx, d.Service)
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrServiceIdentityInactive) {
			return types.InboundTrust{}, 0, ErrNotFound
		}
		if err != nil {
			return types.InboundTrust{}, 0, err
		}
		if si.Sponsor != d.Sponsor {
			return types.InboundTrust{}, 0, ErrNotFound
		}
	}
	trust, err := scanInboundTrust(tx.QueryRow(ctx, `SELECT `+inboundTrustCols+` FROM inbound_trusts WHERE id = $1 AND ($2 = '' OR service_name = $2) FOR UPDATE`,
		d.ID, d.Service))
	if err != nil {
		return types.InboundTrust{}, 0, err
	}
	tag, err := tx.Exec(ctx, `UPDATE api_tokens SET revoked_at = clock_timestamp() WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now()) AND id IN (SELECT token_id FROM inbound_trust_tokens WHERE trust_id = $1)`,
		trust.ID)
	if err != nil {
		return types.InboundTrust{}, 0, fmt.Errorf("store: revoke inbound trust tokens: %w", err)
	}
	if _, err = removeInboundTrusts(ctx, tx, []uuid.UUID{trust.ID}); err != nil {
		return types.InboundTrust{}, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return types.InboundTrust{}, 0, fmt.Errorf("store: commit inbound trust delete: %w", err)
	}
	return trust, int(tag.RowsAffected()), nil
}

// removeInboundTrusts deletes the trusts ids, which the caller holds, with their already-expired token rows (an
// expired exchanged row would otherwise lose its prune with its trust), and returns how many trusts went. Their links
// go by cascade.
func removeInboundTrusts(ctx context.Context, tx Querier, ids []uuid.UUID) (int, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM api_tokens WHERE id IN (SELECT token_id FROM inbound_trust_tokens WHERE trust_id = ANY($1)) AND expires_at <= now() AND NOT EXISTS (SELECT 1 FROM api_token_moves m WHERE m.token_id = api_tokens.id)`,
		ids); err != nil {
		return 0, fmt.Errorf("store: delete expired exchanged tokens: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM inbound_trusts WHERE id = ANY($1)`, ids)
	if err != nil {
		return 0, fmt.Errorf("store: delete inbound trusts: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// removeInboundTrustsOf deletes every trust of the identity service. Its callers hold the identity's row lock and
// call it before they revoke any token.
func removeInboundTrustsOf(ctx context.Context, tx Querier, service string) (int, error) {
	ids, err := collect(ctx, tx, "lock", "inbound trusts", `SELECT id FROM inbound_trusts WHERE service_name = $1 ORDER BY id FOR UPDATE`,
		[]any{service}, func(row pgx.Row) (uuid.UUID, error) {
			var id uuid.UUID
			err := row.Scan(&id)
			return id, err
		})
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return removeInboundTrusts(ctx, tx, ids)
}

// CreateExchangedToken is the one door for an exchanged token. The API has already verified the provider token
// against trust.
func (s PG) CreateExchangedToken(ctx context.Context, trust types.InboundTrust, t types.APIToken, raw string) (types.APIToken, error) {
	if t.Principal != types.ServicePrincipalPrefix+trust.ServiceIdentity || t.Kind != types.APITokenKindInbound || t.ExpiresAt == nil {
		return types.APIToken{}, errors.New("store: an exchanged token is its trust's identity's expiring inbound token")
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.APIToken{}, fmt.Errorf("store: create exchanged token: %w", err)
	}
	defer db.Rollback(ctx, tx)
	var on bool
	err = tx.QueryRow(ctx, `SELECT enabled FROM inbound_federation WHERE id FOR SHARE`).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.APIToken{}, ErrInboundFederationOff
	}
	if err != nil {
		return types.APIToken{}, fmt.Errorf("store: read inbound federation: %w", err)
	}
	if !on {
		return types.APIToken{}, ErrInboundFederationOff
	}
	si, err := lockServiceIdentity(ctx, tx, trust.ServiceIdentity)
	if err != nil {
		return types.APIToken{}, err
	}
	if !si.Usable() {
		return types.APIToken{}, ErrServiceIdentityInactive
	}
	// No row: the trust was deleted, or its identity's sponsor changed, since the API matched it.
	var maxLive int
	var tooLong bool
	err = tx.QueryRow(ctx, `SELECT max_live, $3::timestamptz > now() + (token_ttl_seconds + 60) * interval '1 second' FROM inbound_trusts WHERE id = $1 AND service_name = $2 FOR SHARE`,
		trust.ID, trust.ServiceIdentity, *t.ExpiresAt).Scan(&maxLive, &tooLong)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.APIToken{}, ErrNotFound
	}
	if err != nil {
		return types.APIToken{}, fmt.Errorf("store: read inbound trust: %w", err)
	}
	if tooLong {
		return types.APIToken{}, errors.New("store: an exchanged token lives no longer than its trust's token_ttl_seconds")
	}
	// The identity's row lock above makes this count exact for this transaction.
	var live int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE id IN (SELECT token_id FROM inbound_trust_tokens WHERE trust_id = $1) AND `+apiTokenLive,
		trust.ID).Scan(&live); err != nil {
		return types.APIToken{}, fmt.Errorf("store: count exchanged tokens: %w", err)
	}
	if live >= maxLive {
		return types.APIToken{}, ErrInboundTrustCap
	}
	// A token named by a move is kept: its foreign key would fail the delete, and with it every exchange here.
	if _, err = tx.Exec(ctx, `DELETE FROM api_tokens WHERE id IN (SELECT l.token_id FROM inbound_trust_tokens l WHERE l.trust_id = $1 AND l.expires_at < now() - interval '24 hours' AND NOT EXISTS (SELECT 1 FROM api_token_moves m WHERE m.token_id = l.token_id) ORDER BY l.expires_at LIMIT 100)`,
		trust.ID); err != nil {
		return types.APIToken{}, fmt.Errorf("store: prune exchanged tokens: %w", err)
	}
	// The API never puts WithAdmissionCap on this context, so insertAPIToken's own cap check does nothing here.
	out, err := s.insertAPIToken(ctx, tx, t, raw)
	if err != nil {
		return types.APIToken{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO inbound_trust_tokens (token_id, trust_id, expires_at) VALUES ($1, $2, $3)`,
		out.ID, trust.ID, *out.ExpiresAt); err != nil {
		return types.APIToken{}, fmt.Errorf("store: link exchanged token: %w", err)
	}
	// A token at or before sponsor_since never authenticates: refuse it rather than hand back a dead credential.
	if !out.CreatedAt.After(si.SponsorSince) {
		return types.APIToken{}, ErrServiceIdentityInactive
	}
	if err = tx.Commit(ctx); err != nil {
		return types.APIToken{}, fmt.Errorf("store: commit exchanged token: %w", err)
	}
	return out, nil
}
