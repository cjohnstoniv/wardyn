// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"net/netip"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// localModeState is the resolved LOCAL HOST MODE posture: whether the no-auth
// bypass is in effect, the operator principal to stamp, and whether the bind is
// loopback (also feeds /setup/status readiness).
type localModeState struct {
	enabled  bool
	operator string
	loopback bool
	// trustedPeers is api.Config.LocalTrustedPeers, resolved only in local mode
	// with -local-trust-forwarder.
	trustedPeers []netip.Prefix
}

// resolveAWSSSOEndpointOverride resolves the gated AWS SSO endpoint hatch:
// refuse when it is set without the acknowledgement, otherwise normalize it and
// WARN — loudly, every boot, naming it a test hatch. The warning is the point:
// this is the one knob that makes wardynd treat an arbitrary HTTP server as AWS
// IAM Identity Center, and an operator who inherits a values file carrying it
// must see that in the log rather than discover it from an audit row.
//
// The refusal shape is this file's own -local-mode-with-OIDC refusal below,
// deliberately: a posture this dangerous takes two explicit acts, never one
// stray env var.
func resolveAWSSSOEndpointOverride(f *bootFlags) (string, error) {
	override, err := api.ValidateAWSSSOEndpointOverride(*f.awsSSOEndpointOverride, *f.allowTestEndpoints)
	if err != nil {
		return "", err
	}
	if override != "" {
		slog.Warn(api.AWSSSOEndpointOverrideWarn,
			slog.String("aws_sso_endpoint_override", override),
		)
	}
	return override, nil
}

// resolveLocalMode decides the LOCAL HOST MODE posture (single-developer
// localhost path): bypass public-API auth so the browser UI works with no
// SSO/Dex and no token. Auto-enable when no auth is configured AND the bind is
// loopback; otherwise honor the explicit flag. FAIL CLOSED: never serve a
// no-auth public API on a publicly-routable IP. The sidecar/run-token path
// (internalAuth) is unaffected either way.
func resolveLocalMode(f *bootFlags) (localModeState, error) {
	lm := localModeState{loopback: listenIsLoopback(*f.listen)}
	// bug-rbac-1: an EXPLICIT -local-mode alongside a configured -oidc-issuer
	// is refused up front, before it can ever reach lm.enabled below. The
	// auto-enable heuristic already excludes a configured issuer
	// (*f.oidcIssuer == "" in the OR's right-hand side), so this refusal only
	// ever fires for the explicit flag — humanOrAdminAuth branches on
	// LocalMode FIRST and bypasses OIDC entirely without ever consulting it,
	// so a boot that got this far would silently drop the whole SSO/RBAC
	// deployment to a fixed local:operator with no warning anywhere.
	if *f.localMode && strings.TrimSpace(*f.oidcIssuer) != "" && !*f.allowLocalModeWithOIDC {
		return lm, fmt.Errorf("refusing to start: -local-mode is explicitly set alongside a configured -oidc-issuer %q — "+
			"local mode bypasses ALL public-API auth and would silently disable the configured OIDC/SSO admin-member RBAC deployment; "+
			"unset -local-mode (WARDYN_LOCAL_MODE) or -oidc-issuer, or explicitly set WARDYN_ALLOW_LOCAL_MODE_WITH_OIDC=true to override", *f.oidcIssuer)
	}
	lm.enabled = *f.localMode || (*f.adminToken == "" && *f.oidcIssuer == "" && lm.loopback)
	lm.operator = strings.TrimSpace(*f.localOperator)
	// S-06: "admin-token" is the reserved MECHANISM principal a per_user row's
	// harness-login refuses (internal/api's adminTokenPrincipal) — naming the
	// local operator seat that would collide it with a non-person, refusing
	// the SAME seat boot just accepted.
	if lm.operator == api.AdminTokenPrincipal() {
		return lm, fmt.Errorf("refusing to start: -local-operator (WARDYN_LOCAL_OPERATOR) is %q, the reserved admin-token mechanism principal — local mode's operator seat must be a real, distinguishable person; pick a different name", lm.operator)
	}
	// The demo admin token is published in this repo (compose + docs) and nothing
	// mints a random one, so "auth configured" with THAT value is a full-admin API
	// anyone who read the README can drive. This check sits ABOVE the !lm.enabled
	// return on purpose: setting an admin token is exactly what turns local mode
	// off. Same refuse-vs-warn split as -local-trust-forwarder below — refuse on a
	// specific routable bind, warn on the unspecified one, which from inside a
	// container is indistinguishable from the safe compose 127.0.0.1-publish. Honest
	// ceiling: an operator who republishes compose's port on 0.0.0.0 still has
	// WARDYN_LISTEN=":8080" in the container, so the refusal cannot fire for that
	// (most realistic) exposure path — this is a backstop, not full coverage.
	if strings.TrimSpace(*f.adminToken) == demoAdminToken {
		if listenBindsSpecificRoutable(*f.listen) {
			return lm, fmt.Errorf("refusing to start: WARDYN_ADMIN_TOKEN is the demo token published in this repo (deploy/compose + docs) but the listen address %q binds a specific non-loopback interface — that is an effectively-unauthenticated admin API; set a secret token (e.g. `openssl rand -hex 32`) or bind loopback", *f.listen)
		}
		slog.Warn("wardynd: WARDYN_ADMIN_TOKEN is the demo token published in this repo — anyone who read the docs has full admin. Fine for the loopback-published demo stack; set a secret token before exposing this port.",
			slog.String("listen", *f.listen),
		)
	}
	if err := adminTokenStrengthRefusal(*f.adminToken, *f.listen); err != nil {
		return lm, err
	}
	if !lm.enabled {
		return lm, nil
	}
	if listenIsRoutablePublic(*f.listen) {
		return lm, fmt.Errorf("refusing to start: -local-mode bypasses authentication but the listen address %q is a publicly-routable IP; bind to loopback (127.0.0.1) or a private address, or configure auth (WARDYN_ADMIN_TOKEN / OIDC)", *f.listen)
	}
	// -local-trust-forwarder DISABLES the unspoofable loopback-PEER gate (it
	// trusts a loopback-only host publish so the peer is always the docker
	// gateway). That holds ONLY on a loopback bind or the compose
	// 0.0.0.0-in-container topology whose host publishes 127.0.0.1:PORT. On a
	// SPECIFIC non-loopback interface (private/RFC1918, link-local, or public)
	// it re-opens UNAUTHENTICATED LAN admin access, so refuse to start. The
	// unspecified all-interfaces bind is indistinguishable from the safe
	// compose case from inside the container (wardynd cannot see the host's
	// docker publish), so it earns a DISTINCT error-level log naming the exact
	// requirement rather than a refusal.
	if *f.localTrustFwd {
		if listenBindsSpecificRoutable(*f.listen) {
			return lm, fmt.Errorf("refusing to start: -local-trust-forwarder disables the loopback-peer gate but the listen address %q binds a specific non-loopback interface — "+
				"this re-opens UNAUTHENTICATED LAN admin access; use it ONLY when the port is published loopback-only (127.0.0.1:PORT), i.e. bind loopback here or an unspecified address inside a compose container", *f.listen)
		}
		if !lm.loopback {
			slog.Error("wardynd: -local-trust-forwarder on an unspecified bind DISABLES the loopback-peer gate — the UNAUTHENTICATED public API is exposed to the LAN unless the host publishes 127.0.0.1:PORT ONLY (the Compose default). Verify your docker publish / host firewall.",
				slog.String("listen", *f.listen),
			)
		}
		var raw string
		if f.localTrustedPeers != nil { // a test's partial bootFlags
			raw = *f.localTrustedPeers
		}
		peers, err := localTrustedPeers(raw)
		if err != nil {
			return lm, err
		}
		lm.trustedPeers = peers
		slog.Info("wardynd: local mode trusts these forwarder peers", slog.Any("peers", peers))
	}
	// The four-eyes egress switch cannot be enforced in local mode, and an
	// operator who set both learns that when their FIRST approval hangs — a 503
	// per decision, forever. Say it at boot instead.
	//
	// Scoped to the combination that is actually broken, and only that one: local
	// mode authenticates nobody, so both the decider and the run's created_by
	// come from the same client-supplied source and no request in that mode can
	// prove a second human decided (requireSecondHuman refuses outright). With
	// either half alone there is nothing to say — the switch works normally off
	// local mode, and local mode is unaffected with the switch unset — so this
	// stays silent for both. A boot warning that fires on a merely unusual
	// configuration gets filtered out of the logs within a week, and is then
	// missing for the deployment that needed it.
	//
	// WARN, not a refusal: everything else in the deployment works, so refusing
	// to start would cost an operator their whole daemon over one disabled
	// control. The message names the consequence and the remedy.
	if api.EgressSecondHumanEnabled() {
		slog.Warn("wardynd: WARDYN_EGRESS_SECOND_HUMAN is set but LOCAL MODE authenticates nobody — the four-eyes gate cannot be enforced here, so EVERY egress_domain approval decision will be refused with 503. Configure SSO to use this switch, or unset it.",
			slog.String("listen", *f.listen),
		)
	}
	// The governance switch, once its boot refusal is lifted (exitOnGovernanceFlags): local mode cannot
	// enforce it either, and every covered write and every approval is refused with a 503.
	if api.GovernanceSecondHumanEnabled() {
		slog.Warn("wardynd: WARDYN_GOVERNANCE_SECOND_HUMAN is set but LOCAL MODE authenticates nobody — the four-eyes gate cannot be enforced here, so EVERY governance write and approval will be refused with 503. Configure SSO to use this switch, or unset it.",
			slog.String("listen", *f.listen),
		)
	}
	// The capability switch is the same story for Azure DevOps escalations.
	if api.CapabilitySecondHumanEnabled() {
		slog.Warn("wardynd: WARDYN_CAPABILITY_SECOND_HUMAN is set but LOCAL MODE authenticates nobody — the four-eyes gate cannot be enforced here, so EVERY Azure DevOps capability approval decision will be refused with 503. Configure SSO to use this switch, or unset it.",
			slog.String("listen", *f.listen),
		)
	}
	if lm.operator == "" {
		lm.operator = defaultLocalOperator()
	}
	if lm.loopback {
		slog.Info("wardynd: LOCAL HOST MODE — public-API auth disabled; loopback bind. No SSO/token required.",
			slog.String("operator", lm.operator),
			slog.String("listen", *f.listen),
		)
	} else {
		slog.Warn("wardynd: LOCAL HOST MODE on a non-loopback bind — the UNAUTHENTICATED public API is reachable beyond localhost; ensure a host firewall or configure auth.",
			slog.String("listen", *f.listen),
			slog.String("operator", lm.operator),
		)
	}
	return lm, nil
}
