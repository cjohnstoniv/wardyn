// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The comparator for "which ceiling grant bounds this proposed grant". Lives here because composer holds
// the RUNTIME clamp and internal/api (the WRITE-TIME comparator) imports composer, not the reverse — one
// definition, two callers.
//
// SECURITY invariant: both callers must ask it the same way — some single ceiling grant must dominate the
// proposal on every axis. A per-kind map must never supply the bound instead: the LAST same-kind ceiling
// grant would win, so a ceiling listing two ssh_key grants (normal for two forges) could clamp a proposal
// naming the STRICT forge's pairing to the PERMISSIVE forge's posture, answering differently by slice order.
package composer

import (
	"encoding/json"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// grantPairing is the identity a ceiling entry is matched on, as opposed to the BOUNDS (approval, TTL,
// github scope) clamped once a match is found.
//
// SECURITY: header/format/requireTLS are part of that IDENTITY for api_key, not a bound — matching only on
// (host, secret) would let a member/profile grant keep the blessed (host, secret) pairing while re-homing
// it onto a DIFFERENT header/format, or DROPPING require_tls onto a cleartext transport the operator
// refused. Each is exact-match, so a dropped declaration matches nothing.
type grantPairing struct {
	host          string // the destination host; for env_secret, the env var NAME; for file_secret, the file name
	secretRef     string
	knownHostsRef string // ssh_key only; empty for every other kind

	header string // api_key only; empty for every other kind
	format string // api_key only; empty for every other kind

	requireTLS bool // api_key only; false for every other kind
}

// apiKeyHeader/apiKeyFormat mirror injectionRuleFromScope's defaults. Header names are case-insensitive
// per RFC 9110, so the pairing folds case on the header and compares format byte-exactly (a fmt verb
// string, not a name).
func apiKeyHeader(h string) string {
	if strings.TrimSpace(h) == "" {
		return "authorization"
	}
	return strings.ToLower(strings.TrimSpace(h))
}

func apiKeyFormat(f string) string {
	if f == "" {
		return "Bearer %s"
	}
	return f
}

// GrantPairing returns the pairing a grant names and whether its kind names a stored secret at all.
// covered=false is honest for github_token/cloud_sts, which name no stored secret. ok=false means the
// scope didn't decode into a usable pairing — deliberately not an error return, since fail-closed here
// means an unreadable scope never MATCHES anything. env_secret puts the env var NAME in the host slot,
// file_secret the file name.
func GrantPairing(g types.GrantSpec) (host, secretRef, knownHostsRef string, covered, ok bool) {
	p, covered, ok := grantPairingOf(g)
	return p.host, p.secretRef, p.knownHostsRef, covered, ok
}

func grantPairingOf(g types.GrantSpec) (p grantPairing, covered, ok bool) {
	// One decode struct for every kind: the four stored-secret scopes differ only in which field names
	// carry the host and the secret.
	var sc struct {
		Host                string `json:"host"`
		Name                string `json:"name"`
		File                string `json:"file"`
		SecretName          string `json:"secret_name"`
		KeySecretRef        string `json:"key_secret_ref"`
		KnownHostsSecretRef string `json:"known_hosts_secret_ref"`
		Header              string `json:"header"`
		Format              string `json:"format"`
		RequireTLS          bool   `json:"require_tls"`
	}
	switch g.Kind {
	case types.GrantAPIKey:
		if json.Unmarshal(g.Scope, &sc) != nil || sc.Host == "" || sc.SecretName == "" {
			return grantPairing{}, true, false
		}
		return grantPairing{
			host: sc.Host, secretRef: sc.SecretName,
			header: apiKeyHeader(sc.Header), format: apiKeyFormat(sc.Format),
			requireTLS: sc.RequireTLS,
		}, true, true
	case types.GrantGitPAT:
		pat, err := types.DecodeGitPATScope(g.Scope)
		if err != nil {
			return grantPairing{}, true, false
		}
		return grantPairing{host: pat.Host, secretRef: pat.SecretName}, true, true
	case types.GrantSSHKey:
		if json.Unmarshal(g.Scope, &sc) != nil || sc.Host == "" || sc.KeySecretRef == "" {
			return grantPairing{}, true, false
		}
		return grantPairing{host: sc.Host, secretRef: sc.KeySecretRef, knownHostsRef: sc.KnownHostsSecretRef}, true, true
	case types.GrantEnvSecret:
		if json.Unmarshal(g.Scope, &sc) != nil || sc.Name == "" || sc.SecretName == "" {
			return grantPairing{}, true, false
		}
		return grantPairing{host: sc.Name, secretRef: sc.SecretName}, true, true
	case types.GrantFileSecret:
		if json.Unmarshal(g.Scope, &sc) != nil || sc.File == "" || sc.SecretName == "" {
			return grantPairing{}, true, false
		}
		return grantPairing{host: sc.File, secretRef: sc.SecretName}, true, true
	default:
		// github_token, cloud_sts, and any kind added later: covered=false means "matched on kind alone",
		// so a new stored-secret kind stays bounded by its kind's ceiling entry until added above — never
		// unbounded.
		return grantPairing{}, false, true
	}
}

// samePairing compares two pairings the way the deployment means them: host case-insensitively and
// ignoring a trailing dot (matching internal/api's hostEqual), secret refs byte-exactly.
func samePairing(a, b grantPairing) bool {
	norm := func(h string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), ".")) }
	return a.secretRef == b.secretRef && norm(a.host) == norm(b.host) && a.knownHostsRef == b.knownHostsRef &&
		a.header == b.header && a.format == b.format && a.requireTLS == b.requireTLS
}

// PairingInCeiling reports whether some ceiling grant of the SAME kind names the same stored-secret
// pairing — the identity axis of the one comparator (internal/api's storedSecretPairingInCeiling forwards
// to it). Takes the whole proposed grant, not a destructured pairing, so the pairing rule lives in exactly
// ONE decoder. Fail-closed: a grant whose kind names no stored secret, or whose scope doesn't decode, is
// in no pairing.
func PairingInCeiling(g types.GrantSpec, ceiling []types.GrantSpec) bool {
	want, covered, ok := grantPairingOf(g)
	if !covered || !ok {
		return false
	}
	for _, cg := range ceiling {
		if cg.Kind != g.Kind {
			continue
		}
		got, covered, ok := grantPairingOf(cg)
		if !covered || !ok {
			continue
		}
		if samePairing(got, want) {
			return true
		}
	}
	return false
}

// ceilingGrantsBounding returns the ceiling grants whose BOUNDS apply to g. Two shapes:
//
//   - PAIRED: the kind names a stored secret and some ceiling grant names the same pairing ⇒ that ONE
//     grant's bounds alone — approval posture and TTL come from what's written NEXT TO THAT PAIRING, never
//     another pairing's.
//
//   - UNPAIRED: no stored secret (github_token, cloud_sts), or no ceiling entry pairs it ⇒ EVERY same-kind
//     grant, bounds met (strictest wins) by the caller — order-independent, can only narrow, so this is a
//     safe fallback, not the widening the write-time comparator forbids.
//
// Empty result = the kind is outside the ceiling entirely; the caller drops g.
func ceilingGrantsBounding(g types.GrantSpec, ceiling []types.GrantSpec) []types.GrantSpec {
	var sameKind []types.GrantSpec
	for _, cg := range ceiling {
		if cg.Kind == g.Kind {
			sameKind = append(sameKind, cg)
		}
	}
	if len(sameKind) <= 1 {
		return sameKind
	}
	want, covered, ok := grantPairingOf(g)
	if !covered || !ok {
		return sameKind
	}
	for _, cg := range sameKind {
		got, cgCovered, cgOK := grantPairingOf(cg)
		if cgCovered && cgOK && samePairing(got, want) {
			return []types.GrantSpec{cg}
		}
	}
	return sameKind
}
