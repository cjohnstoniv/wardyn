// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"testing"
)

// TestBlockedRangesMatchPreExtractionLists pins isBlockedIP to the exact CIDR
// set this file carried as inline tables before they moved onto internal/ipguard
// + the net.IP predicates — one representative address per range, so a removed
// literal that is no longer covered fails HERE.
func TestBlockedRangesMatchPreExtractionLists(t *testing.T) {
	blocked := []string{
		"10.0.0.1", "172.16.5.4", "192.168.1.1", // RFC1918
		"127.0.0.1",                       // 127.0.0.0/8 loopback
		"169.254.169.254",                 // 169.254.0.0/16 link-local + metadata
		"100.64.0.1",                      // CGNAT
		"0.1.2.3",                         // 0.0.0.0/8
		"192.0.0.1",                       // IETF protocol assignments
		"198.19.0.1",                      // benchmarking
		"255.255.255.255",                 // limited broadcast
		"::1", "fc00::1", "fe80::1", "::", // v6 loopback / ULA / link-local / unspecified
		"::ffff:127.0.0.1", // IPv4-mapped loopback must not smuggle through
		// The DEPRECATED IPv4-COMPATIBLE form (::a.b.c.d, RFC 4291 §2.5.5.1).
		// net.ParseIP PARSES these, so unlike the inet_aton spellings there is
		// nothing for literal_ip_guard.go's gap-filler to fill — they arrive on
		// the CANONICAL path with To4() == nil (To4 unwraps only ::ffff:/96), so
		// every stdlib predicate answered false and they were admitted at step 0
		// AND by VetHost's literal fast path.
		"::127.0.0.1",       // loopback, IPv4-compatible spelling
		"::169.254.169.254", // cloud metadata, IPv4-compatible spelling
		"::10.0.0.1",        // RFC1918, IPv4-compatible spelling
		"2002:7f00:1::1",    // 6to4 loopback — denied wholesale via ReservedV6
	}
	for _, s := range blocked {
		if kind, _ := isBlockedIP(net.ParseIP(s)); kind == blockNone {
			t.Errorf("isBlockedIP(%s) = blockNone, want blocked", s)
		}
	}
	// ...and the guard must not over-deny: ::/96 is not blocked wholesale, only
	// the embedded address decides, so an IPv4-compatible spelling of a PUBLIC
	// address stays reachable exactly as its canonical spelling does.
	for _, s := range []string{"8.8.8.8", "93.184.216.34", "2606:4700:4700::1111", "::8.8.8.8"} {
		if kind, why := isBlockedIP(net.ParseIP(s)); kind != blockNone {
			t.Errorf("isBlockedIP(%s) = %v (%s), want allowed", s, kind, why)
		}
	}
}

// TestBlockedRangesCoverNonCanonicalSpellings is the same table asked in the
// spelling the RESOLVER accepts and net.ParseIP does not (F143). Every case
// above reaches isBlockedIP through net.ParseIP, so the whole table only ever
// spoke about the canonical dotted-quad: `grep -rn '127\.1|0x7f000001|2130706433|0177\.'`
// over the tree returned nothing, test or source. inet_aton(3) — and therefore
// glibc getaddrinfo, and therefore the corp proxy that finally dials a host we
// hand over BY NAME — reads all four spellings as the same blocked address.
//
// This pins the composition evaluate's step 0 relies on (literalIPGuard ->
// nonCanonicalIPv4 -> isBlockedIP): a blocked range stays blocked in every
// spelling, and the gap-filler stays a gap-filler (nil for a canonical literal,
// nil for an ordinary hostname), so it can never change what the canonical path
// decides.
func TestBlockedRangesCoverNonCanonicalSpellings(t *testing.T) {
	for _, tc := range []struct{ host, canonical, why string }{
		{"127.1", "127.0.0.1", "dotted-double loopback"},
		{"127.0.1", "127.0.0.1", "dotted-triple loopback"},
		{"0x7f000001", "127.0.0.1", "hexadecimal loopback"},
		{"2130706433", "127.0.0.1", "bare 32-bit loopback"},
		{"0177.0.0.1", "127.0.0.1", "octal loopback"},
		{"0xa9fea9fe", "169.254.169.254", "hexadecimal cloud metadata"},
		{"2852039166", "169.254.169.254", "bare 32-bit cloud metadata"},
		{"0251.0376.0.1", "169.254.0.1", "octal link-local"},
		{"0xa000001", "10.0.0.1", "hexadecimal RFC1918"},
		{"0xac100504", "172.16.5.4", "hexadecimal RFC1918 /12"},
		{"0xc0a80101", "192.168.1.1", "hexadecimal RFC1918 /16"},
		{"0x64400001", "100.64.0.1", "hexadecimal CGNAT"},
	} {
		if net.ParseIP(tc.host) != nil {
			t.Errorf("%s: net.ParseIP(%q) parsed it — this table is for the spellings it REFUSES", tc.why, tc.host)
			continue
		}
		ip := nonCanonicalIPv4(tc.host)
		if ip == nil {
			t.Errorf("nonCanonicalIPv4(%q) = nil (%s): the resolver behind a corp proxy reads it as %s",
				tc.host, tc.why, tc.canonical)
			continue
		}
		if ip.String() != tc.canonical {
			t.Errorf("nonCanonicalIPv4(%q) = %s, want %s (%s)", tc.host, ip, tc.canonical, tc.why)
			continue
		}
		if kind, _ := isBlockedIP(ip); kind == blockNone {
			t.Errorf("isBlockedIP(%s from %q) = blockNone, want blocked (%s)", ip, tc.host, tc.why)
		}
	}

	// The gap-filler must stay a gap-filler: nil for everything net.ParseIP
	// already handles and for anything that is not a numeric literal at all, so
	// it can neither re-decide a canonical address nor turn a hostname into one.
	for _, s := range []string{"127.0.0.1", "8.8.8.8", "::1", "example.com", "0x", "999.1", "1.2.3.4.5", ""} {
		if ip := nonCanonicalIPv4(s); ip != nil {
			t.Errorf("nonCanonicalIPv4(%q) = %s, want nil (only the spellings net.ParseIP refuses)", s, ip)
		}
	}

	// ...and a non-canonical spelling of a PUBLIC address is not blocked: the
	// guard must not over-deny either.
	for _, s := range []string{"134744072" /* 8.8.8.8 */, "0x8080808" /* 8.8.8.8 */} {
		ip := nonCanonicalIPv4(s)
		if ip == nil {
			t.Errorf("nonCanonicalIPv4(%q) = nil, want 8.8.8.8", s)
			continue
		}
		if kind, why := isBlockedIP(ip); kind != blockNone {
			t.Errorf("isBlockedIP(%s from %q) = %v (%s), want allowed", ip, s, kind, why)
		}
	}
}
