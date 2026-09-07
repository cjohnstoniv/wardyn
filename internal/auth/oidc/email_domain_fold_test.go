// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// email_domain_fold_test.go pins the ORDER of the two operations in
// emailDomainAllowed: guard the raw domain, THEN fold it.
//
// This package makes the same ASCII promise on four surfaces — emailInList
// (the operator allowlist), deriveRole's lookup loop, ParseRoleMap and
// CanonicalGroupSubject — and every one of them guards the RAW value before
// strings.ToLower/EqualFold touches it. The login DOMAIN gate did not: it
// lowered first and never guarded at all, so strings.ToLower's Unicode case
// MAPPING (U+212A KELVIN SIGN -> 'k', U+0130 -> 'i') let a domain the operator
// never wrote fold onto one they did. WARDYN_OIDC_EMAIL_DOMAINS is documented
// EXACT MATCH and is the only gate standing between an attacker-controlled
// tenant's signed id_token and a session, so "exact" has to mean exact.
package oidc_test

import (
	"net/http"
	"testing"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestEmailDomainFoldEscalation is the decision table. The two escalation rows
// are the finding; the rest are the ordinary posture the guard must not break.
//
// Counterfactual: move the printableASCII check after the strings.ToLower in
// emailDomainAllowed (or delete it) and both escalation rows go red.
func TestEmailDomainFoldEscalation(t *testing.T) {
	cases := []struct {
		name    string
		email   string
		allowed []string
		want    bool
	}{
		// THE ESCALATION. Neither domain is on the list; both fold onto one
		// that is. U+212A is the real-world case (a Kelvin sign is a legal
		// character in an IDN label), U+0130 the second, separate mapping —
		// two different runes reaching the same ASCII letter, so a guard that
		// happened to special-case one would still be wrong.
		{"kelvin sign folds onto an allowed domain", "user@\u212Aorp.com", []string{"korp.com"}, false},
		{"dotted capital I folds onto an allowed domain", "user@\u0130nfra.com", []string{"infra.com"}, false},
		// The same rune inside a LONGER label, so the guard cannot be read as
		// "only a leading character matters".
		{"kelvin sign mid-label", "user@bac\u212Aoffice.com", []string{"backoffice.com"}, false},
		// A non-ASCII domain that folds onto NOTHING is still refused — the
		// guard is a property of the value, not of whether a collision exists.
		{"non-ASCII domain with no allowlist twin", "user@k\u00F6rp.com", []string{"korp.com"}, false},
		// A control character is not a domain either; printableASCII (not
		// ASCIIOnly) is the predicate, matching CanonicalGroupSubject.
		{"control character in the domain", "user@korp.com\x00", []string{"korp.com"}, false},

		// NOT AN ESCALATION — every one of these must keep signing in.
		{"exact ASCII match", "alice@korp.com", []string{"korp.com"}, true},
		{"ASCII case-insensitivity still holds (claim side)", "alice@KORP.com", []string{"korp.com"}, true},
		{"ASCII case-insensitivity still holds (list side)", "alice@korp.com", []string{"KORP.COM"}, true},
		{"second entry on the list", "alice@korp.com", []string{"other.example", "korp.com"}, true},
		// A non-ASCII LOCAL part is none of this gate's business: it is
		// discarded at the '@' split and never reaches the comparison, so an
		// internationalized mailbox on an allowed ASCII domain still signs in.
		{"non-ASCII local part on an allowed ASCII domain", "j\u00F6rg@korp.com", []string{"korp.com"}, true},
		// Pre-existing rules, restated so the guard cannot quietly change them.
		{"exact match, not suffix: a subdomain is a different domain", "alice@eng.korp.com", []string{"korp.com"}, false},
		{"no @ at all", "notanemail", []string{"korp.com"}, false},
		{"empty domain cannot match an empty entry", "user@", []string{""}, false},
		{"unrelated domain", "evil@attacker.example", []string{"korp.com"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := writoidc.EmailDomainAllowedForTest(tc.email, tc.allowed); got != tc.want {
				t.Fatalf("emailDomainAllowed(%q, %v) = %v, want %v", tc.email, tc.allowed, got, tc.want)
			}
		})
	}
}

// TestFoldedDomainLoginDenied is the end-to-end half: a real signed id_token
// carrying the look-alike domain, driven through CallbackHandler, must be
// refused with the SAME auth_error an ordinary wrong-domain login gets — and
// must clear any pre-existing session cookie (L6), like every other deny path.
//
// The control leg is the point: one character apart, the same handler, the same
// config — one signs in and the other must not.
func TestFoldedDomainLoginDenied(t *testing.T) {
	const allowed = "korp.com"

	t.Run("the real ASCII domain signs in", func(t *testing.T) {
		env := newIdPEnv(t)
		auth := env.newAuth(t, []string{allowed})
		w, sess := doRoleCallback(t, env, auth, "alice@korp.com", nil, nil)
		if sess.Sub == "" {
			t.Fatalf("control leg did not sign in (status %d, %q) — the escalation leg proves nothing without it",
				w.Code, w.Result().Header.Get("Location"))
		}
	})

	t.Run("the folded look-alike is denied", func(t *testing.T) {
		env := newIdPEnv(t)
		auth := env.newAuth(t, []string{allowed})
		w, sess := doRoleCallback(t, env, auth, "attacker@\u212Aorp.com", nil, nil)
		if sess.Sub != "" {
			t.Fatalf("a login from \\u212aorp.com was issued a session: strings.ToLower folded the crafted domain onto "+
				"the operator-authored allowlist entry %q, so WARDYN_OIDC_EMAIL_DOMAINS admitted a domain nobody listed (role=%q)",
				allowed, sess.Role)
		}
		if w.Code != http.StatusFound {
			t.Fatalf("callback = %d, want 302", w.Code)
		}
		if loc := w.Result().Header.Get("Location"); !containsAuthError(loc, "email_domain") {
			t.Fatalf("Location = %q, want auth_error=email_domain", loc)
		}
		assertSessionCookieCleared(t, w)
	})
}
