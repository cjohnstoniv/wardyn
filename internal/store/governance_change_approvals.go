// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Approvals from more than one person on a held governance change: one row for each person who has
// approved a pending change, the number of approvals a kind needs, and the sign-in records that decide who
// can still approve. The decision transaction reads and writes the approval rows on its own Querier, so a
// row is written with the change row's lock held and commits or rolls back with the decision.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GovernanceChangeApproval is one person's approval of a pending change. Tier and UnitScope are what the
// approval was given as, for display and audit; whether it still counts is worked out from the person's
// latest sign-in record. Dropped names why it stopped counting and is empty while it counts.
type GovernanceChangeApproval struct {
	ChangeID      uuid.UUID
	Approver      string
	ApproverEmail string
	Tier          string
	UnitScope     string
	Reason        string
	ApprovedAt    time.Time
	// APITokenID, APITokenKind and CLISessionID name the token an approval was given with; empty for a
	// browser session.
	APITokenID   uuid.UUID
	APITokenKind string
	CLISessionID uuid.UUID
	RevSeq       int64
	Dropped      string
	DroppedAt    *time.Time
}

// Counts reports whether the approval counts toward applying the change.
func (a GovernanceChangeApproval) Counts() bool { return a.Dropped == "" }

const governanceChangeApprovalCols = `change_id, approver, approver_email, tier, unit_scope, reason, approved_at,
	api_token_id, api_token_kind, cli_session_id, rev_seq, dropped, dropped_at`

func scanGovernanceChangeApproval(row pgx.Row) (GovernanceChangeApproval, error) {
	var a GovernanceChangeApproval
	var tokenID, sessionID *uuid.UUID
	var tokenKind *string
	if err := row.Scan(&a.ChangeID, &a.Approver, &a.ApproverEmail, &a.Tier, &a.UnitScope, &a.Reason, &a.ApprovedAt,
		&tokenID, &tokenKind, &sessionID, &a.RevSeq, &a.Dropped, &a.DroppedAt); err != nil {
		return GovernanceChangeApproval{}, err
	}
	if tokenID != nil {
		a.APITokenID = *tokenID
	}
	if tokenKind != nil {
		a.APITokenKind = *tokenKind
	}
	if sessionID != nil {
		a.CLISessionID = *sessionID
	}
	return a, nil
}

// nullUUID is id as a statement argument, NULL for the zero value.
func nullUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

// nullText is s as a statement argument, NULL for the empty string.
func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ListGovernanceChangeApprovalsQ is every approval row of one change on q, oldest first, dropped rows
// included.
func ListGovernanceChangeApprovalsQ(ctx context.Context, q Querier, changeID uuid.UUID) ([]GovernanceChangeApproval, error) {
	return collect(ctx, q, "list", "governance change approvals", `SELECT `+governanceChangeApprovalCols+`
		FROM governance_change_approvals WHERE change_id = $1 ORDER BY approved_at, approver`,
		[]any{changeID}, scanGovernanceChangeApproval)
}

// PutGovernanceChangeApprovalQ records a's approval on q: a new row, or the person's own row replaced when
// one is there already (a row that was dropped, or one that would drop). The replacement clears dropped and
// dropped_at and takes the new time, reason, tier, scope and token facts.
func PutGovernanceChangeApprovalQ(ctx context.Context, q Querier, a GovernanceChangeApproval) error {
	if a.Approver == "" {
		return errors.New("store: an approval needs an approver")
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO governance_change_approvals (change_id, approver, approver_email, tier, unit_scope, reason,
			api_token_id, api_token_kind, cli_session_id, rev_seq)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (change_id, approver) DO UPDATE SET approver_email = EXCLUDED.approver_email,
			tier = EXCLUDED.tier, unit_scope = EXCLUDED.unit_scope, reason = EXCLUDED.reason, approved_at = now(),
			api_token_id = EXCLUDED.api_token_id, api_token_kind = EXCLUDED.api_token_kind,
			cli_session_id = EXCLUDED.cli_session_id, rev_seq = EXCLUDED.rev_seq, dropped = '', dropped_at = NULL`,
		a.ChangeID, a.Approver, a.ApproverEmail, a.Tier, a.UnitScope, a.Reason,
		nullUUID(a.APITokenID), nullText(a.APITokenKind), nullUUID(a.CLISessionID), a.RevSeq); err != nil {
		return fmt.Errorf("store: record a governance change approval: %w", err)
	}
	return nil
}

// DropGovernanceChangeApprovalQ marks one approval as no longer counting, with why and, when it is known,
// when it stopped. The row stays.
func DropGovernanceChangeApprovalQ(ctx context.Context, q Querier, changeID uuid.UUID, approver, why string, at *time.Time) error {
	if _, err := q.Exec(ctx, `UPDATE governance_change_approvals SET dropped = $3, dropped_at = $4
		WHERE change_id = $1 AND approver = $2`, changeID, approver, why, at); err != nil {
		return fmt.Errorf("store: drop a governance change approval: %w", err)
	}
	return nil
}

// DeleteGovernanceChangeApprovalQ removes one counting approval (the person takes it back) and reports
// whether there was one.
func DeleteGovernanceChangeApprovalQ(ctx context.Context, q Querier, changeID uuid.UUID, approver string) (bool, error) {
	tag, err := q.Exec(ctx, `DELETE FROM governance_change_approvals WHERE change_id = $1 AND approver = $2 AND dropped = ''`,
		changeID, approver)
	if err != nil {
		return false, fmt.Errorf("store: take back a governance change approval: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// APITokenRevokedAtQ is when a token was revoked, with no filter on expiry or on a CLI session's end: found
// is false when the row is gone, and a nil time with found true means it was never revoked.
func APITokenRevokedAtQ(ctx context.Context, q Querier, id uuid.UUID) (at *time.Time, found bool, err error) {
	err = q.QueryRow(ctx, `SELECT revoked_at FROM api_tokens WHERE id = $1`, id).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: read when an API token was revoked: %w", err)
	}
	return at, true, nil
}

// GovernanceApprovalStore is the side of the store that keeps approvals from several people. A store that
// does not implement it holds none, so a change needs the one approval it always did.
type GovernanceApprovalStore interface {
	// ListGovernanceChangeApprovals is the approval rows of the given changes, oldest first; countingOnly
	// leaves out the dropped ones. One statement for any number of changes.
	ListGovernanceChangeApprovals(ctx context.Context, changeIDs []uuid.UUID, countingOnly bool) ([]GovernanceChangeApproval, error)
	// GovernanceApprovalCountFor is the number of approvals a change of kind needs and whether one must
	// hold the security tier: the kind's own row, else the default row ('*'). found is false when neither
	// exists.
	GovernanceApprovalCountFor(ctx context.Context, kind string) (required int, requireSecurity, found bool, err error)
	// ReplaceGovernanceApprovalCounts replaces every row of the counts with rows, in one transaction under the
	// counts' target lock: the admin token's direct write.
	ReplaceGovernanceApprovalCounts(ctx context.Context, rows []GovernanceApprovalCount) error
}

// GovernanceApprovalDefaultKind is the target_kind of the default row of governance_approval_counts.
const GovernanceApprovalDefaultKind = "*"

var _ GovernanceApprovalStore = PG{}

func (s PG) ListGovernanceChangeApprovals(ctx context.Context, changeIDs []uuid.UUID, countingOnly bool) ([]GovernanceChangeApproval, error) {
	if len(changeIDs) == 0 {
		return nil, nil
	}
	return collect(ctx, s.Pool, "list", "governance change approvals", `SELECT `+governanceChangeApprovalCols+`
		FROM governance_change_approvals WHERE change_id = ANY($1) AND (NOT $2 OR dropped = '')
		ORDER BY change_id, approved_at, approver`, []any{changeIDs, countingOnly}, scanGovernanceChangeApproval)
}

func (s PG) GovernanceApprovalCountFor(ctx context.Context, kind string) (required int, requireSecurity, found bool, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT required, require_security FROM governance_approval_counts
		WHERE target_kind IN ($1, $2) ORDER BY (target_kind = $2) LIMIT 1`, kind, GovernanceApprovalDefaultKind).
		Scan(&required, &requireSecurity)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, fmt.Errorf("store: read the approval count of %s: %w", kind, err)
	}
	return required, requireSecurity, true, nil
}

// GovernanceApprovalCount is one row of the counts: a kind's own count, or the default row
// (GovernanceApprovalDefaultKind), and whether one approver must hold the security tier.
type GovernanceApprovalCount struct {
	Kind            string
	Required        int
	RequireSecurity bool
}

// ListGovernanceApprovalCountsQ is every row of the counts on q, the default row included, ordered by kind.
func ListGovernanceApprovalCountsQ(ctx context.Context, q Querier) ([]GovernanceApprovalCount, error) {
	return collect(ctx, q, "list", "approval counts", `SELECT target_kind, required, require_security
		FROM governance_approval_counts ORDER BY target_kind COLLATE "C"`, nil,
		func(row pgx.Row) (GovernanceApprovalCount, error) {
			var c GovernanceApprovalCount
			return c, row.Scan(&c.Kind, &c.Required, &c.RequireSecurity)
		})
}

// ReplaceGovernanceApprovalCountsQ replaces every row of the counts with rows on q. The caller holds the
// counts' target lock (LockGovernanceTarget with types.GovernanceTargetApprovalCounts).
func ReplaceGovernanceApprovalCountsQ(ctx context.Context, q Querier, rows []GovernanceApprovalCount) error {
	if _, err := q.Exec(ctx, `DELETE FROM governance_approval_counts`); err != nil {
		return fmt.Errorf("store: clear the approval counts: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	kinds := make([]string, len(rows))
	required := make([]int32, len(rows))
	security := make([]bool, len(rows))
	for i, r := range rows {
		kinds[i], required[i], security[i] = r.Kind, int32(r.Required), r.RequireSecurity
	}
	if _, err := q.Exec(ctx, `INSERT INTO governance_approval_counts (target_kind, required, require_security)
		SELECT * FROM unnest($1::text[], $2::int[], $3::bool[])`, kinds, required, security); err != nil {
		return fmt.Errorf("store: write the approval counts: %w", err)
	}
	return nil
}

func (s PG) ReplaceGovernanceApprovalCounts(ctx context.Context, rows []GovernanceApprovalCount) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer db.Rollback(ctx, tx) // a no-op after Commit
	if err := LockGovernanceTarget(ctx, tx, types.GovernanceTargetApprovalCounts); err != nil {
		return err
	}
	if err := ReplaceGovernanceApprovalCountsQ(ctx, tx, rows); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SignInRecordCap bounds the sign-in records one pool read returns: the read asks for one more, so a read
// that returns more than the cap says the directory is larger than the pool can count.
const SignInRecordCap = 2000

// SignInRecord is a person's last sign-in to Wardyn itself (principal_groups, door ""), with what the people
// directory says about them: the email it lists them under, and when they were deactivated (nil while they are
// active, by the directory's one rule). Roles is nil for a record written before the roles claim was kept
// beside the groups.
type SignInRecord struct {
	Principal      string
	Email          string
	DirectoryEmail string
	Groups         []string
	Roles          []string
	Truncated      bool
	SignedInAt     time.Time
	DeactivatedAt  *time.Time
}

// Inactive reports whether the directory counts the person as deactivated.
func (r SignInRecord) Inactive() bool { return r.DeactivatedAt != nil }

// signInRecordRows reads sign-in records with the directory's side of each person, over the directory's own
// two expressions so the inactive rule and the email are defined once. $1 narrows to one principal when it
// is not empty; $2 is the number of rows to read.
const signInRecordRows = peopleDirectoryCTEs + `
SELECT g.principal, g.email, g.groups, g.roles, g.truncated, g.signed_in_at, COALESCE(dir.email, ''), dir.deactivated_at
  FROM principal_groups g
  LEFT JOIN dir ON dir.principal = g.principal
 WHERE g.portal = '' AND ($1 = '' OR g.principal = $1)
 ORDER BY g.principal COLLATE "C"
 LIMIT $2`

func scanSignInRecord(row pgx.Row) (SignInRecord, error) {
	var r SignInRecord
	var groups, roles []byte
	if err := row.Scan(&r.Principal, &r.Email, &groups, &roles, &r.Truncated, &r.SignedInAt, &r.DirectoryEmail, &r.DeactivatedAt); err != nil {
		return SignInRecord{}, err
	}
	if err := json.Unmarshal(groups, &r.Groups); err != nil {
		return SignInRecord{}, fmt.Errorf("the stored groups of %q are not a list: %w", r.Principal, err)
	}
	if roles != nil {
		if err := json.Unmarshal(roles, &r.Roles); err != nil {
			return SignInRecord{}, fmt.Errorf("the stored roles of %q are not a list: %w", r.Principal, err)
		}
	}
	return r, nil
}

// ListSignInRecordsQ is the sign-in records a pool is built from, ordered by principal, at most
// SignInRecordCap+1 of them in one statement.
func ListSignInRecordsQ(ctx context.Context, q Querier) ([]SignInRecord, error) {
	return collect(ctx, q, "list", "sign-in records", signInRecordRows, []any{"", SignInRecordCap + 1}, scanSignInRecord)
}

// GetSignInRecordQ is one person's sign-in record, ErrNotFound when they have none.
func GetSignInRecordQ(ctx context.Context, q Querier, principal string) (SignInRecord, error) {
	if principal == "" {
		return SignInRecord{}, ErrNotFound
	}
	rows, err := collect(ctx, q, "read", "a sign-in record", signInRecordRows, []any{principal, 1}, scanSignInRecord)
	if err != nil {
		return SignInRecord{}, err
	}
	if len(rows) == 0 {
		return SignInRecord{}, ErrNotFound
	}
	return rows[0], nil
}

// DirectoryDeactivatedAtQ is when the people directory deactivated principal, nil while they are active or
// when the directory does not know them. It is the same inactive rule SignInRecord carries, asked for a person
// who may have no sign-in record.
func DirectoryDeactivatedAtQ(ctx context.Context, q Querier, principal string) (*time.Time, error) {
	if principal == "" {
		return nil, nil
	}
	var at *time.Time
	err := q.QueryRow(ctx, peopleDirectoryCTEs+`
SELECT deactivated_at FROM dir WHERE principal = $1`, principal).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: read whether a person is deactivated: %w", err)
	}
	return at, nil
}
