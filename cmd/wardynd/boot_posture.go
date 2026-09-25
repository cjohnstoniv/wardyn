// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/directory"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	_ "github.com/cjohnstoniv/wardyn/internal/secretstore/pg" // register "pg" secret store
)

// Boot-time posture validators — refusals and warnings wardynd raises before
// it serves anything. Carved out of main.go by seam (file-size gate), not by
// behaviour: every function here is byte-identical to its previous home.

// validateMemberModePosture enforces WARDYN_USER_DESKTOP's two preconditions.
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
		return errors.New("refusing to start: WARDYN_USER_DESKTOP is set but local mode is active — " +
			"local mode bypasses public-API auth entirely and makes the loopback developer an ADMIN, " +
			"which is precisely what member mode asserts is impossible; unset WARDYN_LOCAL_MODE " +
			"(or WARDYN_USER_DESKTOP if this really is a single-developer machine that owns its own policy)")
	}
	if !oidcConfigured {
		return errors.New("refusing to start: WARDYN_USER_DESKTOP is set but no OIDC issuer is configured — " +
			"with no signed-in identity there is no role to derive, so every caller is an admin; " +
			"configure WARDYN_OIDC_ISSUER (plus WARDYN_OIDC_ROLE_MAP or WARDYN_OIDC_OPERATOR_EMAILS " +
			"so the developer derives the member role) or unset WARDYN_USER_DESKTOP")
	}
	return nil
}

// validateHybridPosture enforces the org control-plane settings' preconditions
// (issue #100, docs/design/0.8/PLAN.md): WARDYN_ORG_URL and
// WARDYN_ORG_ENROLMENT_TOKEN. Kept as its OWN function rather than folded into
// validateMemberModePosture above: that one owns the local-mode/OIDC
// preconditions, and duplicating them here is exactly how the two would drift
// apart — this one owns hybrid's preconditions instead, and calls neither.
//
// nil when orgURL is empty: no hybrid posture is asserted, nothing to check.
// Otherwise, in order:
//
//  1. memberMode must be on. An org URL with no member-mode assertion is a
//     laptop that claims to report to an org control plane while still
//     behaving as its own admin — the incoherence member mode exists to catch,
//     one level up.
//  2. orgURL must be https://, unless its host is loopback — the same rule the
//     webhook audit sink applies to a bearer token over plaintext
//     (sinks.NewWebhookSink refuses a non-https bearer_token URL): the
//     enrolment token is exactly that kind of long-lived, replayable
//     credential, dispatched with a request to orgURL, and a plaintext URL
//     would send it in cleartext to any peer on the path. allowPlaintextListen
//     (WARDYN_ALLOW_PLAINTEXT_LISTEN) is the SAME override refusePlaintextListen
//     already uses for wardynd's own listen address — one escape hatch, not a
//     second one to keep in sync.
//
// enrolToken set with orgURL empty is refused by the nil check above, so by
// the time clause 2 runs orgURL is known non-empty — a token naming nowhere to
// go is a misconfiguration, not a no-op.
func validateHybridPosture(orgURL, enrolToken string, memberMode, allowPlaintextListen bool) error {
	if orgURL == "" {
		if enrolToken != "" {
			return errors.New("refusing to start: WARDYN_ORG_ENROLMENT_TOKEN is set but WARDYN_ORG_URL is not — " +
				"an enrolment token has nowhere to go without an org control plane to enrol against; " +
				"set WARDYN_ORG_URL or unset WARDYN_ORG_ENROLMENT_TOKEN")
		}
		return nil
	}
	if !memberMode {
		return errors.New("refusing to start: WARDYN_ORG_URL is set but WARDYN_USER_DESKTOP is not — " +
			"a daemon pointed at an org control plane with no member-mode assertion still treats the human at the " +
			"keyboard as its own admin, which is precisely the incoherence member mode exists to refuse; " +
			"set WARDYN_USER_DESKTOP=true or unset WARDYN_ORG_URL")
	}
	u, err := url.Parse(orgURL)
	if err != nil {
		return fmt.Errorf("refusing to start: WARDYN_ORG_URL %q does not parse as a URL", orgURL)
	}
	if !strings.EqualFold(u.Scheme, "https") && !allowPlaintextListen && !listenIsLoopback(u.Hostname()) {
		return fmt.Errorf("refusing to start: WARDYN_ORG_URL %q is not https:// and its host is not loopback — "+
			"the device credential travels with every request this daemon makes to it, and a plaintext non-loopback URL "+
			"sends that credential in cleartext to any peer on the path; use https://, point WARDYN_ORG_URL at a "+
			"loopback host for local testing, or set WARDYN_ALLOW_PLAINTEXT_LISTEN=true to override", orgURL)
	}
	return nil
}

// checkMemberAndHybridBootPosture runs validateMemberModePosture then
// validateHybridPosture in sequence, folded into ONE function call so run()
// (cmd/wardynd/main.go) gains no extra `if err != nil` branch for the second,
// adjacent refusal — gocyclo's function-complexity gate is already at its
// ceiling there. The two validators stay separate on purpose (see
// validateHybridPosture's own doc comment on why it does not extend
// validateMemberModePosture); this is only the call site folded together.
func checkMemberAndHybridBootPosture(memberMode, localMode, oidcConfigured bool, orgURL, enrolToken string, allowPlaintextListen bool) error {
	if err := validateMemberModePosture(memberMode, localMode, oidcConfigured); err != nil {
		return err
	}
	return validateHybridPosture(orgURL, enrolToken, memberMode, allowPlaintextListen)
}

// validateSSOOnlyPosture enforces WARDYN_SSO_ONLY's precondition: SSO must
// actually be the ONLY way in before the daemon may claim it is — and before
// /healthz's sso_only bit, which the sign-in screen reads to drop the
// admin-token form and the role-derivation caveat, may say so either.
//
// Five ways back into "not actually SSO-only", each refused by name:
//
//  1. No OIDC issuer configured — sso-only with no SSO at all would leave the
//     console with no usable sign-in whatsoever.
//  2. WARDYN_ADMIN_TOKEN set — a live shared bearer token is a second front
//     door the posture claims does not exist.
//  3. WARDYN_LOCAL_MODE set — local mode bypasses public-API auth entirely,
//     which is precisely what sso-only asserts is impossible.
//  4. WARDYN_USER_DESKTOP set — member mode's own precondition
//     (validateMemberModePosture above) already requires OIDC, but its
//     desktop profile (deploy/desktop/wardyn.env.m-prime.example) relies on
//     the admin token as a PROCESS credential the daemon authenticates
//     itself with, which sso-only forbids outright.
//  5. WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST set — the override that makes
//     "every signed-in human is an admin" reachable is exactly the ambiguity
//     sso-only exists to close off; refusing it is what lets the sign-in
//     screen safely drop SIGNIN.ROLE_SOURCE's caveat.
func validateSSOOnlyPosture(ssoOnly, oidcConfigured bool, adminToken string, localMode, memberMode, allowNoOperatorList bool) error {
	if !ssoOnly {
		return nil
	}
	if !oidcConfigured {
		return errors.New("refusing to start: WARDYN_SSO_ONLY is set but no OIDC issuer is configured — " +
			"sso-only asserts SSO is the only way into the console, and with no issuer there is no SSO at all; " +
			"configure WARDYN_OIDC_ISSUER (plus WARDYN_OIDC_OPERATOR_EMAILS or WARDYN_OIDC_ROLE_MAP) or unset WARDYN_SSO_ONLY")
	}
	if adminToken != "" {
		return errors.New("refusing to start: WARDYN_SSO_ONLY is set but WARDYN_ADMIN_TOKEN is also set — " +
			"a live admin bearer token is a second way into the console, which sso-only asserts does not exist; " +
			"unset WARDYN_ADMIN_TOKEN or unset WARDYN_SSO_ONLY")
	}
	if localMode {
		return errors.New("refusing to start: WARDYN_SSO_ONLY is set but so is WARDYN_LOCAL_MODE — " +
			"local mode bypasses public-API auth entirely, which is precisely what sso-only asserts is impossible; " +
			"unset WARDYN_LOCAL_MODE or unset WARDYN_SSO_ONLY")
	}
	if memberMode {
		return errors.New("refusing to start: WARDYN_SSO_ONLY is set but so is WARDYN_USER_DESKTOP — " +
			"member mode requires the admin token as a process credential (see deploy/desktop/wardyn.env.m-prime.example), " +
			"which sso-only forbids outright; unset WARDYN_USER_DESKTOP or unset WARDYN_SSO_ONLY")
	}
	if allowNoOperatorList {
		return errors.New("refusing to start: WARDYN_SSO_ONLY is set but so is WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST — " +
			"that override is what makes the \"every signed-in human is an admin\" role-derivation branch reachable, " +
			"which sso-only exists to forbid; unset WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST or unset WARDYN_SSO_ONLY")
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
		"set WARDYN_OIDC_OPERATOR_EMAILS to the humans who may do that — everyone else becomes a user who reads their OWN runs and can launch runs — " +
		"or set WARDYN_OIDC_ROLE_MAP for claim-based roles instead, " +
		"or explicitly set WARDYN_ALLOW_OIDC_NO_OPERATOR_LIST=true to override")
}

// directoryProviderEntra is the one connector §I/PF-29 ships. A value that is
// neither this nor empty is a REFUSAL, not a silent disable: a typo'd
// WARDYN_DIRECTORY_PROVIDER must not read as "feature off".
const directoryProviderEntra = "entra"

// resolveDirectoryConfig turns the four WARDYN_DIRECTORY_* vars plus the OIDC
// ones into the connector's config, or refuses boot. It is the third fail-closed
// posture rule in this file, and the reason it exists at all is that
// internal/directory reads NO environment by design: NewEntra can only answer
// ErrUnconfigured, which surfaces as a lazy 503 at the first keystroke — a
// misconfiguration discovered by an admin typing into a combobox, not by the
// operator who set the variable. A config that cannot work is refused at boot
// instead, where the message can name what to set.
//
// Two credential sources, in order:
//
//  1. The DEDICATED app registration (WARDYN_DIRECTORY_TENANT,
//     _CLIENT_ID, _CLIENT_SECRET). All three or none — a partial set is refused rather than
//     silently falling back to the OIDC one, which would use credentials the
//     operator did not mean to use.
//  2. DEFAULT: the OIDC app registration, tenant derived from the issuer. This
//     is the one-variable common case (WARDYN_DIRECTORY_PROVIDER=entra alone),
//     and it needs the SAME app to carry admin-consented Graph application
//     permissions — User.Read.All + Group.Read.All (Application.Read.All only if
//     App Roles should appear).
//
// The two legs PF-29 names are both refusals here because neither can do the
// client-credentials flow Graph requires: a PUBLIC OIDC client (PKCE, no secret)
// and no OIDC configured at all (an admin-token/local deployment has no issuer
// to derive a tenant from). A zero EntraConfig with a nil error means the
// feature is OFF — the caller wires no Directory, and every "who" field stays
// free text exactly as it behaves with this file unchanged.
func resolveDirectoryConfig(provider, dirTenant, dirClientID, dirSecret, oidcIssuer, oidcClientID, oidcSecret string) (directory.EntraConfig, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return directory.EntraConfig{}, nil
	}
	if !strings.EqualFold(provider, directoryProviderEntra) {
		return directory.EntraConfig{}, fmt.Errorf("refusing to start: unknown WARDYN_DIRECTORY_PROVIDER %q — "+
			"the only connector this build ships is %q (Okta/Google are later connectors behind the same interface); "+
			"fix the value or unset it to leave directory autocomplete off", provider, directoryProviderEntra)
	}

	dirTenant, dirClientID = strings.TrimSpace(dirTenant), strings.TrimSpace(dirClientID)
	// A DEDICATED registration is selected by naming ANY of its three vars —
	// not by naming all three — so the partial case lands on the refusal below
	// instead of quietly reusing the OIDC credentials.
	if dirTenant != "" || dirClientID != "" || dirSecret != "" {
		var missing []string
		if dirTenant == "" {
			missing = append(missing, "WARDYN_DIRECTORY_TENANT")
		}
		if dirClientID == "" {
			missing = append(missing, "WARDYN_DIRECTORY_CLIENT_ID")
		}
		if dirSecret == "" {
			missing = append(missing, "WARDYN_DIRECTORY_CLIENT_SECRET")
		}
		if len(missing) > 0 {
			return directory.EntraConfig{}, fmt.Errorf("refusing to start: the dedicated directory app registration is half-configured — %s %s empty; "+
				"set all three of WARDYN_DIRECTORY_TENANT/_CLIENT_ID/_CLIENT_SECRET, or unset all three to derive the credentials from the OIDC app registration instead",
				strings.Join(missing, " and "), plural(len(missing), "is", "are"))
		}
		return directory.EntraConfig{TenantID: dirTenant, ClientID: dirClientID, ClientSecret: dirSecret}, nil
	}

	// Leg 2 of PF-29: nothing to derive from at all.
	if strings.TrimSpace(oidcIssuer) == "" {
		return directory.EntraConfig{}, errors.New("refusing to start: WARDYN_DIRECTORY_PROVIDER=" + directoryProviderEntra + " but no OIDC issuer is configured — " +
			"the default credential path derives the tenant from WARDYN_OIDC_ISSUER and reuses WARDYN_OIDC_CLIENT_ID/_SECRET, and an admin-token or local-mode deployment has no IdP to derive either from; " +
			"register a dedicated least-privilege app in the tenant (User.Read.All + Group.Read.All, admin-consented) and set WARDYN_DIRECTORY_TENANT/_CLIENT_ID/_CLIENT_SECRET, " +
			"or unset WARDYN_DIRECTORY_PROVIDER to leave every \"who\" field as free text")
	}
	// Leg 1 of PF-29: a PUBLIC client (PKCE, no secret) — the documented,
	// SUPPORTED OIDC shape (WARDYN_OIDC_CLIENT_SECRET is optional), which is
	// exactly why this has to be caught: nothing else in boot would notice.
	if strings.TrimSpace(oidcClientID) == "" || oidcSecret == "" {
		return directory.EntraConfig{}, errors.New("refusing to start: WARDYN_DIRECTORY_PROVIDER=" + directoryProviderEntra + " but the OIDC app registration is a PUBLIC client (WARDYN_OIDC_CLIENT_SECRET is empty) — " +
			"Microsoft Graph needs the client-credentials flow, which a public client cannot perform at all, so every autocomplete would fail at the first keystroke; " +
			"register a dedicated least-privilege app in the tenant (User.Read.All + Group.Read.All, admin-consented) and set WARDYN_DIRECTORY_TENANT/_CLIENT_ID/_CLIENT_SECRET, " +
			"or unset WARDYN_DIRECTORY_PROVIDER to leave every \"who\" field as free text")
	}
	tenant := entraTenantFromIssuer(oidcIssuer)
	if tenant == "" {
		return directory.EntraConfig{}, fmt.Errorf("refusing to start: WARDYN_DIRECTORY_PROVIDER=%s but no Entra tenant can be derived from WARDYN_OIDC_ISSUER %q — "+
			"only a commercial-cloud Entra issuer carries one in its path (https://login.microsoftonline.com/<tenant>/v2.0, or the v1.0 https://sts.windows.net/<tenant>/), "+
			"and the connector talks to graph.microsoft.com, which no sovereign cloud serves; "+
			"set WARDYN_DIRECTORY_TENANT/_CLIENT_ID/_CLIENT_SECRET explicitly, or unset WARDYN_DIRECTORY_PROVIDER", directoryProviderEntra, oidcIssuer)
	}
	return directory.EntraConfig{TenantID: tenant, ClientID: strings.TrimSpace(oidcClientID), ClientSecret: oidcSecret}, nil
}

// entraTenantFromIssuer pulls the tenant id (or verified domain) out of an Entra
// issuer URL — the first path segment of both the v2.0 and v1.0 issuer forms.
// It returns "" for any other host ON PURPOSE: a Dex or Keycloak issuer also has
// a first path segment, and treating it as a tenant would turn a refusable
// misconfiguration into a runtime 502 against the wrong tenant id.
func entraTenantFromIssuer(issuer string) string {
	u, err := url.Parse(strings.TrimSpace(issuer))
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Hostname()) {
	case "login.microsoftonline.com", "sts.windows.net":
	default:
		return ""
	}
	seg, _, _ := strings.Cut(strings.Trim(u.Path, "/"), "/")
	return seg
}

// plural picks the verb form for a list length. ponytail: two words beat a
// second fmt.Errorf branch for the same sentence.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
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

// subscriptionInjectPosture decides whether this deployment may resolve a SHARED
// subscription credential into an agent run at all.
//
// Wardyn's subscription lanes inject ONE operator's live Anthropic OAuth token,
// proxy-side, from a server-global provider. On a single-user desktop that is the
// operator using their own subscription on their own machine. In a multi-user
// deployment it is that operator's subscription serving OTHER people's runs —
// which Anthropic's conditions for running Claude Code in agent infrastructure
// prohibit (each end user must authenticate with their own credential; customers
// may not intermediate Claude usage on their end users' behalf). The enterprise
// running Wardyn inherits that breach, and Wardyn's own audit trail records it.
//
// Three clauses, and LocalMode ALONE is not enough for any of them:
//
//  1. Not the k8s substrate. The Helm chart carries no WARDYN_LOCAL_MODE key, but
//     templates/deployment.yaml renders .Values.env VERBATIM, so
//     `--set env.WARDYN_LOCAL_MODE=true` boots a cluster into local mode, and
//     resolveLocalMode only refuses a SPECIFIC globally-routable bind (a pod's
//     unspecified :8080 warns). "k8s can't reach LocalMode" is therefore false,
//     and this clause is NOT waivable by the override below.
//  2. No OIDC issuer configured. WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC exists
//     precisely to permit local mode alongside a configured issuer — a multi-user
//     SSO deployment with auth bypassed. Gating on LocalMode alone would re-enable
//     sharing for the exact configuration this refusal exists to forbid. Also NOT
//     waivable.
//  3. Local mode, OR the explicit shared-subscription override. Compose is NOT
//     local mode by default (demo-admin-token, WARDYN_LOCAL_MODE=false), so
//     without the second disjunct the demo stack would break and the operator's
//     obvious "fix" would be to set WARDYN_LOCAL_MODE=true — turning authentication
//     off entirely. A safety gate must never create an incentive to weaken auth.
//
// The override is a PROMISE, not an enforcement: nothing stops someone setting it
// in values.env. That is exactly why clauses 1 and 2 sit inside the predicate
// where it cannot reach them.
//
// Returns the reason on refusal so the status surfaces can say WHY rather than
// rendering identically to "the operator never logged in".
func subscriptionInjectPosture(runnerTarget string, oidcConfigured, localMode, allowShared bool) (bool, string) {
	if runnerTarget == "k8s" {
		return false, "the Kubernetes runner is a multi-user deployment: a shared subscription credential would " +
			"serve other people's runs, which the harness vendor's terms prohibit. " +
			"Give each user their own API key (wardyn secret set anthropic-api-key) or use Bedrock"
	}
	if oidcConfigured {
		return false, "OIDC/SSO is configured, which declares that more than one human uses this deployment: " +
			"a shared subscription credential would serve other people's runs, which the harness vendor's terms prohibit. " +
			"Give each user their own API key (wardyn secret set anthropic-api-key) or use Bedrock"
	}
	if localMode || allowShared {
		return true, ""
	}
	return false, "subscription injection is limited to a single-user desktop/local deployment " +
		"(WARDYN_LOCAL_MODE). This daemon serves an authenticated multi-user API, so a shared " +
		"subscription credential would serve other people's runs. Use an API key or Bedrock — or, " +
		"for a demo box that is genuinely single-user, set WARDYN_ALLOW_SHARED_SUBSCRIPTION=true"
}

// parseMountCeilings parses the TWO operator/MDM-set mount ceilings at boot and
// logs their posture warnings: what a MEMBER may bind from their own machine
// (WARDYN_USER_WORKSPACE_ROOTS + its three siblings) and where an ADMIN may
// point a host_path USER DRIVE (WARDYN_USER_DRIVE_HOST_ROOTS).
//
// Together rather than inline, and together rather than apart, because they are
// one posture with two scopes and share every rule: parsed at BOOT so a
// malformed value fails closed here instead of at somebody's first onboarding
// or first run; unset means the narrow answer (a member mounts no host
// directory; no host_path drive may be registered at all); and a root at "/",
// the daemon's own $HOME, or (for the drive list) anything under a denied bind
// prefix is permitted but WARNED about — refusing would be safer, warning is
// what the surrounding boot code already does for a posture the operator may
// have chosen deliberately.
//
// Each warning is logged VERBATIM behind a neutral prefix, because those cases
// are warned about for OPPOSITE reasons and only the parser knows which is
// which: $HOME is far too WIDE, while "/" and a denied-prefix root are DEAD —
// they match NOTHING and refuse every drive (see the three-case doc on
// runner.ParseUserDriveHostRoots). Prefixing every line "dangerously wide"
// re-merged exactly what those parsers keep in separate sentences, and sent an
// operator hunting the drive it wrongly allowed instead of the drive it
// silently refused.
//
// The drive ceiling is the same rule ONE LEVEL UP: a drive's host_root is
// authored in the DATABASE by an admin and its per-person subdirectories are
// bound into OTHER PEOPLE's sandboxes, so the allowlist over it has to live
// where a console compromise cannot reach it.
//
// AND "ONE LEVEL UP" IS ITSELF A POSTURE NOTHING CHECKED. Each parser vets its
// own list and neither could see the other, so the two could name the SAME
// TREE and every check pass: a member then onboards the share as a workspace
// and binds it whole, every other person's home included, through a surface
// that consults no drive allocation. This function is where both lists exist at
// once, so the comparison is made here and warned about in the same voice
// (runner.MountCeilingOverlapWarnings).
func parseMountCeilings(f *bootFlags) (runner.MemberMountPolicy, []string, error) {
	memberMounts, memberWarns, err := runner.ParseMemberMountPolicy(
		*f.memberRoots, *f.memberRootsMap, *f.memberWritableRoots, *f.memberWritableDeny)
	if err != nil {
		return runner.MemberMountPolicy{}, nil, err
	}
	for _, warn := range memberWarns {
		slog.Warn("wardynd: member workspace roots — " + warn)
	}
	driveHostRoots, driveWarns, err := runner.ParseUserDriveHostRoots(*f.userDriveHostRoots)
	if err != nil {
		return runner.MemberMountPolicy{}, nil, err
	}
	for _, warn := range driveWarns {
		slog.Warn("wardynd: user drive host roots — " + warn)
	}
	// The relation BETWEEN the two ceilings, which neither parser can see. Its
	// own prefix: this is not a complaint about either list's contents, it is
	// one about the pair, and an operator who reads it has to change one of two
	// variables rather than the one the line happens to be filed under.
	for _, warn := range runner.MountCeilingOverlapWarnings(memberMounts, driveHostRoots) {
		slog.Warn("wardynd: mount ceilings overlap — " + warn)
	}
	return memberMounts, driveHostRoots, nil
}
