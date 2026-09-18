// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"strconv"
	"strings"
)

// The MITM-ELIGIBILITY SEAM: how one Options.MITMHosts entry is read, and what
// the entry decides about the connection the proxy makes on the far side of a
// tunnel it terminated. Split out of proxy.go/mitm.go/llm_routes.go when the
// scheme arrived and all three crossed the file-size gate; nothing here changed
// meaning in the move.

// parseMITMHostPort normalizes one Options.MITMHosts entry (trim, lowercase,
// drop a trailing dot) and splits its optional ":port" suffix. port==0 means
// the entry carried none — the historical bare-host format, kept only for a
// config written before the port suffix existed — and matches ANY port; a
// malformed or out-of-range port suffix is treated the same as absent rather
// than guessed. No live caller authors a bare entry any more: both
// planArtifactRedirect (W13-S1-5) and authorBedrockBearerInjection (F037) join
// the host to the port they actually configured, so the any-port arm is not a
// default that a new lane can fall into by accident.
// A clean "host:port" (what planArtifactRedirect now authors, W13-S1-5) scopes
// the entry to exactly that port.
//
// AN OPTIONAL "http://" PREFIX says the ORIGIN behind this entry speaks PLAIN
// HTTP, so the MITM's upstream leg must re-originate in cleartext rather than
// TLS. Everything without a prefix is TLS, which is every entry any lane has
// ever authored and every entry a real deployment carries — production is
// byte-for-byte unchanged, and a config written before this existed parses
// identically.
//
// It exists because TERMINATING the tunnel is not optional for Phase B. The
// sandbox holds a placeholder, not a credential; the proxy has to see the
// request to substitute the real token. A blind tunnel would carry the
// placeholder straight through. So for the one deployment shape whose portal is
// a plain-HTTP fake (WARDYN_AWS_SSO_ENDPOINT_OVERRIDE on http://, which already
// refused to boot without WARDYN_ALLOW_TEST_ENDPOINTS), the MITM must terminate
// AND then speak http to the origin. Hard-coding https there dialled TLS at a
// server that serves none: every CONNECT became builtin:dial-failed + 502, no
// role credentials, no model call.
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

// mitmPlaintextUpstream reports whether the MITM'd origin for host serves plain
// HTTP, so forwardInspectedLLM re-originates in cleartext instead of TLS.
//
// EXPLICIT AND POSITIVE, never derived from the injection rule's require_tls:
// that field is a Go bool whose zero value is false, so "does not require TLS"
// silently covers every rule that never set it, and inverting it would have
// started dialling cleartext at real TLS origins. This map holds only what an
// entry actually SAID.
func (p *Proxy) mitmPlaintextUpstream(host string) bool {
	return p.mitmPlaintext[strings.ToLower(strings.TrimSuffix(host, "."))]
}

// compileMITMHosts turns the configured entries into the three lookups the
// proxy keeps: which hosts are MITM-eligible, the port each entry was scoped to
// (0 == the legacy any-port entry), and which ones name a cleartext origin.
//
// A function rather than a loop inside newProxy because the entries now decide
// three things instead of two, and newProxy is at the complexity gate; the
// comments in mitm.go have named this seam for a while.
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
			plaintext[h] = true
		}
	}
	return hosts, ports, plaintext
}

// upstreamSchemeFor is the scheme and default port forwardInspectedLLM
// re-originates a MITM'd request in.
//
// https/443 unless this host's entry said otherwise — a real
// portal.sso.<region>.amazonaws.com is never in that map and neither is any
// corp artifact host, so production is byte-for-byte unchanged. The plain-HTTP
// SSO fake (a deployment that already refused to boot without
// WARDYN_ALLOW_TEST_ENDPOINTS) is the only shape that reaches the other arm.
//
// Hard-coding https here dialled TLS at a server that serves none: every one of
// the SDK's 36-69 CONNECTs per run ended builtin:dial-failed + 502, so no role
// credentials reached the sandbox and every model call starved. NOT terminating
// the tunnel is not the alternative — a blind tunnel carries the sandbox's
// placeholder through untouched, which is Phase B not happening.
func (p *Proxy) upstreamSchemeFor(host string) (scheme string, defaultPort int) {
	if p.mitmPlaintextUpstream(host) {
		return "http", 80
	}
	return "https", 443
}
