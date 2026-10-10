// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The rule for what a GROUP SUBJECT is, and what a USER SUBJECT is, in one
// place: internal/auth/oidc (the login-time match surface), internal/api (the
// write boundaries) and this package's own validators all call in here rather
// than restate it, so a change to identity normalisation is made once.
//
// A group subject matches by exact equality against the printable-ASCII
// login-time snapshot, guarded BEFORE the Unicode fold; anything outside that
// set can never match anyone — a dead deny the console still renders active,
// or a phantom group tier.
//
// Here rather than internal/auth/oidc because this package is the leaf every
// one of those surfaces already imports, while internal/auth/oidc carries the
// OIDC/JOSE stack that pkg/client and cmd/wardyn must not acquire to
// canonicalize a string. One owner, not one per dependency direction.
package types

import (
	"strings"
	"unicode"
)

// CanonicalGroupSubject canonicalizes an operator-authored group name into
// the exact string a session snapshot carries for it, or ok=false when no
// snapshot could ever carry it (empty or non-printable-ASCII).
//
// SECURITY: guards the RAW value before folding — order is the property.
// strings.ToLower's Unicode case mapping (U+212A folds to 'k') would
// otherwise let a crafted look-alike fold onto the real ASCII group and
// inherit its grants.
func CanonicalGroupSubject(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || !PrintableASCII(s) {
		return "", false
	}
	return strings.ToLower(s), true
}

// CanonicalUserSubject canonicalizes an operator-authored USER subject (OIDC
// `sub` or email) into the stored form.
//
// SECURITY: same guard-before-fold order as CanonicalGroupSubject, to stop a
// Unicode look-alike (e.g. U+212A -> 'k') folding onto a real human's subject.
//
// Unlike the group rule it does NOT refuse non-ASCII, deliberately: a `sub`
// is IdP-issued identity, not an operator label, so refusing it would refuse
// a real person — kept unfolded and unrefused. internal/api's own user-subject
// boundaries and the caller's subject list (capabilitySubjects) call this same
// function, so what a caller can BE is exactly what an admin can WRITE. Both
// arms trim whitespace; a non-ASCII string stays unfolded and matches only
// that exact string.
func CanonicalUserSubject(s string) string {
	s = strings.TrimSpace(s)
	if !ASCIIOnlySubject(s) {
		return s
	}
	return strings.ToLower(s)
}

// ASCIIOnlySubject reports whether s contains no rune above ASCII. It answers
// exactly one question — can strings.ToLower move a rune across the ASCII
// boundary — so a control character answers true and this is NOT
// PrintableASCII's rule.
//
// SECURITY: case-insensitive matching (ToLower/EqualFold) does Unicode case
// folding, under which a KELVIN SIGN "k" (U+212A) MATCHES ASCII "k" — so
// guarding the LOWERED value would let a crafted non-ASCII claim fold onto a
// real operator-authored ASCII entry (RoleMap, LegacyAdminEmails, a group
// name) and inherit everything bound to it. The guard must run on the RAW
// value, before the fold, everywhere an ASCII allowlist is compared
// case-insensitively.
func ASCIIOnlySubject(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) < 0
}

// PrintableASCII reports whether every rune of s is a printable ASCII character
// (U+0020..U+007E). Exported for internal/auth/oidc's email-domain allowlist,
// which refuses a claim's domain under the same raw-before-fold rule.
func PrintableASCII(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < ' ' || r > '~' }) < 0
}
