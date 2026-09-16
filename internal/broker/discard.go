// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// discardMinted hands a credential that was minted and then NOT returned back to
// its issuer (B11a-F1).
//
// Only github_token has an issuer-side revoke: the installation token is a live
// GitHub credential the App can surrender (Apps.RevokeInstallationToken, the
// call ruleset.go already makes for its probe token). The other kinds have
// nothing to hand back — git_pat returns the operator's OWN long-lived PAT
// (revoking it would destroy the stored credential every future run depends on),
// ssh_key is a keypair this process generated and never registered anywhere, and
// api_key never leaves the broker at all — so they are a no-op, not an omission.
//
// Best effort, and deliberately silent about the outcome to the caller: every
// call site is already returning an error, and turning a failed hand-back into a
// different error would replace the diagnosis the operator actually needs. It is
// logged instead, without the token.
func (b *Broker) discardMinted(ctx context.Context, m Minted) {
	if b.github == nil || m.Kind != types.GrantGitHubToken || m.Token == "" {
		return
	}
	if err := b.github.Revoke(ctx, m.Token); err != nil {
		slog.WarnContext(ctx, "broker: discarded github token could not be revoked; it stays live until GitHub expires it",
			"grant_id", m.GrantID, "jti", m.JTI, "err", err)
	}
}
