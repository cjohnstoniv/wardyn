// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/ipguard"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validateInternalHosts enforces SiteConfig.InternalHosts's write-time
// invariant: every declared CIDR must lie ENTIRELY inside ipguard.Liftable
// (RFC1918, fc00::/7, or 100.64.0.0/10) — never loopback, link-local, metadata,
// multicast, or any other reserved range, which the proxy's blockKind
// classification keeps un-liftable regardless of what an operator declares
// here. A bare host_suffix must be a real host (validSiteHost); an entry with
// no CIDRs is valid (it lifts the full Liftable set for that host).
func validateInternalHosts(hosts []types.InternalHost) error {
	for i, h := range hosts {
		if !validSiteHost(h.HostSuffix) {
			return fmt.Errorf("internal_hosts[%d].host_suffix: invalid host %q", i, h.HostSuffix)
		}
		if _, err := netip.ParseAddr(strings.ToLower(strings.TrimSpace(h.HostSuffix))); h.Baseline && err == nil {
			return fmt.Errorf("internal_hosts[%d].baseline: %q is an IP address; baseline names a hostname", i, h.HostSuffix)
		}
		for j, c := range h.CIDRs {
			prefix, err := netip.ParsePrefix(c)
			liftable := err == nil && slices.ContainsFunc(ipguard.Liftable, func(l netip.Prefix) bool {
				return l.Bits() <= prefix.Bits() && l.Contains(prefix.Addr())
			})
			if !liftable {
				return fmt.Errorf("internal_hosts[%d].cidrs[%d]: %q must lie inside RFC1918, fc00::/7 or 100.64.0.0/10", i, j, c)
			}
		}
	}
	return nil
}

// logWarnInternalHostsDeclared is the loud, unmissable log an internal-host
// declaration earns: it is the ONLY operator override of the proxy's
// unconditional private/reserved-IP SSRF guard, so the deployment's log must
// name exactly which suffixes and ranges are lifted — an operator reading it
// back later cannot be left to infer the guard's shape from a count. Mirrors
// logWarnUnenforcedNetPolOptOut's contract (internal/runner/k8s/driver.go): the
// declaration itself, then what it does and does not lift.
//
// A warn, not an acknowledgement flag: the write is operator-only,
// audited and Liftable-validated, and it grants no policy allow — the host must
// still pass allowed_domains separately. No declarations => silent.
func logWarnInternalHostsDeclared(hosts []types.InternalHost) {
	if len(hosts) == 0 {
		return
	}
	slog.Warn("wardynd: "+internalHostsDeclaredSentence(hosts), slog.Int("internal_hosts_count", len(hosts)))
}

// internalHostsDeclaredSentence is the ONE sentence naming what an
// InternalHosts declaration does and does not lift — shared by the write-time
// deployment log above and the console's own dedicated internal_hosts check
// row (internalHostsCheck, setup_checks.go), so an operator reads the
// identical claim on whichever surface they are looking at. Callers check
// len(hosts) > 0 themselves; this renders unconditionally.
func internalHostsDeclaredSentence(hosts []types.InternalHost) string {
	decls := make([]string, 0, len(hosts))
	for _, h := range hosts {
		scope := "the full RFC1918/ULA/CGNAT set"
		if len(h.CIDRs) > 0 {
			scope = strings.Join(h.CIDRs, ", ")
		}
		decls = append(decls, h.HostSuffix+" => "+scope)
	}
	return "site config declares INTERNAL HOSTS — the proxy's private/reserved-IP SSRF guard is LIFTED for these host suffixes, " +
		"scoped to the ranges named: " + strings.Join(decls, "; ") + ". Loopback, link-local, the cloud-metadata address, unspecified, multicast and " +
		"NAT64-embedded addresses stay denied regardless of what is declared here, and a policy's allowed_domains must still allow the host separately — " +
		"this lifts the built-in guard only. Remove the entry to restore the unconditional deny."
}

// auditInternalHostSuffixes renders saved.InternalHosts as sorted host
// suffixes for site_config.write's datum — a suffix is exactly what
// logWarnInternalHostsDeclared already puts in the deployment's own log, so
// this adds nothing an operator couldn't already read there, just makes it
// reviewable from the audit trail too. CIDRs are left out: the scoping detail
// belongs to the log line above, not a row every SIEM sink fans out to.
func auditInternalHostSuffixes(hosts []types.InternalHost) []string {
	suffixes := make([]string, 0, len(hosts))
	for _, h := range hosts {
		suffixes = append(suffixes, h.HostSuffix)
	}
	slices.Sort(suffixes)
	return suffixes
}

// auditBaselineInternalHosts is the suffixes marked baseline, sorted: they LOWER the egress grade,
// so a change must be reviewable from the row.
func auditBaselineInternalHosts(hosts []types.InternalHost) []string {
	suffixes := []string{}
	for _, h := range hosts {
		if h.Baseline {
			suffixes = append(suffixes, h.HostSuffix)
		}
	}
	slices.Sort(suffixes)
	return suffixes
}

// proxyInternalHosts is the declaration as the sidecar reads it: the lift only. Baseline is a grading
// fact the proxy never reads, and a sidecar image older than wardynd refuses a key it does not know.
func proxyInternalHosts(hosts []types.InternalHost) []types.InternalHost {
	out := slices.Clone(hosts)
	for i := range out {
		out[i].Baseline = false
	}
	return out
}
