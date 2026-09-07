// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"log/slog"
	"slices"
	"strings"
)

// claimKeyedRoleMapValues returns the role-map keys that ONLY a `roles` or
// `groups` claim can answer, sorted.
//
// A key holding "@" is an email, and the `email` claim is one the authorization
// request already asks for — such a row is unaffected by a gated group scope
// and must never trigger either half of the warning. One implementation, used
// by the boot half and the login half alike, so "which rows depend on the group
// claim" cannot come out differently in the two places that ask it.
//
// Sorted because a map's iteration order is not, and an operator comparing two
// restarts (or two log lines) must not see the same fact rendered two ways.
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

// warnMergedMapNeedsGroupsScope is the LOGIN-TIME half of
// warnUnrequestedGroupsScope, and it exists because the boot half is
// structurally blind to most of what it is about.
//
// Boot sees Config.RoleMap — the chart's WARDYN_OIDC_ROLE_MAP — and nothing
// else. The map a login actually derives from is that one MERGED with
// Config.RoleMappings, the console's Getting Started -> People rows, which are
// created and deleted through the API while the process runs. A deployment that
// manages every group->role row from the console therefore booted completely
// silent on exactly the finding the boot warning was added for, and reading the
// store at construction would not have closed it: the row added at 10am was not
// there at 9am. The merged map only exists at login, so this is where the
// question gets asked.
//
// THE CONDITIONS, all three, or it is noise:
//
//   - The provider advertises a `groups` scope this request does not ask for
//     (a.groupsScopeUnrequested, read from the discovery document in New). Every
//     Entra tenant answers false here — Entra defines no such scope and emits
//     the claim without one — so the documented Entra path never sees this line.
//   - The MERGED map holds a value only a claim can answer. A map keyed purely
//     on emails depends on nothing that is missing.
//   - No login since this process started has carried a `roles` or `groups`
//     value. One that did is proof the IdP sends the claim to this client, and
//     the question is then settled for good — sawGroupClaim latches, and the
//     line can never fire afterwards.
//
// It is deliberately ONE line per process (warnedMergedGroupsScope): this is an
// observation about how the deployment is configured, not an event, and a
// per-login warning about a condition the operator cannot fix from the log is
// how a warning gets filtered out permanently.
//
// It cannot be a DENIAL. An omitted `groups` claim is byte-for-byte "this human
// is in no groups" — there is no `_claim_names` marker to fail closed on, the
// way an Entra overage has (see claimsOverage) — so refusing the login here
// would lock out every human on an IdP that simply has no groups to send.
// Telling the operator is the only honest move available.
func (a *Authenticator) warnMergedMapNeedsGroupsScope(roleMap map[string]string, rolesClaim, groupsClaim []string) {
	if !a.groupsScopeUnrequested {
		return
	}
	if len(rolesClaim) > 0 || len(groupsClaim) > 0 {
		// This IdP does send the claim to this client. Latch it: the warning is
		// about a claim that never arrives, and one that did answers the
		// question for every login after this one too.
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
