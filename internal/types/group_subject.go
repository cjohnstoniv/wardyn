// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The rule for what a GROUP SUBJECT is, kept here since every write boundary
// already reaches this package.
//
// A group subject matches by exact equality against the printable-ASCII
// login-time snapshot, guarded BEFORE the Unicode fold; anything outside that
// set can never match anyone — a dead deny the console still renders active,
// or a phantom group tier.
//
// Lives here rather than internal/auth/oidc (which states the same rule for
// the match surface) so pkg/client and cmd/wardyn don't need the OIDC/JOSE
// stack; TestCanonicalGroupSubjectHasOneAnswer pins the two copies together.
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
	if s == "" || !printableASCII(s) {
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
// a real person — kept unfolded and unrefused.
//
// Known gap: internal/api's capabilitySubjects folds with a bare ToLower, so
// a non-ASCII subject stored here can end up a dead, not misdirected, row
// there; fixing it belongs on the session surface.
func CanonicalUserSubject(s string) string {
	s = strings.TrimSpace(s)
	if !ASCIIOnlySubject(s) {
		return s
	}
	return strings.ToLower(s)
}

// ASCIIOnlySubject reports whether s has no rune above ASCII — mirrors
// oidc.ASCIIOnly so internal/types needn't import the OIDC stack.
func ASCIIOnlySubject(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) < 0
}

// printableASCII reports whether every rune of s is a printable ASCII character
// (U+0020..U+007E).
func printableASCII(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < ' ' || r > '~' }) < 0
}
