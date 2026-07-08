// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errNoRow is returned by Querier.QueryRow.Scan when no row matched. pgx
// returns pgx.ErrNoRows; the fake returns this. We match on either via the
// caller-supplied isNoRows or by string, but to stay dependency-light the
// pgxAdapter translates pgx.ErrNoRows into errNoRow.
var errNoRow = errors.New("broker: no row")

// loadGrant reads a grant's spec and run id (no lock; routing pre-check only).
func (b *Broker) loadGrant(ctx context.Context, grantID uuid.UUID) (types.GrantSpec, uuid.UUID, error) {
	tx, err := b.db.Begin(ctx)
	if err != nil {
		return types.GrantSpec{}, uuid.Nil, fmt.Errorf("broker: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var specRaw []byte
	var runID uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT run_id, spec FROM credential_grants WHERE id = $1`, grantID).
		Scan(&runID, &specRaw)
	if err != nil {
		if errors.Is(err, errNoRow) {
			return types.GrantSpec{}, uuid.Nil, ErrGrantNotFound
		}
		return types.GrantSpec{}, uuid.Nil, fmt.Errorf("broker: load grant: %w", err)
	}
	var spec types.GrantSpec
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		return types.GrantSpec{}, uuid.Nil, fmt.Errorf("broker: decode grant spec: %w", err)
	}
	return spec, runID, nil
}

// ensureApproval finds the credential approval for a grant or creates a PENDING
// one (requested_scope = the grant spec scope — exactly what the approver will
// see). It returns the current approval state. Concurrency: migration
// 0002_approval_uniqueness adds the partial unique index
// approvals_pending_credential_uniq (one PENDING credential approval per
// grant), so the SELECT-then-INSERT runs inside one tx with ON CONFLICT DO
// NOTHING; a racing double-insert loses harmlessly and the re-select returns
// the single winner.
func (b *Broker) ensureApproval(ctx context.Context, grantID, runID uuid.UUID, spec types.GrantSpec) (types.ApprovalRequest, error) {
	tx, err := b.db.Begin(ctx)
	if err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("broker: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	var ap types.ApprovalRequest
	var decidedAt *time.Time
	err = tx.QueryRow(ctx,
		`SELECT id, state, requested_scope, minted_jti, reason
		   FROM approvals
		  WHERE grant_id = $1 AND kind = 'credential'
		  ORDER BY requested_at DESC
		  LIMIT 1`, grantID).
		Scan(&ap.ID, &ap.State, &ap.RequestedScope, &ap.MintedJTI, &ap.Reason)
	_ = decidedAt
	switch {
	case err == nil:
		ap.RunID = runID
		ap.GrantID = &grantID
		ap.Kind = types.ApprovalCredential
		committed = true // read-only; nothing to commit but keep tx tidy
		_ = tx.Commit(ctx)
		return ap, nil
	case errors.Is(err, errNoRow):
		// Create a PENDING approval whose requested_scope == grant spec scope.
		// The partial unique index approvals_pending_credential_uniq (one
		// PENDING credential approval per grant) makes a concurrent double-
		// insert lose via DO NOTHING; the re-select below returns the winner.
		newID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO approvals (id, run_id, grant_id, kind, requested_scope, state)
			 VALUES ($1, $2, $3, 'credential', $4, 'PENDING')
			 ON CONFLICT (grant_id) WHERE kind = 'credential' AND state = 'PENDING' DO NOTHING`,
			newID, runID, grantID, []byte(spec.Scope)); err != nil {
			return types.ApprovalRequest{}, fmt.Errorf("broker: insert approval: %w", err)
		}
		err = tx.QueryRow(ctx,
			`SELECT id, state, requested_scope, minted_jti, reason
			   FROM approvals
			  WHERE grant_id = $1 AND kind = 'credential'
			  ORDER BY requested_at DESC
			  LIMIT 1`, grantID).
			Scan(&ap.ID, &ap.State, &ap.RequestedScope, &ap.MintedJTI, &ap.Reason)
		if err != nil {
			return types.ApprovalRequest{}, fmt.Errorf("broker: re-select approval after insert: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return types.ApprovalRequest{}, fmt.Errorf("broker: commit approval insert: %w", err)
		}
		committed = true
		ap.RunID = runID
		ap.GrantID = &grantID
		ap.Kind = types.ApprovalCredential
		return ap, nil
	default:
		return types.ApprovalRequest{}, fmt.Errorf("broker: select approval: %w", err)
	}
}

// selectGrantApprovalForUpdate is the authoritative read inside the mint tx. It
// locks the grant row (FOR UPDATE) joined LEFT to its credential approval so
// the auto-mint path (no approval) and the gated path share one query. When
// approvalHint is non-Nil the join is narrowed to that approval id.
func selectGrantApprovalForUpdate(ctx context.Context, tx Tx, grantID, approvalHint uuid.UUID) (grantApprovalRow, error) {
	var (
		r         grantApprovalRow
		specRaw   []byte
		apID      *uuid.UUID
		apRunID   *uuid.UUID
		apState   *string
		reqScope  []byte
		mintedJTI *string
	)
	// FOR UPDATE OF g locks the grant row, serializing concurrent mints for the
	// same grant. It CANNOT also lock `a`: Postgres forbids FOR UPDATE on the
	// nullable side of an outer join (the auto-mint path LEFT-JOINs no approval).
	// This means single-use CANNOT rely on the a.minted_jti value scanned here: a
	// contender that blocks on the g lock mid-tx resumes on its ORIGINAL statement
	// snapshot and reads a stale minted_jti='' — EvalPlanQual re-checks only the
	// locked tuple (g), never re-fetching the non-locked approval row on the
	// nullable side. So the row.mintedJTI fast-path guard catches only contenders
	// whose snapshot postdates the winner's commit; the lock-blocked-waiter case
	// is caught ONLY by the rows-affected check on the conditional minted_jti
	// UPDATE (broker.mint) — that check is the load-bearing single-use guarantee.
	const q = `
		SELECT g.id, g.run_id, g.spec,
		       a.id, a.run_id, a.state, a.requested_scope, a.minted_jti
		  FROM credential_grants g
		  LEFT JOIN approvals a
		         ON a.grant_id = g.id
		        AND a.kind = 'credential'
		        AND ($2::uuid IS NULL OR a.id = $2::uuid)
		 WHERE g.id = $1
		 ORDER BY a.requested_at DESC NULLS LAST
		 LIMIT 1
		 FOR UPDATE OF g`
	var hint any
	if approvalHint == uuid.Nil {
		hint = nil
	} else {
		hint = approvalHint
	}
	err := tx.QueryRow(ctx, q, grantID, hint).
		Scan(&r.grantID, &r.grantRunID, &specRaw, &apID, &apRunID, &apState, &reqScope, &mintedJTI)
	if err != nil {
		if errors.Is(err, errNoRow) {
			return grantApprovalRow{}, ErrGrantNotFound
		}
		return grantApprovalRow{}, fmt.Errorf("broker: select grant+approval for update: %w", err)
	}
	if err := json.Unmarshal(specRaw, &r.grantSpec); err != nil {
		return grantApprovalRow{}, fmt.Errorf("broker: decode grant spec: %w", err)
	}
	if apID != nil {
		r.hasApproval = true
		r.approvalID = *apID
		if apRunID != nil {
			r.approvalRunID = *apRunID
		}
		if apState != nil {
			r.approvalState = types.ApprovalState(*apState)
		}
		r.requestedScope = json.RawMessage(reqScope)
		if mintedJTI != nil {
			r.mintedJTI = *mintedJTI
		}
	}
	return r, nil
}

// runRevoked reports whether the run has been revoked. The kill-switch cascade
// (Identity.RevokeRun) INSERTs into identity_revocations; checking it inside the
// mint tx fails a mint closed once a revocation is durably recorded. This is a
// tightening, not a full race close: a revoke that commits during this tx (after
// this read) still yields a <=1h token — the published minted-token residual.
//
// It matches ANY identity_revocations row for the run (run-level or a per-JTI
// entry, both of which populate run_id), so a single-JTI revocation would block
// all further mints for the run. That is deliberately fail-closed: if any
// credential for a run was revoked, refusing new mints for that run is the safe
// direction. (No per-JTI revoke caller exists today; run-level is the live path.)
func runRevoked(ctx context.Context, tx Tx, runID uuid.UUID) (bool, error) {
	var revoked bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM identity_revocations WHERE run_id = $1)`, runID).Scan(&revoked)
	if err != nil {
		return false, fmt.Errorf("broker: check revocation: %w", err)
	}
	return revoked, nil
}

// mintedJTIsForRun lists minted_jti values for a run's credential approvals.
func (b *Broker) mintedJTIsForRun(ctx context.Context, runID uuid.UUID) ([]string, error) {
	// Single-row-at-a-time API: we iterate via a small helper query. The
	// Querier surface is row-oriented; for the revoke cascade we accept a
	// dedicated rows method on TxBeginner implementations that support it.
	rb, ok := b.db.(rowsBeginner)
	if !ok {
		// Fakes that don't implement bulk reads return nothing to revoke.
		return nil, nil
	}
	return rb.MintedJTIs(ctx, runID)
}

// rowsBeginner is the optional bulk-read surface used only by RevokeRun. The
// pgx adapter implements it; minimal fakes may omit it.
type rowsBeginner interface {
	MintedJTIs(ctx context.Context, runID uuid.UUID) ([]string, error)
}

// jsonScopeEqual reports whether two JSON scopes are semantically equal,
// independent of key ordering or insignificant whitespace. This is the
// no-widening comparison: the minted scope must be EXACTLY the approved scope.
func jsonScopeEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	ca, err := canonicalJSON(a)
	if err != nil {
		return false
	}
	cb, err := canonicalJSON(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

// canonicalJSON re-encodes JSON with sorted object keys so byte comparison is
// order-insensitive. encoding/json sorts map keys on marshal.
func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // preserve numeric form so 1 != 1.0 only if textually so
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
