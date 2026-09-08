// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// upstreamAllowAllProxy builds the exact shape F105 is about: a corp upstream
// configured (so egressTarget hands the destination to the corp proxy BY NAME,
// unresolved and unpinned) with allow_all_egress, so nothing but the
// unconditional literal-IP guard stands between the sandbox and the target.
func upstreamAllowAllProxy(t *testing.T) *Proxy {
	t.Helper()
	up, err := parseUpstreamProxy("http://corp-proxy.internal:3128")
	if err != nil {
		t.Fatalf("parseUpstreamProxy: %v", err)
	}
	return newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowAllEgress: true}),
		Sink:     &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
		Resolver: publicResolver{},
		Upstream: up,
	})
}

// TestNonCanonicalLiteralIPIsDeniedLikeItsCanonicalSpelling pins the bound
// THREAT-MODEL.md states for the corp-upstream relaxation: "a literal
// private/loopback/link-local/metadata IP is still denied at the literal-IP
// guard".
//
// net.ParseIP accepts ONLY the canonical dotted-quad, while inet_aton — and
// therefore glibc getaddrinfo, and therefore the corp proxy that actually
// dials — reads 127.1, 0x7f000001 and 2130706433 as 127.0.0.1 and
// 0251.0376.0.1 as 169.254.0.1 (a link-local address, not loopback: the row's
// `is` column carries the pairing so the claim cannot drift). net.ParseIP is
// also nil for a ZONE-SUFFIXED IPv6 literal (fe80::1%eth0, and the RFC 6874
// authority spelling fe80::1%25eth0) that netip.ParseAddr parses and net.Dial
// dials. Every one of those spellings used to walk past step 0 as an ordinary
// "hostname" and be handed to the operator's proxy verbatim.
func TestNonCanonicalLiteralIPIsDeniedLikeItsCanonicalSpelling(t *testing.T) {
	p := upstreamAllowAllProxy(t)

	// is = the address the spelling actually names to the thing that dials, so
	// a wrong pairing (the "0251.0376.0.1 is 127.0.0.1" the docs used to claim)
	// fails here rather than living on in a comment.
	blocked := []struct{ host, is, why string }{
		{"127.1", "127.0.0.1", "dotted-double loopback"},
		{"127.0.1", "127.0.0.1", "dotted-triple loopback"},
		{"0x7f000001", "127.0.0.1", "hexadecimal loopback"},
		{"2130706433", "127.0.0.1", "bare 32-bit loopback"},
		{"0251.0376.0.1", "169.254.0.1", "octal link-local"},
		{"0xa9fea9fe", "169.254.169.254", "hexadecimal cloud metadata"},
		{"2852039166", "169.254.169.254", "bare 32-bit cloud metadata"},
		{"0xa000001", "10.0.0.1", "hexadecimal RFC1918"},
		{"167772161", "10.0.0.1", "bare 32-bit RFC1918"},
		// F105 second axis: net.ParseIP is nil for a zoned IPv6 literal, so the
		// zone id alone used to decide allow-vs-deny for the SAME address.
		{"fe80::1%eth0", "fe80::1", "zone-suffixed IPv6 link-local"},
		{"fe80::1%25eth0", "fe80::1", "percent-encoded zone (RFC 6874 authority form)"},
		{"ff02::1%eth0", "ff02::1", "zone-suffixed IPv6 multicast"},
	}
	for _, tc := range blocked {
		t.Run(tc.host, func(t *testing.T) {
			// The pairing itself, not just "some blocked address": the gap-filler
			// must read this spelling as exactly the address the row names.
			if ip := nonCanonicalLiteralIP(tc.host); ip == nil || ip.String() != tc.is {
				t.Fatalf("nonCanonicalLiteralIP(%q) [%s] = %v, want %s: the doc and the guard "+
					"must agree on WHICH address the spelling names", tc.host, tc.why, ip, tc.is)
			}
			d, target, _ := p.evaluate(context.Background(), tc.host, 443, http.MethodConnect, "")
			if d != egress.Deny {
				t.Fatalf("evaluate(%q) [%s] = %v target=%q, want Deny: every inet_aton spelling of a "+
					"blocked address must be denied at the literal-IP guard, or the corp proxy resolves it for us",
					tc.host, tc.why, d, target)
			}
			if _, _, err := p.egressTarget(tc.host, 443); err == nil {
				t.Fatalf("egressTarget(%q) [%s] returned no error: serveMITMRequest and the two brokers "+
					"reach egressTarget WITHOUT going through evaluate, so the guard has to hold here too",
					tc.host, tc.why)
			}
		})
	}

	// The IPv4-COMPATIBLE ::/96 spellings (RFC 4291 §2.5.5.1) are the same claim
	// on the axis net.ParseIP DOES parse, so no gap-filler is involved: they
	// reach the guard on the CANONICAL path, with To4() == nil (To4 unwraps only
	// ::ffff:/96) and every stdlib predicate answering false. Both guards have to
	// hold — evaluate's step 0 AND VetHost, whose literal fast path calls the
	// same isBlockedIP, so a doc that says "step 0 misses it but VetHost binds
	// it" would be false in both halves.
	for _, tc := range []struct{ host, is, why string }{
		{"::127.0.0.1", "127.0.0.1", "IPv4-compatible loopback"},
		{"::169.254.169.254", "169.254.169.254", "IPv4-compatible cloud metadata"},
		{"::10.0.0.1", "10.0.0.1", "IPv4-compatible RFC1918"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			ip := net.ParseIP(tc.host)
			if ip == nil {
				t.Fatalf("net.ParseIP(%q) = nil — this table is for the spellings it PARSES", tc.host)
			}
			if got := nonCanonicalLiteralIP(tc.host); got != nil {
				t.Fatalf("nonCanonicalLiteralIP(%q) = %s, want nil: the gap-filler fills a gap beside "+
					"net.ParseIP and must not re-decide a spelling net.ParseIP already accepts", tc.host, got)
			}
			if kind, why := isBlockedIP(ip); kind == blockNone {
				t.Fatalf("isBlockedIP(%q) [%s] = blockNone (%q): its low 32 bits ARE %s, which the "+
					"canonical spelling is denied for", tc.host, tc.why, why, tc.is)
			}
			d, target, _ := p.evaluate(context.Background(), tc.host, 443, http.MethodConnect, "")
			if d != egress.Deny {
				t.Fatalf("evaluate(%q) [%s] = %v target=%q, want Deny: bound (3) of the corp-upstream "+
					"relaxation promises every literal spelling the guard PARSES is denied at step 0",
					tc.host, tc.why, d, target)
			}
			if r := VetHost(tc.host, publicResolver{}); !r.Denied {
				t.Fatalf("VetHost(%q) [%s] admitted it (ip=%v): the resolve-based guard takes the LITERAL "+
					"fast path here and calls the same isBlockedIP, so it binds this spelling exactly "+
					"when step 0 does — never instead of it", tc.host, tc.why, r.IP)
			}
			if _, _, err := p.egressTarget(tc.host, 443); err == nil {
				t.Fatalf("egressTarget(%q) [%s] returned no error: serveMITMRequest and the two brokers "+
					"reach egressTarget WITHOUT going through evaluate, so the guard has to hold here too",
					tc.host, tc.why)
			}
		})
	}

	// Canonical control: unchanged behaviour, both spellings of the same address
	// must agree.
	for _, host := range []string{"127.0.0.1", "169.254.169.254", "10.0.0.1", "fe80::1", "ff02::1"} {
		if d, _, _ := p.evaluate(context.Background(), host, 443, http.MethodConnect, ""); d != egress.Deny {
			t.Fatalf("evaluate(%q) = %v, want Deny (canonical control)", host, d)
		}
	}

	// NEGATIVE control — the guard must not over-deny. A non-canonical spelling
	// of a PUBLIC address, and an ordinary hostname, stay reachable.
	for _, host := range []string{
		"134744072" /* 8.8.8.8 */, "0x8080808", /* 8.8.8.8 */
		"2001:4860:4860::8888%eth0", // a PUBLIC address, zoned: still allowed
		"::8.8.8.8",                 // IPv4-compatible spelling of a PUBLIC address: ::/96 is not denied wholesale
		"api.example.com",
	} {
		if d, _, _ := p.evaluate(context.Background(), host, 443, http.MethodConnect, ""); d != egress.Allow {
			t.Fatalf("evaluate(%q) = %v, want Allow: only BLOCKED addresses may be refused by this guard", host, d)
		}
	}
}
