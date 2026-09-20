// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import "net"

// The charset half of ValidDomainEntry's "reject the dead entry at write time"
// rule (B10-F7), in its own file because policy.go is at the 1000-line gate.
//
// A non-ASCII entry is dead for the same reason a mid-label "*" is. An
// internationalised name travels the wire as PUNYCODE, so no request host can
// ever equal the literal "ëxample.com" an operator typed: the entry is accepted,
// it protects nothing, and on denied_domains it reads as a control that is in
// force. That is precisely the "policy that lies" ValidDomainEntry exists to
// refuse, and it is refused at the write boundary where the operator is looking
// at the text they just wrote.
//
// The charset is the LDH set the operator-promotion door has always applied
// (hostrules.ValidApprovedHost's suggestedHostRE), so the two operator doors now
// agree. Punycode is itself LDH, so the spelling that WORKS is the spelling that
// is accepted.

// DRAFT (M2 canon pending)
//
// charsetWhy is the reason string ValidDomainEntry's error wraps — the sentence
// the operator reads in the 422 when a policy write is refused. It is the ONE
// new user-facing string this lane adds; tests assert through the constant.
const charsetWhy = "must be spelled in ASCII letters, digits, '-' and '.'; " +
	"an internationalised name travels the wire as punycode (xn--…), so type that"

// deadCharsetEntry reports whether a classifyDomain-normalised entry is spelled
// outside the wire charset. IP literals are exempt, as they are from the ":port"
// check beside this one — an IPv6 literal is legitimately full of ':'.
func deadCharsetEntry(exact, wild string) bool {
	if exact != "" && net.ParseIP(exact) == nil && !ldhHost(exact) {
		return true
	}
	return wild != "" && !ldhHost(wild)
}

// ldhHost reports whether h is spelled in the LDH charset — ASCII letters,
// digits, '-' — plus the '.' separators, which is every hostname that can appear
// on the wire (RFC 1123, and punycode by construction). Callers pass a
// classifyDomain-normalised value, so it is already lowercased and dot-trimmed,
// and a wildcard's leading '.' is covered by the same set.
func ldhHost(h string) bool {
	for i := 0; i < len(h); i++ {
		switch c := h[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}
