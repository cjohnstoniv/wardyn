// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// revokeRetiringToken retires the OLD run token's own jti once its proxy is
// confirmed gone or replaced (O2, least-privilege credentials, owner law
// 2026-09-24): a live old token must not go on answering /internal/* calls
// until its TTL lapses on its own. Best-effort and silent on a miss: an
// already-expired retiring token fails Verify on expiry before revocation is
// even reached, so there is nothing to revoke, and a provider without
// single-jti revocation is not held back.
//
// CALLER MUST wait until ReplaceProxy has returned before calling this (F4,
// Fable review): calling it any earlier — before the claim, before
// EnsureProxyImage, before ReplaceProxy — can revoke a LIVE run's still-
// serving old proxy's only token when the replace never even starts (a
// refused claim, an image pull failure). That proxy would then be unable to
// renew or answer a decision while its allowlisted egress keeps flowing,
// audit-dark, until the lapsed-token sweep's ~1h05m window loses it. The one
// call site that leaves an untouched live proxy running (reviveRunProxy's
// "old proxy was never touched" arm) MUST NOT call this either, for the same
// reason.
func (s *Server) revokeRetiringToken(ctx context.Context, retiring string, runID uuid.UUID) {
	claims, verr := s.cfg.Identity.Verify(ctx, retiring, internalAudience)
	if verr != nil {
		return
	}
	jr, ok := s.cfg.Identity.(jtiRevoker)
	if !ok {
		return
	}
	if err := jr.RevokeJTI(ctx, claims.JTI, runID); err != nil {
		slog.WarnContext(ctx, "wardynd: revoking a revived run's retiring token failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// renewedTokens is the ids the run's last token renewal recorded: the minted token, and the one it
// presented, which the proxy keeps when the lease send behind a renewal fails.
type renewedTokens struct{ jti, prev string }

func (t renewedTokens) ids() []string { return []string{t.jti, t.prev} }

// retireRunTokenLineage reads the run's renewed token ids and cuts the old proxy's lineage off in one
// statement (store.RunLoser.RetireRunTokenLineage). A failed cut revokes nothing later and leaves
// renewals as they were.
func (s *Server) retireRunTokenLineage(ctx context.Context, runID uuid.UUID, fresh string) renewedTokens {
	loser, ok := s.cfg.Store.(store.RunLoser)
	if !ok {
		return renewedTokens{}
	}
	jti, prev, err := loser.RetireRunTokenLineage(ctx, runID, fresh)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: retiring a revived run's renewed token lineage failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return renewedTokens{}
	}
	return renewedTokens{jti: jti, prev: prev}
}

// restoreRunTokenLineage hands a kept, untouched proxy back the lineage retireRunTokenLineage cut, so
// its next renewal goes through. A renewal it tried in between was refused as retryable.
func (s *Server) restoreRunTokenLineage(ctx context.Context, runID uuid.UUID, fresh string, old renewedTokens) {
	loser, ok := s.cfg.Store.(store.RunLoser)
	if !ok {
		return
	}
	if err := loser.RestoreRunTokenLineage(ctx, runID, fresh, old.jti, old.prev); err != nil {
		slog.WarnContext(ctx, "wardynd: restoring a kept proxy's token lineage failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// revokeRenewedTokens revokes the tokens the replaced proxy renewed since it started: the config the revive
// reads carries the token the proxy began with, but each renewal minted a newer one that would otherwise
// stay valid until its own expiry. jtis are the ids read before the swap (retireRunTokenLineage), except keep, the
// fresh token's. Same caller rule as revokeRetiringToken: only once the old proxy is, or may be, gone.
func (s *Server) revokeRenewedTokens(ctx context.Context, runID uuid.UUID, jtis []string, keep string) {
	jr, ok := s.cfg.Identity.(jtiRevoker)
	if !ok {
		return
	}
	for _, jti := range jtis {
		if jti == "" || jti == keep {
			continue
		}
		if err := jr.RevokeJTI(ctx, jti, runID); err != nil {
			slog.WarnContext(ctx, "wardynd: revoking a revived run's renewed token failed",
				slog.String("run_id", runID.String()), slog.Any("err", err))
		}
	}
}
