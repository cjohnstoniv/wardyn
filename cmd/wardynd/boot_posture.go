// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	_ "github.com/cjohnstoniv/wardyn/internal/secretstore/pg" // register "pg" secret store
	"strings"
)

// Boot-time posture validators — refusals and warnings wardynd raises before
// it serves anything. Carved out of main.go by seam (file-size gate), not by
// behaviour: every function here is byte-identical to its previous home.

// validateMemberModePosture enforces WARDYN_MEMBER_MODE's two preconditions.
//
// Member mode is an ASSERTION about topology, not a new authorization tier: it
// claims the human driving this daemon is a member and the operator authority
// lives elsewhere (an org IdP, MDM-managed config). The role split in
// http.go/routes.go already does all the enforcing; what member mode adds is a
// boot-time check that the assertion is actually TRUE — because if it is not,
// every clamp the member path relies on is reachable by the person at the
// keyboard, silently.
//
//  1. LocalMode must be OFF. humanOrAdminAuth branches on LocalMode FIRST and
//     bypasses public-API auth entirely, which makes the loopback developer an
//     admin (isOperator returns true with no session to demote) — the exact
//     opposite of what member mode asserts.
//  2. OIDC must be configured. Without an issuer there is no identity to derive
//     a role from, so isOperator returns true for every caller (no session role
//     to demote) and "member mode" would describe nobody.
//
// O2 (owner decision, 2026-08-23): real OIDC only. There is deliberately no
// "MDM asserts the identity" variant profile — a second identity path would be
// a second place a role can be forged, for a deployment shape 0.6 does not have.
func validateMemberModePosture(memberMode, localMode, oidcConfigured bool) error {
	if !memberMode {
		return nil
	}
	if localMode {
		return errors.New("refusing to start: WARDYN_MEMBER_MODE is set but local mode is active — " +
			"local mode bypasses public-API auth entirely and makes the loopback developer an ADMIN, " +
			"which is precisely what member mode asserts is impossible; unset WARDYN_LOCAL_MODE " +
			"(or WARDYN_MEMBER_MODE if this really is a single-developer machine that owns its own policy)")
	}
	if !oidcConfigured {
		return errors.New("refusing to start: WARDYN_MEMBER_MODE is set but no OIDC issuer is configured — " +
			"with no signed-in identity there is no role to derive, so every caller is an admin; " +
			"configure WARDYN_OIDC_ISSUER (plus WARDYN_OIDC_ROLE_MAP or WARDYN_OIDC_OPERATOR_EMAILS " +
			"so the developer derives the member role) or unset WARDYN_MEMBER_MODE")
	}
	return nil
}

// validateOperatorPosture is the second boot-time fail-closed rule, kept beside
// validateConfig (and pure, for the same reason) but applied later: OIDC is not
// built until boot_deps.go, well after validateConfig runs at the top of run().
//
// Configuring SSO IS the declaration that more than one human exists, so an
// empty operator allowlist is not a default — it is an ambiguity in which every
// person the IdP lets in silently holds the admin token's power. Refuse, with
// WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST as the explicit override (the
// WARDYN_ALLOW_PLAINTEXT_LISTEN precedent). UNCONDITIONAL — not conditioned on
// the bind address the way the plaintext rule is: a loopback bind bounds who can
// reach the port, not who the IdP authenticates.
//
// No OIDC => nothing to decide: the admin token and local mode are a single
// shared credential with no human identity to key a role off, so they are always
// operators and this rule never fires.
//
// hasRoleMap also satisfies the rule: WARDYN_OIDC_ROLE_MAP switches deriveRole
// (internal/auth/oidc) to claim-based admin/member derivation that no longer
// depends on the operator allowlist at all (an unmatched claim falls through to
// WARDYN_OIDC_DEFAULT_ROLE or is denied) — so a role-map-only deployment, the
// recipe .claude/skills/wardyn-k8s-setup/SKILL.md documents, is not the
// every-human-is-admin ambiguity this refusal exists to catch.
func validateOperatorPosture(oidcConfigured bool, operatorEmails []string, allowNoOperatorList bool, hasRoleMap bool) error {
	if !oidcConfigured || len(operatorEmails) > 0 || allowNoOperatorList || hasRoleMap {
		return nil
	}
	return errors.New("refusing to start: OIDC SSO is configured but the operator allowlist is empty — " +
		"EVERY human the IdP signs in would be admin-equivalent (rewrite policies/workspaces/site-config, connect the shared harness credential, " +
		"write and delete secrets, decide approvals, and open an interactive shell in any running sandbox) — absent a role map — " +
		"set WARDYN_OIDC_OPERATOR_EMAILS to the humans who may do that — everyone else becomes a member who reads their OWN runs and can launch runs — " +
		"or set WARDYN_OIDC_ROLE_MAP for claim-based roles instead, " +
		"or explicitly set WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST=true to override")
}

// validateUISandboxConfig is the UI-sandbox gateway's boot-time fail-closed
// rule, kept beside validateConfig and pure for the same reason.
//
// The refusal that matters is the SAME-ADDRESS one. Everything the gateway
// relays is the sandbox's OWN code, and the only thing keeping that code away
// from the console's session storage and admin actions is that it arrives on a
// different browser origin. Bound to the console's address, the gateway would
// not merely fail to listen twice — the feature's entire security argument
// would be false. So it is refused loudly here, in terms of what breaks, rather
// than surfacing as "address already in use". The SSH gateway's address is
// checked too: a shared port there is a plain misconfiguration, but it is one
// boot can name instead of leaving to a bind error.
//
// The origin template, when set, must carry {run} — a template without it would
// hand EVERY run the same host, silently turning per-run isolation back into
// the shared origin it exists to replace.
//
// The TLS posture is taken rather than re-derived so this listener answers to
// the SAME plaintext refusal the console does (refusePlaintextListen): the
// relay session cookie is a bearer credential, and it travels on this address.
func validateUISandboxConfig(uiListen, listen, sshListen, originTemplate string, posture tlsPosture, allowPlaintextListen bool) error {
	if uiListen == "" {
		return nil // off: nothing to validate, no listener, no new surface
	}
	if err := refusePlaintextListen("-ui-sandbox-listen", uiListen, posture, allowPlaintextListen); err != nil {
		return err
	}
	if sameListenAddress(uiListen, listen) {
		return fmt.Errorf("refusing to start: -ui-sandbox-listen %q is the same address as -listen — "+
			"the UI-sandbox gateway relays the SANDBOX's own pages, and serving them on the console's origin would let that "+
			"sandbox-authored code read the console session and drive every admin action the operator can; "+
			"give the gateway its own address (e.g. \":8081\") or unset WARDYN_UI_SANDBOX_LISTEN to disable it", uiListen)
	}
	if sshListen != "" && sameListenAddress(uiListen, sshListen) {
		return fmt.Errorf("refusing to start: -ui-sandbox-listen %q is the same address as -ssh-listen; give each gateway its own address", uiListen)
	}
	if originTemplate != "" && !strings.Contains(originTemplate, "{run}") {
		return fmt.Errorf("refusing to start: WARDYN_UI_SANDBOX_ORIGIN_TEMPLATE %q has no {run} placeholder — "+
			"every run would share one origin while the deployment claims per-run isolation; "+
			"use e.g. \"https://run-{run}.ui.example.com\", or unset it for the documented shared-origin mode", originTemplate)
	}
	return nil
}
