// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"log/slog"
	"slices"
	"strings"
)

// claimKeyedRoleMapValues returns the role-map keys only a `roles`/`groups`
// claim can answer, sorted (map order isn't, and the boot and login halves of
// this warning must render the same fact identically). A key holding "@" is
// an email, already covered by the requested `email` claim, so it never
// triggers the warning.
func claimKeyedRoleMapValues(roleMap map[string]string) []string {
	var claimKeyed []string
	for k := range roleMap {
		if !strings.Contains(k, "@") {
			claimKeyed = append(claimKeyed, k)
		}
	}
	slices.Sort(claimKeyed)
	return claimKeyed
}

// warnMergedMapNeedsGroupsScope is the login-time half of
// warnUnrequestedGroupsScope: boot only sees Config.RoleMap, but login uses
// that merged with Config.RoleMappings (console rows added/removed at
// runtime), so a console-only deployment needs this check at login, not
// construction.
//
// Fires only when all three hold: the provider advertises an unrequested
// `groups` scope (Entra never does); the merged map has a value only a
// `roles`/`groups` claim can answer; and no login this process has seen
// carried that claim (latched permanently once one does).
//
// Logs once per process, not per login — an operator can't fix this from the
// log, so repeating it is just noise. It never denies the login: an omitted
// `groups` claim is indistinguishable from "this human is in no groups", so
// failing closed here would lock out every human on an IdP that sends none.
func (a *Authenticator) warnMergedMapNeedsGroupsScope(roleMap map[string]string, rolesClaim, groupsClaim []string) {
	if !a.groupsScopeUnrequested {
		return
	}
	if len(rolesClaim) > 0 || len(groupsClaim) > 0 {
		// This IdP does send the claim: latch it, settling the question for good.
		a.sawGroupClaim.Store(true)
		return
	}
	if a.sawGroupClaim.Load() {
		return
	}
	claimKeyed := claimKeyedRoleMapValues(roleMap)
	if len(claimKeyed) == 0 {
		return
	}
	a.warnedMergedGroupsScope.Do(func() {
		slog.Warn("oidc: no login on this deployment has carried a `roles` or `groups` claim, the merged role map "+
			"(chart plus console-managed rows) is keyed on values only such a claim can answer, and this provider "+
			"advertises a `groups` scope Wardyn does not request — if the IdP gates the claim behind that scope it "+
			"sends none, which is indistinguishable from `this human is in no groups`, so those rows decide nothing",
			"issuer", a.cfg.IssuerURL, "scope", groupsScope, "requested_scopes", a.oauth2.Scopes,
			"claim_keyed_merged_map_values", claimKeyed,
			"env", "WARDYN_OIDC_ROLE_MAP", "console_store", a.cfg.RoleMappings != nil)
	})
}
