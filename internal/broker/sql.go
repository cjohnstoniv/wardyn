// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/notify"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errNoRow is returned by Querier.QueryRow.Scan when no row matched (pgxAdapter translates pgx.ErrNoRows into it, dependency-light).
var errNoRow = errors.New("broker: no row")

// loadGrant reads a grant's spec and run id (no lock; routing pre-check only).
func (b *Broker) loadGrant(ctx context.Context, grantID uuid.UUID) (types.GrantSpec, uuid.UUID, error) {
	tx, err := b.db.BeginReadCommitted(ctx)
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

// selectLiveCredentialApproval is ensureApproval's ONE lookup spelling
// (newest non-EXPIRED approval), shared by the pre-insert read and post-insert re-select.
const selectLiveCredentialApproval = `
	SELECT id, state, requested_scope, minted_jti, reason
	  FROM approvals
	 WHERE grant_id = $1 AND kind = 'credential' AND state <> 'EXPIRED'
	 ORDER BY requested_at DESC
	 LIMIT 1`

// ensureApproval finds the credential approval for a grant or creates a
// PENDING one (requested_scope = the grant spec scope), via the partial
// unique index approvals_pending_credential_uniq (racing SELECT-then-INSERT,
// ON CONFLICT DO NOTHING, re-select the winner). EXPIRED rows are skipped so
// a swept approval re-raises as fresh PENDING; DENIED/CANCELLED are terminal
// and stay mapped to ErrApprovalDenied.
func (b *Broker) ensureApproval(ctx context.Context, grantID, runID uuid.UUID, spec types.GrantSpec) (types.ApprovalRequest, error) {
	tx, err := b.db.BeginReadCommitted(ctx)
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
	err = tx.QueryRow(ctx, selectLiveCredentialApproval, grantID).
		Scan(&ap.ID, &ap.State, &ap.RequestedScope, &ap.MintedJTI, &ap.Reason)
	switch {
	case err == nil:
		ap.RunID = runID
		ap.GrantID = &grantID
		ap.Kind = types.ApprovalCredential
		committed = true // read-only; nothing to commit but keep tx tidy
		_ = tx.Commit(ctx)
		return ap, nil
	case errors.Is(err, errNoRow):
		// PENDING insert; a concurrent double-insert loses via DO NOTHING.
		afterCommit, err := insertPendingApproval(ctx, tx, uuid.New(), runID, grantID, []byte(spec.Scope))
		if err != nil {
			return types.ApprovalRequest{}, err
		}
		err = tx.QueryRow(ctx, selectLiveCredentialApproval, grantID).
			Scan(&ap.ID, &ap.State, &ap.RequestedScope, &ap.MintedJTI, &ap.Reason)
		if err != nil {
			return types.ApprovalRequest{}, fmt.Errorf("broker: re-select approval after insert: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return types.ApprovalRequest{}, fmt.Errorf("broker: commit approval insert: %w", err)
		}
		committed = true
		afterCommit(ctx)
		ap.RunID = runID
		ap.GrantID = &grantID
		ap.Kind = types.ApprovalCredential
		return ap, nil
	default:
		return types.ApprovalRequest{}, fmt.Errorf("broker: select approval: %w", err)
	}
}

// insertCredentialApproval is the broker's one approvals INSERT. A concurrent double-insert loses via
// DO NOTHING, which is also what makes the loser enqueue no notification.
const insertCredentialApproval = `INSERT INTO approvals (id, run_id, grant_id, kind, requested_scope, state)
	 VALUES ($1, $2, $3, 'credential', $4, 'PENDING')
	 ON CONFLICT (grant_id) WHERE kind = 'credential' AND state = 'PENDING' DO NOTHING`

// insertPendingApproval inserts the PENDING approval and, when approval notifications are configured,
// its outbox rows in the SAME statement (a CTE over the inserted row), so a lost race inserts neither.
// The returned func records a budget suppression and must run after the transaction commits.
func insertPendingApproval(ctx context.Context, tx Tx, id, runID, grantID uuid.UUID, scope []byte) (func(context.Context), error) {
	var profileID *uuid.UUID
	if notify.Enabled() {
		// Same transaction as the insert: a concurrent profile change cannot route this approval by stale data.
		if err := tx.QueryRow(ctx, notify.ProfileSQL, runID).Scan(&profileID); err != nil && !errors.Is(err, errNoRow) {
			return nil, fmt.Errorf("broker: read run profile: %w", err)
		}
	}
	enq := notify.NewEnqueue(types.ApprovalCredential, profileID)
	if !enq.On() {
		if _, err := tx.Exec(ctx, insertCredentialApproval, id, runID, grantID, scope); err != nil {
			return nil, fmt.Errorf("broker: insert approval: %w", err)
		}
		return func(context.Context) {}, nil
	}
	var raised, queued int64
	args := append([]any{id, runID, grantID, scope}, enq.Args()...)
	err := tx.QueryRow(ctx,
		`WITH ins AS (`+insertCredentialApproval+` RETURNING id, run_id, requested_at)`+enq.Tail(5), args...).
		Scan(&raised, &queued)
	if err != nil {
		return nil, fmt.Errorf("broker: insert approval: %w", err)
	}
	return func(ctx context.Context) { enq.Done(ctx, id, runID, raised, queued) }, nil
}

// selectGrantApprovalForUpdate is the authoritative read inside the mint tx:
// FOR UPDATE-locks the grant row LEFT-joined to its credential approval
// (narrowed to approvalHint if non-Nil), so auto-mint and the gated path share one query.
func selectGrantApprovalForUpdate(ctx context.Context, tx Tx, grantID, approvalHint uuid.UUID) (grantApprovalRow, error) {
	var (
		r         grantApprovalRow
		specRaw   []byte
		apID      *uuid.UUID
		apRunID   *uuid.UUID
		apState   *string
		reqScope  []byte
		mintedJTI *string
		decScope  *string
	)
	// FOR UPDATE OF g locks only the grant row (Postgres forbids locking the
	// nullable side of an outer join); single-use instead relies on the
	// rows-affected check on broker.mint's conditional minted_jti UPDATE.
	const q = `
		SELECT g.id, g.run_id, g.spec,
		       a.id, a.run_id, a.state, a.requested_scope, a.minted_jti, a.decision_scope
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
		Scan(&r.grantID, &r.grantRunID, &specRaw, &apID, &apRunID, &apState, &reqScope, &mintedJTI, &decScope)
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
		// RAW, never Normalize()d — "" must stay "" through leaseCoversRemint.
		if decScope != nil {
			r.decisionScope = types.ApprovalScope(*decScope)
		}
	}
	return r, nil
}

// runRevoked reports whether the run has been revoked (Identity.RevokeRun
// inserts into identity_revocations), checked inside the mint tx so a
// durably-recorded revocation fails the mint (not full race-closed: a revoke
// committing after this read still yields one <=1h token). Matches ANY row, fail-closed.
func runRevoked(ctx context.Context, tx Tx, runID uuid.UUID) (bool, error) {
	var revoked bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM identity_revocations WHERE run_id = $1)`, runID).Scan(&revoked)
	if err != nil {
		return false, fmt.Errorf("broker: check revocation: %w", err)
	}
	return revoked, nil
}

// jsonScopeEqual reports whether two JSON scopes are semantically equal (key
// order/whitespace insensitive) — the no-widening check: minted must EXACTLY match approved.
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
