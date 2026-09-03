// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// THE comparator for "which ceiling grant bounds this proposed grant".
//
// It lives here, in composer, because this package holds the RUNTIME clamp
// (clampGrants) and internal/api — which holds the WRITE-TIME comparator
// (governanceGrantWithinCeiling) — imports composer rather than the other way
// round. One definition, two callers: the write-time comparator refuses a
// profile grant no ceiling grant dominates, the runtime clamp narrows a run's
// grant to the bound of the ceiling grant that covers it, and both ask THIS file
// which ceiling grant that is.
//
// They used to ask differently, and disagreed on the axis that matters. The
// comparator searched per-grant ("some single ceiling grant dominates on every
// axis"); the clamp built `map[GrantKind]GrantSpec` and let the LAST same-kind
// ceiling grant win, then took RequiresApproval and TTL from that arbitrary
// grant. A ceiling listing two ssh_key grants — the normal shape for two forges,
// and equally normal for api_key and git_pat — therefore clamped a proposal
// naming the STRICT forge's pairing to the PERMISSIVE forge's approval posture
// and TTL, and returned different answers for the same ceiling SET depending on
// slice order. A stripped requires_approval auto-mints the injection at proxy
// boot with no human in the loop, which is the widening the write-time
// comparator was written to refuse and the runtime clamp was quietly allowing.
package composer

import (
	"encoding/json"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// grantPairing is the (host, secret, known_hosts) triple a stored-secret grant
// names — the identity a ceiling entry is matched on, as opposed to the BOUNDS
// (approval, TTL, github scope) that are clamped once a match is found.
type grantPairing struct {
	host          string // the destination host; for env_secret, the env var NAME (see below)
	secretRef     string
	knownHostsRef string // ssh_key only; empty (and compared as such) for every other kind
}

// GrantPairing returns the pairing a grant names and whether its kind names a
// stored secret at all.
//
// covered=false is the honest answer for github_token and cloud_sts: they name
// no stored secret (github_token mints an App installation token, cloud_sts is
// refused by the embedded IdP), so same-kind membership IS their identity test
// and the github repo/permission subset is checked separately.
//
// ok=false means the scope did not decode into a usable pairing. It is
// deliberately NOT an error return: this file answers "does this ceiling entry
// cover this grant", and an unreadable scope simply cannot be said to cover
// anything — so it never MATCHES, which is the fail-closed direction. Whether an
// unreadable scope is a REQUEST error is a different question, answered where a
// grant is validated for delivery (internal/api's storedSecretGrantPairing,
// which fails such a grant closed).
//
// env_secret puts the env var NAME in the host slot, so the ceiling comparison
// is an exact (name, secret) pairing rather than a secret a member could re-home
// to a variable the operator never wrote.
func GrantPairing(g types.GrantSpec) (host, secretRef, knownHostsRef string, covered, ok bool) {
	p, covered, ok := grantPairingOf(g)
	return p.host, p.secretRef, p.knownHostsRef, covered, ok
}

func grantPairingOf(g types.GrantSpec) (p grantPairing, covered, ok bool) {
	// One decode struct for every kind: the four stored-secret scopes differ
	// only in which field names carry the host and the secret, and enumerating
	// them here keeps the pairing rule readable as the single table it is.
	var sc struct {
		Host                string `json:"host"`
		Name                string `json:"name"`
		SecretName          string `json:"secret_name"`
		KeySecretRef        string `json:"key_secret_ref"`
		KnownHostsSecretRef string `json:"known_hosts_secret_ref"`
	}
	switch g.Kind {
	case types.GrantAPIKey, types.GrantGitPAT:
		if json.Unmarshal(g.Scope, &sc) != nil || sc.Host == "" || sc.SecretName == "" {
			return grantPairing{}, true, false
		}
		return grantPairing{host: sc.Host, secretRef: sc.SecretName}, true, true
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
	default:
		// github_token, cloud_sts, and any kind added later. Reporting
		// covered=false for an UNKNOWN kind is safe here in a way it is not at a
		// delivery gate: not-covered means "matched on kind alone", so a new
		// stored-secret kind is bounded by its kind's ceiling entry until
		// someone adds it above — never unbounded.
		return grantPairing{}, false, true
	}
}

// samePairing compares two pairings the way the deployment means them: the host
// case-insensitively and ignoring a trailing dot (the same normalization
// internal/api's hostEqual applies, because "corp.example." and "corp.example"
// are the same host and a ceiling written either way must bind), the secret
// refs byte-exactly (a secret NAME is a store key, not a hostname).
func samePairing(a, b grantPairing) bool {
	norm := func(h string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), ".")) }
	return a.secretRef == b.secretRef && norm(a.host) == norm(b.host) && a.knownHostsRef == b.knownHostsRef
}

// PairingInCeiling reports whether some ceiling grant of the SAME kind names the
// same stored-secret pairing. This is the identity axis of the one comparator —
// internal/api's storedSecretPairingInCeiling forwards to it, so the member
// grant filter, the governance profile bound and the runtime clamp cannot drift
// into three answers for one question.
func PairingInCeiling(kind types.GrantKind, host, secretRef, knownHostsRef string, ceiling []types.GrantSpec) bool {
	want := grantPairing{host: host, secretRef: secretRef, knownHostsRef: knownHostsRef}
	for _, cg := range ceiling {
		if cg.Kind != kind {
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

// ceilingGrantsBounding returns the ceiling grants whose BOUNDS apply to g.
//
// Two shapes, and the split is the whole rule:
//
//   - PAIRED. The kind names a stored secret and some ceiling grant names the
//     same pairing ⇒ that ONE grant, and its bounds alone. This is exactly the
//     write-time comparator's "some single ceiling grant dominates on every
//     axis": the approval posture and TTL a proposal is held to are the ones the
//     operator wrote NEXT TO THAT PAIRING, never another pairing's.
//
//   - UNPAIRED. The kind names no stored secret (github_token, cloud_sts), or it
//     does and no ceiling entry pairs it ⇒ EVERY same-kind grant, whose bounds
//     are met (strictest wins) by the caller.
//
// The meet is the safe fallback, not a second rule: it is order-independent and
// can only narrow, so a proposal that reaches it is never handed a bound the
// operator did not write somewhere. It is also not the widening the write-time
// comparator forbids — that one is about ACCEPTING a proposal by taking one
// ceiling grant's approval posture with another's TTL, and a meet takes the
// strictest of both.
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
