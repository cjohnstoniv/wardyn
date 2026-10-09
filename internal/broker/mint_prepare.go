// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

var errMintChanged = errors.New("broker: grant or approval changed during credential preparation")

func (b *Broker) mint(ctx context.Context, caller *identity.Claims, grantID, approvalHint uuid.UUID) (Minted, error) {
	// Bound contention without pinning a connection during stored-secret reads.
	for attempt := 0; attempt < 3; attempt++ {
		before, err := b.mintSnapshot(ctx, caller, grantID, approvalHint)
		if err != nil {
			return Minted{}, err
		}
		var minted Minted
		if before.grantSpec.Kind == types.GrantGitPAT || before.grantSpec.Kind == types.GrantSSHKey {
			minted, err = b.mintKind(ctx, caller, before.grantSpec)
			if err != nil {
				b.auditMint(ctx, caller, grantID, before.approvalID, "", before.grantSpec.Scope, "failure")
				return Minted{}, err
			}
		}
		out, err := b.commitMint(ctx, caller, grantID, approvalHint, before, minted)
		if !errors.Is(err, errMintChanged) {
			return out, err
		}
	}
	return Minted{}, errMintChanged
}

func (b *Broker) mintSnapshot(ctx context.Context, caller *identity.Claims, grantID, approvalHint uuid.UUID) (grantApprovalRow, error) {
	tx, err := b.db.BeginReadCommitted(ctx)
	if err != nil {
		return grantApprovalRow{}, fmt.Errorf("broker: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	row, _, err := b.checkedMintRow(ctx, tx, caller, grantID, approvalHint)
	return row, err
}

// Both reads enforce all authorization gates. Only the second transaction burns
// the approval and records success; changed authority discards the preparation.
func (b *Broker) checkedMintRow(ctx context.Context, tx Tx, caller *identity.Claims, grantID, approvalHint uuid.UUID) (grantApprovalRow, bool, error) {
	row, err := selectGrantApprovalForUpdate(ctx, tx, grantID, approvalHint)
	if err != nil {
		return grantApprovalRow{}, false, err
	}
	if row.grantRunID != caller.RunID {
		return grantApprovalRow{}, false, ErrRunMismatch
	}

	if row.grantSpec.Kind == types.GrantCloudSTS {
		b.auditRefusedMint(ctx, tx, caller, grantID, row.approvalID, row.grantSpec.Scope, "denied")
		return grantApprovalRow{}, false, ErrRequiresSPIRE
	}

	// Single-use guard: a written minted_jti blocks re-mint — UNLESS the human
	// scoped their decision to the whole run, which is the B2 lease (see
	// leaseCoversRemint). `leased` rides the rest of this function: it suppresses
	// the minted_jti burn below (already burnt, and the conditional UPDATE would
	// return 0 rows and fail the mint closed) and it is stamped on the audit
	// event, because a lease widens what ONE approval authorizes and the stream
	// has to say which mints were the human's and which were the lease's.
	leased := false
	if row.mintedJTI != "" {
		if !leaseCoversRemint(row) {
			return grantApprovalRow{}, false, ErrAlreadyMinted
		}
		leased = true
	}

	// Chokepoint self-enforcement: an approval-required grant must carry an
	// approval row. MintForGrant's routing guarantees this, but re-check it in
	// the tx so the mint is self-contained for EVERY entry point (a caller
	// passing a Nil approval hint would otherwise auto-mint an approval-gated
	// grant that has no approval row). Fail closed.
	if row.grantSpec.RequiresApproval && !row.hasApproval {
		b.auditRefusedMint(ctx, tx, caller, grantID, uuid.Nil, row.grantSpec.Scope, "denied")
		return grantApprovalRow{}, false, ErrNotApproved
	}

	if row.hasApproval {
		// Anything that is not APPROVED refuses here, so DENIED, EXPIRED and
		// CANCELLED all land on ErrNotApproved — the mint chokepoint's
		// fail-closed default needs no per-state arm, and a state it has never
		// heard of refuses too.
		if row.approvalState != types.ApprovalApproved {
			if row.approvalState == types.ApprovalPending {
				return grantApprovalRow{}, false, ErrApprovalPending{ApprovalID: row.approvalID}
			}
			return grantApprovalRow{}, false, ErrNotApproved
		}
		if row.approvalRunID != caller.RunID {
			return grantApprovalRow{}, false, ErrRunMismatch
		}
		// No-widening: the approver saw exactly requested_scope; it must
		// deep-equal the grant spec scope.
		if !jsonScopeEqual(row.requestedScope, row.grantSpec.Scope) {
			b.auditRefusedMint(ctx, tx, caller, grantID, row.approvalID, row.grantSpec.Scope, "denied")
			return grantApprovalRow{}, false, ErrScopeMismatch
		}
	}

	// Kill-switch: refuse to mint for a run that has already been revoked. The
	// check runs inside the mint tx so a durably-recorded revocation blocks a
	// subsequent mint — closing the gap where a mint reaching the broker after
	// the run was killed still produced a live credential. The sub-RTT concurrent
	// case (a revoke committing during this tx, after the check) is the published
	// 1h-minted-token residual, not closed here. See threatmodel §5 #7.
	if revoked, err := runRevoked(ctx, tx, row.grantRunID); err != nil {
		return grantApprovalRow{}, false, err
	} else if revoked {
		b.auditRefusedMint(ctx, tx, caller, grantID, row.approvalID, row.grantSpec.Scope, "denied")
		return grantApprovalRow{}, false, ErrRunRevoked
	}

	return row, leased, nil
}
