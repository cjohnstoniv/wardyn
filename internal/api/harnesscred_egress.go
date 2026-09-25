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
// Region-DERIVED, never wildcarded: the proxy's matcher (classifyDomain) knows
// only a LEADING "*." or an exact host, so "oidc.*.amazonaws.com" allows nothing,
// and "*.amazonaws.com" opens every AWS service — too wide for a login box.
// ssoRegion == "" pre-allows nothing regional: the CLI's hosts then surface as
// deny_with_review approvals the operator can grant from the login pane.
// endpointOverride is the TEST hatch (awssso_endpoint.go). It collapses ALL
// THREE regional entries, including device.sso.<r>, onto the one fake host,
// which serves the verification page too; missing that entry looks like a hung
// login (the CLI prints a verification URL the sandbox is denied).
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
