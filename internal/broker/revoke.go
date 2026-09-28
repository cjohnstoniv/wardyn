// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// revokeNote is the honest per-kind revocation story stamped on every
// credential.revoke row: each kind gets a note true to what the cascade
// actually does to it, never one GitHub-shaped sentence applied to all four.
// Nothing is actually invalidated by this cascade, and the note must not
// claim GitHub TTL semantics for an operator-managed PAT that Wardyn cannot
// expire, down-scope, or deny — the identity denylist stops further mints
// only, not use of a secret the sandbox already holds.
func revokeNote(kind string) string {
	switch types.GrantKind(kind) {
	case types.GrantGitHubToken:
		return "github installation tokens expire (<=1h); RevokeRun does not call GitHub's DELETE /installation/token — it must be presented the token itself, and RevokeRun holds only the jti, no copy of the value addressed by it — relying on TTL expiry + identity denylist"
	case types.GrantGitPAT, types.GrantSSHKey:
		return "operator must rotate this secret at the forge — wardyn cannot revoke, expire or down-scope it; the identity denylist only stops further mints"
	case types.GrantAPIKey:
		return "api_key values stay proxy-side and are never resident in the sandbox; the identity denylist stops further mints and the proxy stops injecting"
	default:
		return "no per-credential revocation for this grant kind — relying on TTL expiry + identity denylist"
	}
}

// RevokeRun best-effort revokes credentials minted for a run, part of the
// kill-switch cascade. Honest limitation: nothing here invalidates a
// credential already handed out — RevokeRun has only the jti, never the
// token value, so it cannot call a revoke endpoint that needs the token
// itself. It (a) audits each minted jti as a revoke with the per-kind story
// (revokeNote), and (b) relies on identity.Provider.RevokeRun to deny further
// mints for the run.
//
// The cascade enumerates what the run ACTUALLY minted (credential.mint audit
// rows UNION the approvals burn), not just approvals whose minted_jti was
// burnt — approvals alone would miss every auto-mintable grant and the
// 2nd..Nth mint of a leased git_pat.
func (b *Broker) RevokeRun(ctx context.Context, runID uuid.UUID) error {
	minted, err := b.db.MintedCredentials(ctx, runID)
	if err != nil {
		return err
	}
	actor := spiffeForRun(runID)
	for _, mc := range minted {
		d := map[string]any{
			"jti":  mc.JTI,
			"note": revokeNote(mc.Kind),
		}
		if mc.Kind != "" {
			d["kind"] = mc.Kind
		}
		data, _ := json.Marshal(d)
		ev := types.AuditEvent{
			ID:        uuid.New(),
			Time:      time.Now().UTC(),
			RunID:     &runID,
			ActorType: types.ActorSystem,
			Actor:     "wardyn-broker",
			Action:    "credential.revoke",
			Target:    actor,
			Outcome:   "success",
			Data:      data,
		}
		if err := b.audit.Record(ctx, ev); err != nil {
			audit.LogWriteFailure(ctx, ev, err)
		}
	}
	return nil
}
