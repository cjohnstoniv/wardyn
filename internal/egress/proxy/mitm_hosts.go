// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"strconv"
	"strings"
)

// The MITM-eligibility seam: how one Options.MITMHosts entry is read, and
// what the entry decides about the connection the proxy makes on the far
// side of a tunnel it terminated.

// parseMITMHostPort normalizes one Options.MITMHosts entry (trim, lowercase,
// drop a trailing dot) and splits its optional ":port" suffix. port==0 means
// the entry carried none (the legacy bare-host format) and matches ANY port;
// a malformed or out-of-range port suffix is treated the same as absent.
//
// An optional "http://" prefix says the origin behind this entry speaks
// plain HTTP, so the MITM's upstream leg must re-originate in cleartext
// rather than TLS. Everything without a prefix is TLS.
//
// TERMINATING the tunnel is not optional: the sandbox holds a placeholder,
// not a credential, and the proxy has to see the request to substitute the
// real token — a blind tunnel would carry the placeholder straight through.
// So a plain-HTTP test SSO fake needs the MITM to terminate AND speak http
// to the origin; hard-coding https here would dial TLS at a server that
// serves none.
func parseMITMHostPort(entry string) (host string, port int, plaintext bool) {
	entry = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(entry)), ".")
	switch {
	case strings.HasPrefix(entry, "http://"):
		entry, plaintext = strings.TrimPrefix(entry, "http://"), true
	case strings.HasPrefix(entry, "https://"):
		entry = strings.TrimPrefix(entry, "https://")
	}
	if entry == "" {
		return "", 0, false
	}
	if h, ps, err := net.SplitHostPort(entry); err == nil {
		if p, perr := strconv.Atoi(ps); perr == nil && p > 0 && p < 65536 {
			return h, p, plaintext
		}
	}
	return entry, 0, plaintext
}

// mitmPlaintextUpstream reports whether the MITM'd origin for host serves
// plain HTTP, so forwardInspectedLLM re-originates in cleartext instead of
// TLS.
//
// Explicit and positive, never derived from the injection rule's
// require_tls: that Go bool zero-values to false, so inverting it would
// start dialling cleartext at real TLS origins by default.
func (p *Proxy) mitmPlaintextUpstream(host string, port int) bool {
	return p.mitmPlaintext[plaintextKey(strings.ToLower(strings.TrimSuffix(host, ".")), port)]
}

// plaintextKey is the ONE spelling of the plaintext set's key. A port of 0 is
// the legacy any-port entry, which keeps its own key so it cannot be confused
// with a port-scoped one.
func plaintextKey(host string, port int) string {
	return host + "\x00" + strconv.Itoa(port)
}

// compileMITMHosts turns the configured entries into the three lookups the
// proxy keeps: which hosts are MITM-eligible, the port each entry was scoped
// to (0 == the legacy any-port entry), and which ones name a cleartext origin.
func compileMITMHosts(entries []string) (hosts map[string]bool, ports map[string]int, plaintext map[string]bool) {
	hosts = make(map[string]bool, len(entries))
	ports = make(map[string]int, len(entries))
	plaintext = make(map[string]bool, len(entries))
	for _, entry := range entries {
		h, port, plain := parseMITMHostPort(entry)
		if h == "" {
			continue
		}
		hosts[h] = true
		ports[h] = port
		if plain {
			// KEYED BY host:port, not by host: the scheme is a property of the
			// ENTRY (port-scoped), while ports[h] is last-writer-wins, so a
			// host-keyed flag could re-originate a different port's TLS entry
			// in cleartext.
			plaintext[plaintextKey(h, port)] = true
		}
	}
	return hosts, ports, plaintext
}

// upstreamSchemeFor is the scheme and default port forwardInspectedLLM
// re-originates a MITM'd request in: https/443 unless this host's entry said
// otherwise, which only a plain-HTTP test SSO fake does — production is
// byte-for-byte unchanged.
//
// Hard-coding https here would dial TLS at a server that serves none, every
// CONNECT ending builtin:dial-failed + 502. NOT terminating the tunnel is
// not the alternative: a blind tunnel carries the sandbox's placeholder
// through untouched.
func (p *Proxy) upstreamSchemeFor(host string, port int) (scheme string, defaultPort int) {
	if p.mitmPlaintextUpstream(host, port) {
		return "http", 80
	}
	return "https", 443
}
