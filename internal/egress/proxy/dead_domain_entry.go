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
// CompilePolicy never re-checks ValidDomainEntry, so a policy written before a
// refusal landed compiles silently.
//
// Warn, never fail: refusing the run would take every sandbox down on upgrade
// day over an entry already inert, a worse outcome than the deny it reports.
// An entry in a spelling step 0 refuses in every request (a character outside
// ASCII, an address in a non-standard spelling) is the exception, refused before
// this runs (refuseIPSpellingEntries, at config load): it reads as a deny of the
// name or address and binds nothing.
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
