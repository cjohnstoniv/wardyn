// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"fmt"
	"reflect"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/google/uuid"
)

// commitMint atomically burns the approval and records the prepared mint after
// revalidating its locked authorization snapshot. approvalHint, when
// non-Nil, narrows the SELECT to that approval id (used after MintForGrant's
// ensureApproval); when Nil it resolves the approval (or auto-approval) by
// grant id. caller.SPIFFEID is the audit actor.
func (b *Broker) commitMint(ctx context.Context, caller *identity.Claims, grantID, approvalHint uuid.UUID, prepared grantApprovalRow, minted Minted) (Minted, error) {
	tx, err := b.db.BeginReadCommitted(ctx)
	if err != nil {
		b.discardMinted(ctx, minted)
		return Minted{}, fmt.Errorf("broker: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			b.discardMinted(ctx, minted)
		}
	}()

	row, leased, err := b.checkedMintRow(ctx, tx, caller, grantID, approvalHint)
	if err != nil {
		return Minted{}, err
	}
	if !reflect.DeepEqual(row, prepared) {
		return Minted{}, errMintChanged
	}
	// GitHub issuance keeps its existing grant-lock serialization. Only stored
	// secrets need preparation outside the transaction to release the pool.
	if minted.Kind == "" {
		minted, err = b.mintKind(ctx, caller, row.grantSpec)
		if err != nil {
			b.auditRefusedMint(ctx, tx, caller, grantID, row.approvalID, row.grantSpec.Scope, "failure")
			return Minted{}, err
		}
	}
	minted.GrantID = grantID
	minted.ApprovalID = row.approvalID

	// Write minted_jti back in the SAME transaction (the provable join), and
	// require the conditional UPDATE to affect exactly one row. This rows-affected
	// check is LOAD-BEARING for single-use, not a backstop: the row.mintedJTI fast
	// path above only catches contenders whose statement snapshot postdates the
	// winner's commit. A contender that BLOCKS on the FOR UPDATE OF g lock mid-tx
	// resumes on its ORIGINAL snapshot and reads a stale minted_jti='' from the
	// joined approval row — Postgres runs EvalPlanQual only for the locked tuple
	// (g), never re-fetching the non-locked, nullable-side approval — so it passes
	// the fast path and mints a real token. Only this conditional UPDATE, which
	// re-checks minted_jti='' against the latest committed row and returns 0 rows,
	// stops that second credential from being returned. On 0 rows we fail closed;
	// the deferred Rollback discards the tx, so the minted token above is never
	// returned and expires at its <=1h TTL. (Proven by a two-session PG16
	// experiment; see TestPG_ConcurrentMintOnApproval_ExactlyOnce.)
	//
	// Skipped under a lease, and that is the lease: the burn already happened on
	// the first mint, so this conditional UPDATE would match 0 rows and fail a
	// re-mint the human explicitly authorized. Nothing else is skipped — the
	// approval state, run ownership, no-widening and kill-switch checks above all
	// still ran on this transaction, so a revoked run's lease is dead the moment
	// the revocation commits.
	if row.hasApproval && !leased {
		n, err := tx.Exec(ctx,
			`UPDATE approvals SET minted_jti = $1 WHERE id = $2 AND minted_jti = ''`,
			minted.JTI, row.approvalID)
		if err != nil {
			return Minted{}, fmt.Errorf("broker: write minted_jti: %w", err)
		}
		if n != 1 {
			// A concurrent mint already claimed this approval. The token minted
			// above is discarded (never returned); audit the loss so the throwaway
			// mint is visible in the trail rather than silent.
			b.auditRefusedMint(ctx, tx, caller, grantID, row.approvalID, row.grantSpec.Scope, "denied")
			return Minted{}, ErrAlreadyMinted
		}
	}

	// Write the credential.mint SUCCESS row on the SAME tx as the minted_jti
	// burn, so the audit event and the single-use burn commit atomically. A
	// post-commit write on a SEPARATE connection would open a crash window:
	// minted_jti committed, then a crash before the audit write burns the approval
	// with NO credential.mint row and nothing delivered — the git helper's retry
	// then gets 409 already_minted forever. Fail CLOSED: if the durable record
	// cannot be written, roll the whole mint back rather than hand out an
	// unrecorded credential.
	mintEv := mintEvent(caller, grantID, row.approvalID, minted.JTI, row.grantSpec.Scope, "success")
	mintEv.Data = withSecretScope(mintEv.Data, minted)
	if leased {
		// A lease widens what ONE human decision authorizes, so the stream must
		// say which mints the human made and which the lease did — B2's own
		// condition for the feature. Stamped on the event rather than raised as a
		// separate action so an existing credential.mint consumer sees it without
		// subscribing to anything new.
		mintEv.Data = withLeaseMarker(mintEv.Data, row.decisionScope)
	}
	if err := insertAuditEventTx(ctx, tx, mintEv); err != nil {
		return Minted{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Minted{}, fmt.Errorf("broker: commit mint tx: %w", err)
	}
	committed = true

	// The durable record is committed above (in-tx). Fan the SAME event out to the
	// SIEM sinks — best-effort, primary store NOT re-written (see the siem field
	// doc). The Fanout logs any per-child failure itself. WithoutCancel: the
	// credential is already the caller's, so a client hanging up now must not
	// cost its SIEM record (SyslogSink.Emit skips on a done ctx). Every sink
	// enqueues or writes locally, so detaching cannot pin the request.
	if b.siem != nil {
		_ = b.siem.Emit(context.WithoutCancel(ctx), mintEv)
	}

	// Register the minted token — and, for ssh_key, its known_hosts material —
	// in the mask registry so PTY/asciicast streams can mask verbatim
	// occurrences of the credential. A nil registry is a no-op; only
	// value-bearing kinds (github_token, git_pat, ssh_key) set Token — api_key
	// never does (its value stays proxy-side). KnownHosts is mask-registered too:
	// see the Minted.KnownHosts doc comment for why a nominally
	// public field still gets this treatment.
	// A value that cannot be put on record is not handed out: the mint is
	// committed and audited, and the caller gets the error instead.
	if err := b.maskMinted(caller.RunID, minted); err != nil {
		return Minted{}, err
	}

	return minted, nil
}
