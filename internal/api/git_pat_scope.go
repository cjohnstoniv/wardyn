// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// validatePATGrantScope is the write-time check of one git_pat grant's scope
// (validateEligibleGrant's git_pat arm, here because policy.go is the file the
// size gate watches).
//
// host and secret_name are required and a reserved platform-internal secret
// name is refused, so a policy can never exfiltrate wardyn-signing-key or
// session-key as a git password. nameSinkReservedSecret, not
// sinkReservedSecret (#1048): this kind returns the raw value into the sandbox
// when the broker is off, so it needs the WIDER guard that also refuses a
// wardyn-provider-*-key name. The broker sink (mintGitPAT) enforces the same
// invariant defense in depth.
//
// strict decodes the scope for a WRITE: an unknown key, an out-of-enum
// access/forge, a malformed repos entry and api: true are refused, because a
// typo such as "repo" would otherwise read as an omission and an omission means
// unnarrowed. Record-mode synthesis (strict=false) builds a policy from stored
// grant specs and reads them leniently, as every read does.
//
// A narrowed scope on an Azure DevOps host is refused too, naming that host's
// own lane: its git traffic is served by the Azure DevOps gate, which does not
// read repos, access or api. Only the hosts Azure DevOps names by address are
// known here; the dispatch refusal, which reads site config, is authoritative
// for the rest.
func validatePATGrantScope(i int, g types.GrantSpec, strict bool) error {
	decode := types.DecodeGitPATScope
	if strict {
		decode = types.DecodeGitPATScopeStrict
	}
	sc, derr := decode(g.Scope)
	if derr != nil {
		return fmt.Errorf("eligible_grants[%d]: git_pat scope invalid: %w", i, derr)
	}
	if nameSinkReservedSecret(sc.SecretName) {
		return fmt.Errorf("eligible_grants[%d]: git_pat references reserved secret name %q", i, sc.SecretName)
	}
	if strict && sc.SetsAnyAxis() && adoGrantHost(types.SiteConfig{}, sc.Host) {
		return fmt.Errorf("eligible_grants[%d]: git_pat scope sets repos, access, api or forge for the Azure DevOps host %q, "+
			"which is served by the Azure DevOps lane (azure_devops_capabilities and the Azure DevOps credential), "+
			"and that lane does not read those fields", i, sc.Host)
	}
	return nil
}

// validatePATNarrowedDuplicates refuses two git_pat grants for one host in one
// policy where either is narrowed. The proxy holds one grant per host, so the
// narrowing would depend on list order (today's last-wins), and a union would
// let one grant's PAT reach the other's repositories. Unnarrowed duplicates
// keep today's behaviour. A scope that does not decode is skipped here: the
// per-grant validation refuses it.
func validatePATNarrowedDuplicates(grants []types.GrantSpec) error {
	type hostGrant struct {
		host     string
		index    int
		narrowed bool
	}
	var prior []hostGrant
	for i, g := range grants {
		if g.Kind != types.GrantGitPAT {
			continue
		}
		sc, err := types.DecodeGitPATScope(g.Scope)
		if err != nil {
			continue
		}
		for _, p := range prior {
			if hostEqual(p.host, sc.Host) && (p.narrowed || sc.Narrowed()) {
				return fmt.Errorf("eligible_grants[%d] and eligible_grants[%d]: two git_pat grants for host %q where one sets "+
					"repos, access read or api: only one grant per host is enforced, so the narrowing would depend on list order — "+
					"merge them into one grant, or use different hosts", p.index, i, sc.Host)
			}
		}
		prior = append(prior, hostGrant{sc.Host, i, sc.Narrowed()})
	}
	return nil
}

// refusePATNarrowedDuplicates is validatePATNarrowedDuplicates at create: a 422
// written here, for the spec a run is about to be built from, which for a stored
// policy no write-time check has necessarily seen.
func refusePATNarrowedDuplicates(w http.ResponseWriter, prefix string, spec types.RunPolicySpec) bool {
	if err := validatePATNarrowedDuplicates(spec.EligibleGrants); err != nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonInlinePolicyInvalid, prefix+err.Error())
		return true
	}
	return false
}
