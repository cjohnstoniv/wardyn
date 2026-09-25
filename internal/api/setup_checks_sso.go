// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "strings"

// setup_checks_sso.go holds the two OIDC-role-derivation /setup/status rows —
// split out of setup_checks.go (#491) to keep it under the file-size gate.

// ssoRBACCheck warns when OIDC is configured and either of two conditions
// makes some signed-in human derive role "admin" by accident rather than by
// name (internal/auth/oidc's deriveRole; accessRolePosture's `before` reads
// the same arm):
//
//  1. Nothing splits admins from members at all: no role mapping — neither
//     the chart's WARDYN_OIDC_ROLE_MAP nor a console-managed row (migration
//     0051, the People step) — and no admin list (the operator allowlist,
//     WARDYN_OIDC_OPERATOR_EMAILS). Fine for a single-operator deployment, but
//     it silently grants admin to EVERY signed-in human the moment a second
//     one shows up. An admin list alone is ok (Q457-5): an unmatched person
//     then derives user. This arm takes priority over the second — a
//     deployment with neither a role map nor an admin list reads this row's
//     strings even if defaultRoleAdmin is also true (Q491-1: one banner, not
//     two competing ones).
//  2. A role map (or admin list) IS set, but WARDYN_OIDC_DEFAULT_ROLE=admin
//     (defaultRoleAdmin) — so every sign-in the map doesn't match still
//     falls through to admin (deriveRole's arm 3). Before #491 this read
//     "ok": a role map being set was the only thing this check looked for,
//     so a deployment could have named rows, a default role of admin, and no
//     warning anywhere. Cause "default_role" tells the console which of the
//     two sentences to show (everyone-admin-banner.tsx); Q457-6's canon
//     already treats Blocking as this row's own concern, not the banner's,
//     so this arm blocks the console the same as arm 1.
//
// consoleRows is whether the store currently holds at least one People-step
// row (the same nil-Store guard setup.go's own read of it applies —
// unreadable/absent reads as false, the conservative direction: it surfaces
// the warning rather than hiding it). Only surfaced when OIDC is configured
// (mirrors bedrockProviderCheck's own "worth showing at all" gate).
//
// Wording per docs/design/admin-access-canon.md (frozen; the console's
// everyone-is-an-admin banner carries the same warn sentence per cause).
func ssoRBACCheck(oidcConfigured, roleMapConfigured, consoleRows, adminList, defaultRoleAdmin bool) (SetupCheck, bool) {
	if !oidcConfigured {
		return SetupCheck{}, false
	}
	detail, fix, cause, warn := ssoRBACWarnReason(roleMapConfigured, consoleRows, adminList, defaultRoleAdmin)
	if !warn {
		return SetupCheck{
			ID: "sso_rbac", Label: "Who is an admin", Status: "ok",
			Detail: "People are mapped to admin or user, so a person's role comes from their sign-in.",
		}, true
	}
	// The one Blocking: true site for this row (setup_blocking_guard_test.go
	// parses the AST per function, not per branch) — shared by both warn causes.
	return SetupCheck{
		ID: "sso_rbac", Label: "Who is an admin", Status: "warn",
		Detail: detail, Fix: fix, Cause: cause,
		Blocking: true,
	}, true
}

// ssoRBACWarnReason picks which of the two sso_rbac warn causes applies, or
// reports no warning at all. Arm 1 (neither role map, console rows, nor admin
// list) takes priority over arm 2 (defaultRoleAdmin) per Q491-1: a deployment
// with neither set reads #484's original sentence even if the default role is
// also admin, so the packet shows one banner, never two competing ones.
func ssoRBACWarnReason(roleMapConfigured, consoleRows, adminList, defaultRoleAdmin bool) (detail, fix, cause string, warn bool) {
	switch {
	case !roleMapConfigured && !consoleRows && !adminList:
		return "Nobody is mapped to a role and no admin list is set, so everyone who signs in is an admin.",
			"Map people to admin or user on the People step, so only the people you name can change this deployment.",
			"", true
	case defaultRoleAdmin:
		return "A role map is set, but the default role is admin, so a sign-in the map doesn't match is still an admin.",
			"Set WARDYN_OIDC_DEFAULT_ROLE to user or a user type (chart: env.WARDYN_OIDC_DEFAULT_ROLE), so a sign-in the role map doesn't match becomes a user, not an admin.",
			"default_role", true
	default:
		return "", "", "", false
	}
}

// tlsCookiePostureCheck warns when the OIDC redirect URL is https — evidence
// that TLS terminates somewhere in front of this deployment — but wardynd
// still computed secureCookies=false (validateConfig, cmd/wardynd/main.go: the
// exact condition this inverts is tlsEnabled||WARDYN_TLS_TERMINATED), so the
// session cookie is issued without the Secure attribute: the classic
// behind-an-ingress misconfiguration where WARDYN_TLS_TERMINATED was never
// set. Only surfaced when OIDC is configured AND the redirect URL is https —
// there is nothing to warn about otherwise.
func tlsCookiePostureCheck(oidcConfigured bool, redirectURL string, secureCookies bool) (SetupCheck, bool) {
	if !oidcConfigured || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(redirectURL)), "https://") {
		return SetupCheck{}, false
	}
	if secureCookies {
		return SetupCheck{
			ID: "tls_cookie_posture", Label: "TLS/cookie posture", Status: "ok",
			Detail: "The OIDC redirect URL is https and wardynd knows the connection is TLS-protected; session cookies are marked Secure.",
		}, true
	}
	return SetupCheck{
		ID: "tls_cookie_posture", Label: "TLS/cookie posture", Status: "warn",
		Detail: "The OIDC redirect URL is https but WARDYN_TLS_TERMINATED is not set, so wardynd still thinks it is serving plain HTTP: the session cookie is issued WITHOUT the Secure attribute.",
		Fix:    "Set WARDYN_TLS_TERMINATED=true (helm: env.WARDYN_TLS_TERMINATED) when TLS terminates at an upstream reverse proxy/ingress.",
	}, true
}
