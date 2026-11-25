// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

// TokenSeal keeps each GitHub installation token a run minted in run_github_token_seals (migration 0403), sealed
// under the 'cred' subject key of the run's masking-manifest owner, so that the hand-back worker
// (RunGitHubTokenRevokes) can present it to GitHub's DELETE /installation/token after the run's revoke. The owner is
// run_mask_manifest.owner, the name a person's erasure fences and deletes under; a run with no masking manifest keeps
// no copy. Safe for concurrent use.
type TokenSeal struct {
	pool *pgxpool.Pool
	keys maskmanifest.Keys
}

// NewTokenSeal returns a TokenSeal over pool, sealing with keys.
func NewTokenSeal(pool *pgxpool.Pool, keys maskmanifest.Keys) *TokenSeal {
	return &TokenSeal{pool: pool, keys: keys}
}

// tokenSealAADLabel is the AAD's first field, so this AAD never parses as another purpose's (kek.Encode).
const tokenSealAADLabel = "wardyn/github-token-seal/v1"

// The outcomes a row ends with: GitHub took the token back (or answered it was already invalid), it expired, or the
// worker gave up (retries ran out before expiry, or the owner's key is gone).
const (
	sealHandedBack = "handed_back"
	sealExpired    = "expired"
	sealAbandoned  = "abandoned"
)

// errSealOwnerChanged is a manifest owner that differs between prepare and the mint transaction. The owner is written
// once per run at dispatch, so this is not expected; the mint rolls back.
var errSealOwnerChanged = errors.New("broker: the run's masking-manifest owner changed during the mint")

func tokenSealAAD(runID uuid.UUID, jti string, version int) []byte {
	return kek.Encode(tokenSealAADLabel, runID.String(), jti, strconv.Itoa(version))
}

// preparedSeal is a token sealed before the mint transaction opens: the owner it was sealed under, the key version and
// the ciphertext. It never holds the token.
type preparedSeal struct {
	owner   string
	version int
	sealed  []byte
}

// sealState is one row's state as RevokeRun reads it: no value is opened.
type sealState struct {
	expiresAt time.Time
	outcome   string // "" while the row holds the token
}

// dueRow is one claimed row of the hand-back queue.
type dueRow struct {
	runID     uuid.UUID
	jti       string
	owner     string
	version   int
	sealed    []byte
	expiresAt time.Time
	attempts  int
}

// prepare seals token under the run's manifest owner's current key. It runs before any transaction: the key service
// reads, and may create, a key on its own pool connection, which must never wait behind connections the mint
// transaction holds. A run with no masking manifest answers nil, nil (no copy is kept); a fenced one ErrFenced.
func (s *TokenSeal) prepare(ctx context.Context, runID uuid.UUID, jti, token string, _ time.Time) (*preparedSeal, error) {
	var owner string
	var fenced bool
	err := s.pool.QueryRow(ctx, `SELECT owner, fenced_at IS NOT NULL FROM run_mask_manifest WHERE run_id = $1`, runID).Scan(&owner, &fenced)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("broker: read the run's masking manifest: %w", err)
	case fenced:
		return nil, maskmanifest.ErrFenced
	}
	version, key, err := s.keys.Current(ctx, owner, subjectkey.PurposeCred)
	if err != nil {
		return nil, fmt.Errorf("broker: the run owner's key: %w", err)
	}
	defer clear(key)
	sealed, err := kek.Seal(key, []byte(token), tokenSealAAD(runID, jti, version))
	if err != nil {
		return nil, fmt.Errorf("broker: seal the github token: %w", err)
	}
	return &preparedSeal{owner: owner, version: version, sealed: sealed}, nil
}

// sealTx writes p on the mint's transaction, after re-reading the run's manifest FOR SHARE: an erasure that fenced it
// since prepare refuses the write, so the mint rolls back. It also prunes other runs' tombstones and never-queued
// rows 5 minutes past their expiry, skipping any row another transaction holds. p nil writes nothing.
func (s *TokenSeal) sealTx(ctx context.Context, tx Querier, runID uuid.UUID, jti string, expiresAt time.Time, p *preparedSeal) (bool, error) {
	if p == nil {
		return false, nil
	}
	var owner string
	var fenced bool
	err := tx.QueryRow(ctx, `SELECT owner, fenced_at IS NOT NULL FROM run_mask_manifest WHERE run_id = $1 FOR SHARE`, runID).Scan(&owner, &fenced)
	switch {
	case errors.Is(err, errNoRow):
		return false, maskmanifest.ErrFenced
	case err != nil:
		return false, fmt.Errorf("broker: read the run's masking-manifest fence: %w", err)
	case fenced:
		return false, maskmanifest.ErrFenced
	case owner != p.owner:
		return false, errSealOwnerChanged
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_github_token_seals (run_id, jti, owner, key_version, sealed, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, runID, jti, p.owner, p.version, p.sealed, expiresAt); err != nil {
		return false, fmt.Errorf("broker: write the sealed github token: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM run_github_token_seals WHERE (run_id, jti) IN (
		SELECT run_id, jti FROM run_github_token_seals
		 WHERE expires_at < now() - interval '5 minutes' AND (revoke_due_at IS NULL OR ended_at IS NOT NULL)
		 ORDER BY expires_at LIMIT 100 FOR UPDATE SKIP LOCKED)`); err != nil {
		return false, fmt.Errorf("broker: prune expired github token seals: %w", err)
	}
	return true, nil
}

// sealedJTIs reads every row of runID, its expiry and how it ended. No value is opened.
func (s *TokenSeal) sealedJTIs(ctx context.Context, runID uuid.UUID) (map[string]sealState, error) {
	rows, err := s.pool.Query(ctx, `SELECT jti, expires_at, COALESCE(ended_outcome, '') FROM run_github_token_seals WHERE run_id = $1`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]sealState{}
	for rows.Next() {
		var jti string
		var st sealState
		if err := rows.Scan(&jti, &st.expiresAt, &st.outcome); err != nil {
			return nil, err
		}
		out[jti] = st
	}
	return out, rows.Err()
}

// queue asks for the hand-back of runID's held tokens (jtis nil: every one): a row already expired and never asked
// for ends as expired, the rest not yet asked for are due now.
func (s *TokenSeal) queue(ctx context.Context, runID uuid.UUID, jtis []string) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer db.Rollback(ctx, tx)
	if _, err := tx.Exec(ctx, `UPDATE run_github_token_seals SET ended_at = now(), ended_outcome = 'expired', sealed = '\x'::bytea,
		revoke_due_at = NULL, next_attempt_at = NULL
		WHERE run_id = $1 AND ($2::text[] IS NULL OR jti = ANY($2)) AND ended_at IS NULL AND revoke_due_at IS NULL AND expires_at <= now()`,
		runID, jtis); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE run_github_token_seals SET revoke_due_at = now(), next_attempt_at = now()
		WHERE run_id = $1 AND ($2::text[] IS NULL OR jti = ANY($2)) AND ended_at IS NULL AND revoke_due_at IS NULL`,
		runID, jtis); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// claimDue claims up to limit due rows for 2 minutes and answers them with the database's clock at the claim (zero
// when none was due). Two replicas never claim one row; a pass that dies leaves its rows to come back once the hold
// passes.
func (s *TokenSeal) claimDue(ctx context.Context, limit int) ([]dueRow, time.Time, error) {
	rows, err := s.pool.Query(ctx, `UPDATE run_github_token_seals SET next_attempt_at = now() + interval '2 minutes'
		WHERE (run_id, jti) IN (SELECT run_id, jti FROM run_github_token_seals
			WHERE revoke_due_at IS NOT NULL AND ended_at IS NULL AND next_attempt_at <= now()
			ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING run_id, jti, owner, key_version, sealed, expires_at, attempts, now()`, limit)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	var out []dueRow
	var now time.Time
	for rows.Next() {
		var r dueRow
		if err := rows.Scan(&r.runID, &r.jti, &r.owner, &r.version, &r.sealed, &r.expiresAt, &r.attempts, &now); err != nil {
			return nil, time.Time{}, err
		}
		out = append(out, r)
	}
	return out, now, rows.Err()
}

// openRow opens a claimed row's token under its owner's key and the row's AAD: the one place a sealed token becomes
// plaintext, to present it to GitHub.
func (s *TokenSeal) openRow(ctx context.Context, r dueRow) (string, error) {
	key, err := s.keys.Key(ctx, r.owner, subjectkey.PurposeCred, r.version)
	if err != nil {
		return "", fmt.Errorf("broker: the run owner's key: %w", err)
	}
	defer clear(key)
	plain, err := kek.Open(key, r.sealed, tokenSealAAD(r.runID, r.jti, r.version))
	if err != nil {
		return "", errors.Join(errSealUnopenable, err)
	}
	defer clear(plain)
	return string(plain), nil
}

// end makes a held row a tombstone with outcome: its sealed copy goes at once, the row stays until the prune.
func (s *TokenSeal) end(ctx context.Context, runID uuid.UUID, jti, outcome string) error {
	_, err := s.pool.Exec(ctx, `UPDATE run_github_token_seals SET ended_at = now(), ended_outcome = $3, sealed = '\x'::bytea,
		revoke_due_at = NULL, next_attempt_at = NULL WHERE run_id = $1 AND jti = $2 AND ended_at IS NULL`, runID, jti, outcome)
	return err
}

// retryAt records a failed attempt and the next try.
func (s *TokenSeal) retryAt(ctx context.Context, runID uuid.UUID, jti string, attempts int, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE run_github_token_seals SET attempts = $3, next_attempt_at = $4
		WHERE run_id = $1 AND jti = $2 AND ended_at IS NULL`, runID, jti, attempts, at)
	return err
}

// DeleteOwner removes every row sealed under owner, tombstones included, and answers how many: a person's erasure.
func (s *TokenSeal) DeleteOwner(ctx context.Context, owner string) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM run_github_token_seals WHERE owner = $1`, owner)
	if err != nil {
		return 0, fmt.Errorf("broker: delete a person's sealed github tokens: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
