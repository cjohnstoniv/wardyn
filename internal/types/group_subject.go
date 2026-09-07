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

import (
	"strings"
	"unicode"
)

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

// CanonicalUserSubject canonicalizes an operator-authored USER subject — an
// OIDC `sub` or an email address — into the form a write boundary stores.
//
// SAME ORDER, DIFFERENT ANSWER FOR NON-ASCII. Like CanonicalGroupSubject it
// looks at the RAW value before folding, and for the same reason: strings.ToLower
// does Unicode case mapping, so KELVIN SIGN U+212A folds to ASCII 'k' and U+0130
// folds to 'i'. An admin who types a look-alike spelling of a real human's
// address would otherwise have it FOLDED ONTO that human's subject and the row
// stored against them — a drive allocated to somebody nobody named, with an
// audit trail showing the exotic string the admin actually typed. Guard first,
// fold second, and there is nothing to fold onto.
//
// It does NOT refuse a non-ASCII value, which is the one place it parts company
// with the group rule, deliberately. A group subject is matched by exact
// equality against the login-time snapshot, which carries printable ASCII only,
// so a non-ASCII group can never match anyone and saying so at the boundary is
// the honest answer. A USER subject is an identity, not an operator-authored
// label: the `sub` claim is whatever the identity provider issues, and refusing
// one because it is not ASCII would refuse a real person. So it is kept exactly
// as typed — unfolded, because folding is precisely what makes a look-alike
// dangerous, and unrefused, because it may be somebody.
//
// The residual, stated rather than implied: internal/api's capabilitySubjects
// folds the session's own subject with a bare strings.ToLower, so a non-ASCII
// subject stored verbatim here is matched against a folded one there and the row
// matches nobody. It is a dead row, not a misdirected one — the direction that
// fails closed — and it is unreachable through the email lane, which oidc's
// deriveEmail already ASCII-guards at login. Closing it belongs on the session
// surface, where the fold is.
func CanonicalUserSubject(s string) string {
	s = strings.TrimSpace(s)
	if !ASCIIOnlySubject(s) {
		return s
	}
	return strings.ToLower(s)
}

// ASCIIOnlySubject reports whether s contains no rune above ASCII. It is the
// same rule oidc.ASCIIOnly states for the login-time match surface, spelled here
// because internal/types is imported by pkg/client and cmd/wardyn and must not
// drag the server's OIDC dependency stack in to answer a question about a
// string. internal/api's TestCanonicalGroupSubjectHasOneAnswer already pins the
// sibling pair against drift; the same idea covers this one.
func ASCIIOnlySubject(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) < 0
}

// printableASCII reports whether every rune of s is a printable ASCII character
// (U+0020..U+007E).
func printableASCII(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < ' ' || r > '~' }) < 0
}
