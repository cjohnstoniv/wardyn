// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
		return "github installation tokens are handed back to GitHub (DELETE /installation/token, presented the sealed token) by a background worker after RevokeRun queues them, retried until the token expires; " +
			"a token with no sealed copy (minted before this version, for a run with no masking manifest, or its owner erased) expires on GitHub's own <=1h TTL; the identity denylist stops further mints"
	case types.GrantGitPAT, types.GrantSSHKey:
		return "operator must rotate this secret at the forge — wardyn cannot revoke, expire or down-scope it; the identity denylist only stops further mints"
	case types.GrantAPIKey:
		return "api_key values stay proxy-side and are never resident in the sandbox; the identity denylist stops further mints and the proxy stops injecting"
	default:
		return "no per-credential revocation for this grant kind — relying on TTL expiry + identity denylist"
	}
}

// RevokeRun best-effort revokes credentials minted for a run, part of the
// kill-switch cascade. It (a) audits each minted jti as a revoke with the
// per-kind story (revokeNote), first, on the caller's ctx; (b) queues each
// github_token whose value the broker keeps sealed (TokenSeal) for the hand-back
// worker (RunGitHubTokenRevokes), which presents it to GitHub's
// DELETE /installation/token — that endpoint must be presented the token itself —
// and retries until the token expires; and (c) relies on identity.Provider.RevokeRun
// to deny further mints for the run. RevokeRun itself makes no GitHub call and
// opens no seal, so the kill never waits for GitHub. A github_token row carries
// target_revoke: queued, expired, handed_back, abandoned, or none (no sealed copy).
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
	var states map[string]string
	if b.tokenSeal != nil && slices.ContainsFunc(minted, func(mc MintedCredential) bool { return types.GrantKind(mc.Kind) == types.GrantGitHubToken }) {
		var serr error
		if states, serr = b.githubRevokeStates(ctx, runID, nil); serr != nil {
			return fmt.Errorf("broker: read sealed github tokens: %w", serr)
		}
	}
	actor := spiffeForRun(b.trustDomain, runID)
	// due: a jti is queued, or expired while still held (queue ends it as expired, so a rerun reads what happened).
	due := false
	for _, mc := range minted {
		d := map[string]any{
			"jti":   mc.JTI,
			"note":  revokeNote(mc.Kind),
			"scope": "run_credentials",
		}
		if mc.Kind != "" {
			d["kind"] = mc.Kind
		}
		if types.GrantKind(mc.Kind) == types.GrantGitHubToken {
			st, ok := states[mc.JTI]
			if !ok {
				st = targetRevokeNone
			}
			d["target_revoke"] = st
			due = due || st == targetRevokeQueued || st == sealExpired
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
	if due {
		if qerr := b.queueGitHubRevokes(ctx, runID, nil); qerr != nil {
			return fmt.Errorf("broker: queue github token revokes: %w", qerr)
		}
	}
	return nil
}
