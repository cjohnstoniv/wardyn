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
// credential.revoke row. NOTHING is actually invalidated by this cascade — the
// note is the only place the audit trail says WHY, and one constant note lied
// about two of the four kinds (F013): it claimed GitHub TTL semantics for an
// operator-managed PAT that Wardyn cannot expire, down-scope, or deny (the
// identity denylist stops further MINTS, not use of a secret the sandbox already
// holds — see mintGitPAT/mintSSHKey in broker_mint_kinds.go, "the honesty ceiling
// for this grant kind").
func revokeNote(kind string) string {
	switch types.GrantKind(kind) {
	case types.GrantGitHubToken:
		return "github installation tokens expire (<=1h); wardyn does not call GitHub's DELETE /installation/token — it must be presented the token itself, and RevokeRun holds only the jti, no copy of the value addressed by it — relying on TTL expiry + identity denylist"
	case types.GrantGitPAT, types.GrantSSHKey:
		return "operator must rotate this secret at the forge — wardyn cannot revoke, expire or down-scope it; the identity denylist only stops further mints"
	case types.GrantAPIKey:
		return "api_key values stay proxy-side and are never resident in the sandbox; the identity denylist stops further mints and the proxy stops injecting"
	default:
		return "no per-credential revocation for this grant kind — relying on TTL expiry + identity denylist"
	}
}

// RevokeRun best-effort revokes credentials minted for a run, part of the
// kill-switch cascade. HONEST LIMITATION: nothing here invalidates a credential
// that was already handed out. GitHub App installation tokens are not revoked
// individually before their (<=1h) expiry: GitHub's DELETE /installation/token
// endpoint is real, but it can only be called by presenting the token itself,
// and RevokeRun has only the jti. The token value is never PERSISTED (mintGitHub
// returns it in Minted{Token: token}, broker.go), but in-memory copies DO exist
// and none is addressable by jti or consulted here: mint() registers the bytes in
// the run's mask corpus (b.maskReg.Add(caller.RunID, minted.Token), broker.go;
// evicted by the RunSecretGrace sweep, runs_lifecycle.go) so PTY/asciicast
// streams can mask them, and the proxy sidecar's brokered-credential path may
// cache a minted token per grant for re-use within one clone. Neither is a
// revocation handle — and a git_pat/ssh_key is an operator-managed secret Wardyn
// never owned. We
// therefore (a) audit each minted jti as a revoke with
// outcome=success for the audit join, recording in the event data the per-KIND
// story (revokeNote) rather than one GitHub-shaped sentence, and (b) rely on
// identity revocation (identity.Provider.RevokeRun, called by the kill cascade)
// to deny any further mints for the run.
//
// The cascade enumerates what the run ACTUALLY minted (mintedCredentialsSQL:
// credential.mint audit rows UNION the approvals burn), not just the approvals
// whose minted_jti was burnt. Sourcing it from approvals alone emitted ZERO rows
// for every auto-mintable grant (requires_approval=false creates no approval row)
// and for the 2nd..Nth mint of a leased git_pat, while THREAT-MODEL.md publishes
// step 4 of the kill cascade as "every minted credential for the run" (F096/F122).
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
