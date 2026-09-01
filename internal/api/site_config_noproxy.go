// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// Write-time validation for SiteConfig.UpstreamProxyNoProxy — the corporate
// upstream proxy's BYPASS list. The live disclosure is the sidecar's own
// construction log (NewServer, internal/egress/proxy/server.go), which names
// the compiled entries per run beside the upstream-chaining line.
//
// Why this is worth validating at the trust boundary rather than leaving to the
// sidecar: the proxy DROPS an entry it cannot compile (never widens the list),
// so a typo'd suffix means "still proxied" — silently, at dispatch time, in the
// one configuration whose entire purpose is to stop a destination being
// proxied. That is the exact "each attempt failed at a different layer" shape
// the private-endpoint work exists to end, so a bad entry has to be a 400 at
// the write, not a shrug at the run.

// validateUpstreamProxyNoProxy enforces SiteConfig.UpstreamProxyNoProxy's
// write-time invariant: every entry is either a CIDR or a host/domain suffix
// the proxy will actually honour, decided by the PROXY'S OWN rule
// (proxy.ValidNoProxyEntry) rather than a second copy of it here — a dual
// matcher over one operator-authored list is the drift bug this codebase
// already warns about, and here it would drift in the direction of a bypass
// that silently is not one.
//
// The NO_PROXY "*" wildcard is refused by that rule, deliberately: "bypass
// everything" is spelled by clearing upstream_proxy_url, and one character
// must not be able to un-chain a whole estate's egress.
//
// Callers: validateSiteConfig (PUT /site-config), beside validateInternalHosts.
func validateUpstreamProxyNoProxy(entries []string) error {
	for i, e := range entries {
		if !proxy.ValidNoProxyEntry(e) {
			return fmt.Errorf("upstream_proxy_no_proxy[%d]: %q must be a CIDR (100.64.0.0/10) or a host/domain suffix "+
				"(vpce.amazonaws.com, .corp.internal) — wildcards are not accepted; clear upstream_proxy_url to bypass everything", i, e)
		}
	}
	return nil
}
