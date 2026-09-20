// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// deadDomainEntryWarning is the one sentence this pass exists to say. Kept as a
// constant because the test asserts THROUGH it: a warning nobody can find is
// the same as no warning.
const deadDomainEntryWarning = "wardyn-proxy: policy entry can never match any request and is DEAD"

// warnDeadDomainEntries runs ValidDomainEntry over the run's two domain lists at
// sidecar boot and names every entry that can never match.
//
// ValidDomainEntry guards the API WRITE doors, so it refuses a malformed entry
// on the way IN and has nothing to say about one that is already stored.
// CompilePolicy — which every run's sidecar calls on the policy it was
// dispatched — never asked. A policy written before the charset refusal landed
// therefore compiles silently, and on the DENY side that is a fail-OPEN: a
// non-ASCII `denied_domains` entry compiles to a host spelling no request can
// carry (a request arrives punycode-encoded), so the deny denies nothing and
// nothing anywhere says so.
//
// Warn, never fail. Refusing the run would take every sandbox on an estate down
// on upgrade day over an entry that has been inert since it was written — a
// strictly worse outcome than the deny it is reporting. The compiled policy is
// byte-identical either way; this adds the signal, not a refusal.
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
