// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// discardMinted revokes a credential that was minted and then not returned to
// its issuer. Only github_token has an issuer-side revoke; git_pat, ssh_key and
// api_key are no-ops by design, not omissions. Best effort: failures are logged,
// never returned, so they don't mask the caller's own error.
func (b *Broker) discardMinted(ctx context.Context, m Minted) {
	if b.github == nil || m.Kind != types.GrantGitHubToken || m.Token == "" {
		return
	}
	if err := b.github.Revoke(ctx, m.Token); err != nil {
		slog.WarnContext(ctx, "broker: discarded github token could not be revoked; it stays live until GitHub expires it",
			"grant_id", m.GrantID, "jti", m.JTI, "err", err)
	}
}
