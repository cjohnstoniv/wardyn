// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// testDNSAudit asserts the agent's resolver is its run proxy: the sandbox's lookups reach the proxy's DNS
// listener, which records each and answers none, instead of a resolver of the substrate's own. The
// substrate's probe runs in a sandbox at the strongest class, so the nightly gVisor leg proves the resolver
// file under runsc. Skipped when the driver declares no confinement classes or injects no probe.
func testDNSAudit(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skipf("driver %q declares no confinement classes; the resolver is not testable without a sandbox", r.Name())
	}
	if opts.DNSProbe == nil {
		t.Skipf("no DNSProbe supplied for driver %q; DNSAudit case skipped", r.Name())
	}
	sb := createStrongestSandbox(t, ctx, r, caps, opts, "DNS audit case")
	opts.DNSProbe(t, sb.Ref)
}
