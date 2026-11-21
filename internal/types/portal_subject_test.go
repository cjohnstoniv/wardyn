// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestRefuseDelegatePersonSubject(t *testing.T) {
	cases := []struct {
		name, kind, subject string
		refused             bool
	}{
		{"user with the prefix", "user", "delegate:person:abc:def", true},
		{"user with the prefix in upper case", "user", "DELEGATE:PERSON:ABC:DEF", true},
		{"user with the prefix and padding", "user", "  Delegate:Person:abc:def ", true},
		{"group with the prefix", "group", "delegate:person:abc:def", false},
		{"user without the prefix", "user", "alice@example.com", false},
		{"user with a nearby prefix", "user", "delegate:persons:abc", false},
		{"empty user", "user", "", false},
	}
	for _, c := range cases {
		err := RefuseDelegatePersonSubject(c.kind, c.subject)
		if c.refused != errors.Is(err, ErrDelegatePersonNotAllowed) || (!c.refused && err != nil) {
			t.Errorf("%s: err = %v, refused want %v", c.name, err, c.refused)
		}
	}
}

func TestPortalIssuerKey(t *testing.T) {
	k := PortalIssuerKey("https://idp.example.com")
	if !regexp.MustCompile(`^[a-z2-7]{26}$`).MatchString(k) {
		t.Fatalf("key %q is not 26 lowercase base32 characters", k)
	}
	if k != PortalIssuerKey("https://idp.example.com") {
		t.Fatal("the key is not deterministic")
	}
	// The issuer is hashed as written: a trailing slash or a case change is another issuer.
	for _, other := range []string{"https://idp.example.com/", "https://IDP.example.com", "http://idp.example.com"} {
		if PortalIssuerKey(other) == k {
			t.Errorf("%q shares a key with the exact issuer", other)
		}
	}
}

func TestPortalPrincipal(t *testing.T) {
	k := PortalIssuerKey("https://idp.example.com")
	p := PortalPrincipal(k, "00u1AbC")
	if !strings.HasPrefix(p, PortalPersonPrefix+k+":") {
		t.Fatalf("principal %q lacks the prefix and key", p)
	}
	if p != strings.ToLower(p) {
		t.Fatalf("principal %q is not lower case", p)
	}
	if !IsPortalPersonPrincipal(p) {
		t.Fatalf("principal %q is not recognised as a portal person", p)
	}
	if p == PortalPrincipal(k, "00u1abc") {
		t.Error("two ids that differ only in case share a principal")
	}
	if p == PortalPrincipal(PortalIssuerKey("https://idp2.example.com"), "00u1AbC") {
		t.Error("two issuers share a principal")
	}
}

func TestNormIssuerURL(t *testing.T) {
	cases := map[string]string{
		"":                               "",
		"https://Idp.Example.com:443/":   "https://idp.example.com",
		"HTTPS://idp.example.com":        "https://idp.example.com",
		"https://idp.example.com/oauth2": "https://idp.example.com/oauth2",
		"https://idp.example.com:8443/":  "https://idp.example.com:8443",
		"not a url":                      "not a url",
	}
	for in, want := range cases {
		if got := NormIssuerURL(in); got != want {
			t.Errorf("NormIssuerURL(%q) = %q, want %q", in, got, want)
		}
	}
}
