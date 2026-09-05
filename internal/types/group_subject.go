// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The rule for what a GROUP SUBJECT is, in the one package every write boundary
// that stores one can already reach.
//
// A group subject is matched by exact string equality against the login-time
// group snapshot (store_capabilities.go's `subject = ANY($2::text[])`, and the
// same shape in ResolveGovernanceProfile). That snapshot is strictly narrower
// than "lowercase it": it carries printable ASCII only, guarded BEFORE the
// Unicode fold. So a subject outside that set is a row that can never match
// anyone — a DENY that protects nothing while the console renders it as active,
// and a group tier that HasGroupTierDriveGrants still counts as present.
//
// WHY HERE. internal/auth/oidc holds the same rule for the MATCH surface, and
// the two capability/governance write boundaries call it there. The user-drive
// grant is validated in internal/types, and internal/types is imported by
// pkg/client and cmd/wardyn — so reaching for oidc's copy would drag the
// server's OIDC/JOSE dependency stack into the public client library and the CLI
// to canonicalize a string. internal/types is the leaf every one of those
// surfaces already depends on, which makes it the one place the rule can live
// once. oidc.CanonicalGroupSubject should become a delegate to this function;
// until it does, internal/api's TestCanonicalGroupSubjectHasOneAnswer asserts
// the two homes cannot answer differently.
package types

import "strings"

// CanonicalGroupSubject canonicalizes an operator-authored group name into the
// EXACT string a session snapshot carries for a claim of that name, or reports
// ok=false when NO snapshot can ever carry it (empty/whitespace-only, or any
// character outside printable ASCII).
//
// THE ASCII GUARD RUNS ON THE RAW VALUE, BEFORE THE FOLD, and the order is the
// security property. strings.ToLower does Unicode case mapping: KELVIN SIGN
// U+212A folds to ASCII 'k' and U+0130 folds to 'i', so guarding the LOWERED
// value would let a crafted subject "Kubernetes-admins" (U+212A) fold ONTO the
// operator-authored ASCII group "kubernetes-admins" and bind to it — inheriting
// every allocation the real group holds. Fold first, guard second, and the guard
// is decorative.
func CanonicalGroupSubject(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !printableASCII(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// printableASCII reports whether every rune of s is a printable ASCII character
// (U+0020..U+007E).
func printableASCII(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < ' ' || r > '~' }) < 0
}
