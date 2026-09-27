// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"fmt"
	"log/slog"
	"slices"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
)

// scopesSupported reads the provider discovery document's scopes_supported,
// or (nil, false) when the document is unreadable or the key is absent/empty.
// Shared by providerGatesGroupsScope and validateExtraScopes so "can this
// provider's advertised scopes even be asked" is answered identically in both
// places: scopes_supported is OPTIONAL in OIDC discovery, so a real,
// spec-compliant provider may omit it, and that absence is not evidence of
// anything — never grounds to warn OR to refuse boot.
func scopesSupported(provider *gooidc.Provider) ([]string, bool) {
	var meta struct {
		ScopesSupported []string `json:"scopes_supported"`
	}
	if err := provider.Claims(&meta); err != nil || len(meta.ScopesSupported) == 0 {
		return nil, false
	}
	return meta.ScopesSupported, true
}

// providerGatesGroupsScope reports whether this provider's discovery document
// advertises a `groups` scope that the authorization request does not ask for —
// the single condition both halves of the warning (warnUnrequestedGroupsScope,
// its login-time twin warnMergedMapNeedsGroupsScope in groups_scope.go) are
// keyed on, computed in one place so they can never disagree about a
// provider's posture.
//
// A discovery document this build cannot read, or one that publishes no
// scopes_supported at all, is NOT evidence that the provider gates `groups`:
// both answer false rather than guess and cry wolf on every login screen that
// follows. `requested` already carrying `groups` (Config.ExtraScopes, #1101)
// answers false unconditionally — asked for, so there is nothing left to warn
// about — which is also how configuring that scope silences BOTH warnings
// with no separate switch.
func providerGatesGroupsScope(provider *gooidc.Provider, requested []string) bool {
	if slices.Contains(requested, groupsScope) {
		return false // asked for; there is nothing to warn about
	}
	supported, ok := scopesSupported(provider)
	return ok && slices.Contains(supported, groupsScope)
}

// validateExtraScopes checks Config.ExtraScopes (WARDYN_OIDC_EXTRA_SCOPES,
// #1101) against the provider's discovery scopes_supported, once at BOOT
// rather than at every login: an unsupported scope must refuse boot with a
// named reason, never surface as invalid_scope at a human's login — exactly
// the failure asking blind risks (Entra defines no such scopes and rejects
// any it does not recognise outright, locking out every human on an upgrade
// nobody opted into).
//
// A provider that publishes NO scopes_supported at all is not refused: the
// list is OPTIONAL in OIDC discovery, so an absent one is not proof the scope
// is unsupported, only that this provider does not say — refusing boot on
// that absence would punish deployments that did nothing wrong. It WARNS
// instead and trusts the operator's own configuration, the same asymmetry
// providerGatesGroupsScope draws from an absent list.
func validateExtraScopes(provider *gooidc.Provider, extra []string) error {
	if len(extra) == 0 {
		return nil
	}
	supported, ok := scopesSupported(provider)
	if !ok {
		slog.Warn("oidc: this provider's discovery document publishes no scopes_supported, so WARDYN_OIDC_EXTRA_SCOPES cannot be checked against it — requesting it unchecked rather than refusing boot on an absent list",
			"extra_scopes", extra)
		return nil
	}
	for _, s := range extra {
		if !slices.Contains(supported, s) {
			return fmt.Errorf("oidc: WARDYN_OIDC_EXTRA_SCOPES names %q, which this provider's discovery document does not advertise in scopes_supported (%v) — refusing to start rather than fail every human's login with invalid_scope",
				s, supported)
		}
	}
	return nil
}
