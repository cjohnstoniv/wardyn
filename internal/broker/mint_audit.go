// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/identity"
)

// auditRefusedMint records a denied or failed mint from inside mint, and ends
// the mint transaction first. The Recorder borrows its own pool connection, so
// auditing while tx still holds one (and the grant row lock) costs two
// connections per refusal, and on a pool of one waits on itself forever. The
// row cannot ride tx either: these arms roll back, which would discard it.
// mint's deferred Rollback then finds the transaction closed and does nothing.
func (b *Broker) auditRefusedMint(ctx context.Context, tx Tx, caller *identity.Claims, grantID, approvalID uuid.UUID, scope json.RawMessage, outcome string) {
	_ = tx.Rollback(ctx)
	b.auditMint(ctx, caller, grantID, approvalID, "", scope, outcome)
}

// auditMint emits a credential.mint audit event via the Recorder chain. Used for
// the DENIED and FAILURE outcomes. Those arms roll the mint tx back, so the row
// cannot ride it, and the Recorder's own spooling fallback is the durability
// they get. The Recorder takes its own pool connection, so a caller inside mint
// must end the tx BEFORE calling this (auditRefusedMint does); MintForGrant
// calls it with no tx open. The SUCCESS outcome does NOT go through here: it
// rides the mint tx (insertAuditEventTx) so the audit row and the minted_jti
// burn commit atomically.
func (b *Broker) auditMint(ctx context.Context, caller *identity.Claims, grantID, approvalID uuid.UUID, jti string, scope json.RawMessage, outcome string) {
	ev := mintEvent(caller, grantID, approvalID, jti, scope, outcome)
	if err := b.audit.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
}
