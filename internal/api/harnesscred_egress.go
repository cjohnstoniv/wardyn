// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "fmt"

// The login sandbox's EGRESS allowlist, in its own file because harnesscred.go
// is at the 1000-line cap (scripts/check-file-size.sh) and this is the seam
// that grew: loginEgress now follows the gated AWS SSO endpoint override
// (awssso_endpoint.go) as well as deriving the regional AWS hosts.

// loginEgress is the allowlist the login sandbox actually runs under: the row's
// region-free hosts plus, for a region-scoped flow, the AWS SSO endpoints for
// ssoRegion.
//
// Region-DERIVED, never wildcarded, because the proxy's only matcher
// (classifyDomain, internal/egress/proxy/policy.go) understands exactly two
// forms: a LEADING "*." suffix match, or an exact host. A mid-label pattern
// like "oidc.*.amazonaws.com" is neither — it compiles to an exact hostname no
// real request can ever equal, so it allows nothing (the shipped bug this
// replaces: the login was denied on the very hosts it "pre-allowed"). The one
// supported form that would cover every region is "*.amazonaws.com", which
// opens every AWS service (S3, EC2, …) to the sandbox — far too wide for a
// login box, so the region is resolved instead of widened.
//
// ssoRegion == "" (Bedrock region not configured yet) pre-allows nothing
// regional: the two hosts the CLI dials then surface as deny_with_review
// approvals the operator can grant from the login pane — recoverable and
// honest, unlike the silent dead entries it replaces.
// endpointOverride is the TEST hatch (awssso_endpoint.go). It collapses ALL
// THREE regional entries — including device.sso.<r>, which no other caller adds
// — onto the one fake host, because the fake serves the verification page from
// the same server as the two services. Missing that third entry is the failure
// that looks like the login hanging: the CLI prints a verification URL the
// sandbox is then denied.
func (hl harnessLogin) loginEgress(ssoRegion, endpointOverride string) []string {
	hosts := append([]string(nil), hl.egress...)
	if hl.regionalSSOEgress && ssoRegion != "" {
		// oidc.<r> + portal.sso.<r> — the same pair a Bedrock run needs.
		hosts = append(hosts, ssoEgressHosts(ssoRegion, endpointOverride)...)
		// … plus the device-authorization verification page, which only the
		// interactive login flow displays. Under the override the line above
		// already added the one host that serves it.
		if endpointOverride == "" {
			hosts = append(hosts, fmt.Sprintf("device.sso.%s.amazonaws.com", ssoRegion))
		}
	}
	return hosts
}
