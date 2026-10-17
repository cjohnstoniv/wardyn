// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// envGitPATAPIBitbucketServer is whether a git_pat grant may set api: true for
// forge bitbucket_server. GitLab and Gitea need no flag; Bitbucket Server's table
// ships behind this one, off by default. Only wardynd reads it (envEnabled, so a
// garbage value is a boot refusal): the proxy gate reads no env and enforces the
// forge carried on the grant. Write, dispatch and Review all ask patAPIBitbucketOn.
const envGitPATAPIBitbucketServer = "WARDYN_GIT_PAT_API_BITBUCKET_SERVER"

func patAPIBitbucketOn() bool { return envEnabled(envGitPATAPIBitbucketServer) }

// errPATAPIBitbucketOff is the write refusal and the dispatch sentence's cause.
const errPATAPIBitbucketOff = "api: true on forge bitbucket_server needs WARDYN_GIT_PAT_API_BITBUCKET_SERVER, which is off on this deployment"

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
	if strict && sc.API && sc.Forge == types.PATForgeBitbucketServer && !patAPIBitbucketOn() {
		return fmt.Errorf("eligible_grants[%d]: git_pat scope %s", i, errPATAPIBitbucketOff)
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

// errPATNarrowingSSHConflict marks the policy-write refusal of a narrowed
// git_pat beside a same-forge ssh_key; specRefusalReason maps it to its wire
// reason.
var errPATNarrowingSSHConflict = errors.New(reasonGitPATNarrowingSSHConflict)

// validatePATNarrowingSSHConflict refuses a policy that narrows a git_pat grant
// (repos, access read or api) and also declares an ssh_key for the same forge,
// following validateGrantLaneExclusivity: SSH is a second push path the PAT
// broker cannot see, so the narrowing would not bind. Dispatch refuses the same
// pairing (patNarrowingRefusal), which stays authoritative for a stored policy
// this write never saw. An ssh_key is accepted only for github.com and
// dev.azure.com (sshOver443Endpoint), folded here as it is there.
func validatePATNarrowingSSHConflict(grants []types.GrantSpec) error {
	for i, g := range grants {
		if g.Kind != types.GrantGitPAT {
			continue
		}
		sc, err := types.DecodeGitPATScope(g.Scope)
		if err != nil || !sc.Narrowed() || !patSSHKeyFor(grants, sc.Host) {
			continue
		}
		return fmt.Errorf("%w: eligible_grants[%d] narrows a git_pat grant for %q (repos, access read or api), and this policy also "+
			"declares an ssh_key for the same forge. SSH is a second push path the PAT broker cannot see, so the narrowing would not bind. "+
			"Drop the ssh_key grant, or the narrowing", errPATNarrowingSSHConflict, i, sc.Host)
	}
	return nil
}
