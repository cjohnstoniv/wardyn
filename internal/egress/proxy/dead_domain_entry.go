// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// deadDomainEntryWarning is kept as a const because a test asserts on it verbatim.
const deadDomainEntryWarning = "wardyn-proxy: policy entry can never match any request and is DEAD"

// warnDeadDomainEntries runs ValidDomainEntry over the run's two domain lists at
// sidecar boot and names every entry that can never match.
//
// CompilePolicy never re-checks ValidDomainEntry, so a policy written before the
// charset refusal landed compiles silently — on the DENY side that is a fail-OPEN,
// since a non-ASCII entry compiles to a host spelling no request can carry.
//
// Warn, never fail: refusing the run would take every sandbox down on upgrade
// day over an entry already inert, a worse outcome than the deny it reports.
func warnDeadDomainEntries(ctx context.Context, spec types.RunPolicySpec) {
	for _, l := range []struct {
		field   string
		entries []string
	}{
		{"allowed_domains", spec.AllowedDomains},
		{"denied_domains", spec.DeniedDomains},
	} {
		for _, d := range l.entries {
			if err := ValidDomainEntry(d); err != nil {
				slog.WarnContext(ctx, deadDomainEntryWarning,
					slog.String("field", l.field), slog.String("entry", d), slog.Any("why", err))
			}
		}
	}
}
