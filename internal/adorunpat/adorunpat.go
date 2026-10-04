// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package adorunpat keeps each `minted_pat` run's current Azure DevOps token in Postgres
// (migration 0121), so a resolve served by any replica hands out the token another created.
// Until 0.8.6 only the process that minted it held the value.
//
// The value is sealed with kek.Seal under the run owner's 'cred' subject key (package
// subjectkey), the AAD binding the run id, the rendering and the key version, so destroying
// that key leaves the row undecryptable. Alongside it sit what the token was built from and the
// run's pause and forced-mint marks: the state one replica's cache used to hold.
//
// A writer checks the run's masking manifest (migration 0109) is not fenced in its own
// transaction, the same durable fence every replica honours when a person is erased, so no
// replica can write a token back after the erasure removed it.
package adorunpat

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

// ErrFenced is a Save for a run whose person is being erased.
var ErrFenced = maskmanifest.ErrFenced

// Keys is the subject-key service the token is sealed under.
type Keys = maskmanifest.Keys

const (
	aadLabel = "wardyn/ado-run-pat/v1"
	// renderingToken names the rendering sealed: the token itself. The other two renderings of
	// it (base64 of ":token", the Basic header value) are derived from it, never stored.
	renderingToken = "token"
)

// Record is one run's token state, as the cache held it.
type Record struct {
	Owner           string // the run owner's secret subject: the key the token is sealed under
	AuthorizationID string
	Token           string // "" when the run has no current token
	Scope           string
	ValidTo         time.Time
	Capabilities    []string
	Paused          bool
	ForcedAt        time.Time
}

// Store is one process's access to the table. Safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	keys Keys
}

// New returns a Store over pool, sealing with keys.
func New(pool *pgxpool.Pool, keys Keys) *Store { return &Store{pool: pool, keys: keys} }

func aad(runID uuid.UUID, rendering string, version int) []byte {
	return kek.Encode(aadLabel, runID.String(), rendering, strconv.Itoa(version))
}

// Load returns runID's record, found false when there is none. An error means the state could
// not be read or opened, which the caller refuses on rather than treating as empty.
func (s *Store) Load(ctx context.Context, runID uuid.UUID) (Record, bool, error) {
	var (
		r       Record
		version *int
		sealed  []byte
		validTo *time.Time
		forced  *time.Time
		caps    string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT owner, authorization_id, scope, valid_to, key_version, sealed, capabilities, paused, forced_at
		FROM ado_run_pat_state WHERE run_id = $1`, runID).
		Scan(&r.Owner, &r.AuthorizationID, &r.Scope, &validTo, &version, &sealed, &caps, &r.Paused, &forced)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, fmt.Errorf("adorunpat: read the run's token state: %w", err)
	}
	if validTo != nil {
		r.ValidTo = *validTo
	}
	if forced != nil {
		r.ForcedAt = *forced
	}
	if caps != "" {
		r.Capabilities = strings.Split(caps, " ")
	}
	if sealed != nil && version != nil {
		key, err := s.keys.Key(ctx, r.Owner, subjectkey.PurposeCred, *version)
		if err != nil {
			return Record{}, false, fmt.Errorf("adorunpat: the owner's key: %w", err)
		}
		defer clear(key)
		plain, err := kek.Open(key, sealed, aad(runID, renderingToken, *version))
		if err != nil {
			return Record{}, false, fmt.Errorf("adorunpat: open the run's token: %w", err)
		}
		r.Token = string(plain)
		clear(plain)
	}
	return r, true, nil
}

// Save writes runID's record for owner (the run's secret subject). ErrFenced when the run's
// person is being erased; nothing is written then.
func (s *Store) Save(ctx context.Context, runID uuid.UUID, owner string, r Record) error {
	if owner == "" {
		return maskmanifest.ErrNoOwner
	}
	var version *int
	var sealed []byte
	if r.Token != "" {
		v, key, err := s.keys.Current(ctx, owner, subjectkey.PurposeCred)
		if err != nil {
			return fmt.Errorf("adorunpat: the owner's key: %w", err)
		}
		defer clear(key)
		blob, err := kek.Seal(key, []byte(r.Token), aad(runID, renderingToken, v))
		if err != nil {
			return fmt.Errorf("adorunpat: seal the run's token: %w", err)
		}
		version, sealed = &v, blob
	}
	var validTo, forced *time.Time
	if !r.ValidTo.IsZero() {
		validTo = &r.ValidTo
	}
	if !r.ForcedAt.IsZero() {
		forced = &r.ForcedAt
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("adorunpat: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var fenced bool
	err = tx.QueryRow(ctx, `SELECT fenced_at IS NOT NULL FROM run_mask_manifest WHERE run_id = $1 FOR SHARE`, runID).Scan(&fenced)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("adorunpat: read the run's fence: %w", err)
	case fenced:
		return ErrFenced
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO ado_run_pat_state (run_id, owner, authorization_id, scope, valid_to, key_version, sealed, capabilities, paused, forced_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (run_id) DO UPDATE SET owner = EXCLUDED.owner, authorization_id = EXCLUDED.authorization_id,
			scope = EXCLUDED.scope, valid_to = EXCLUDED.valid_to, key_version = EXCLUDED.key_version,
			sealed = EXCLUDED.sealed, capabilities = EXCLUDED.capabilities, paused = EXCLUDED.paused,
			forced_at = EXCLUDED.forced_at, updated_at = now()`,
		runID, owner, r.AuthorizationID, r.Scope, validTo, version, sealed, strings.Join(r.Capabilities, " "), r.Paused, forced); err != nil {
		return fmt.Errorf("adorunpat: write the run's token state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("adorunpat: commit: %w", err)
	}
	return nil
}

// Delete removes runID's record: the run ended, or its tokens were revoked.
func (s *Store) Delete(ctx context.Context, runID uuid.UUID) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM ado_run_pat_state WHERE run_id = $1`, runID); err != nil {
		return fmt.Errorf("adorunpat: delete the run's token state: %w", err)
	}
	return nil
}

// DeleteOwner removes every record owner's runs hold, returning how many it removed: the
// erasure of a person.
func (s *Store) DeleteOwner(ctx context.Context, owner string) (int, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM ado_run_pat_state WHERE owner = $1`, owner)
	if err != nil {
		return 0, fmt.Errorf("adorunpat: delete a person's token state: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
