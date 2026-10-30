// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
)

// The MITM-eligibility seam: how one Options.MITMHosts entry is read, and what it decides
// about the connection the proxy makes on the far side of a tunnel it terminated.

// parseMITMHostPort normalizes one Options.MITMHosts entry (trim, lowercase, drop a
// trailing dot) and splits its optional ":port" suffix; port==0 means none was given (the
// legacy bare-host format) and matches ANY port, same as a malformed one.
//
// An optional "http://" prefix says the origin speaks plain HTTP, so the MITM's upstream leg
// re-originates in cleartext rather than TLS; everything else is TLS.
//
// Terminating the tunnel is not optional: the sandbox holds a placeholder, not a credential,
// and the proxy must see the request to substitute the real token — a blind tunnel would
// carry the placeholder straight through. A plain-HTTP test SSO fake needs the MITM to
// terminate AND speak http to the origin.
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
			return hostKey(h), p, plaintext
		}
	}
	return hostKey(entry), 0, plaintext
}

// hostKey is the one spelling the proxy's host-keyed tables (MITM eligibility, ports and scheme,
// model channels, injection rules) use at compile time and at every lookup: domainmatch.CanonHost,
// so two spellings of one IP literal (dotted and IPv4-mapped, compressed and expanded IPv6) share
// a key, as they do in the egress policy. A host name is only lower-cased and dot-trimmed.
func hostKey(h string) string { return domainmatch.CanonHost(strings.TrimSpace(h)) }

// mitmPlaintextUpstream reports whether the MITM'd origin for host serves plain HTTP, so
// forwardInspectedLLM re-originates in cleartext instead of TLS. Explicit and positive,
// never derived from the injection rule's require_tls — that bool zero-values to false, so
// inverting it would default to cleartext at real TLS origins.
func (p *Proxy) mitmPlaintextUpstream(host string, port int) bool {
	return p.mitmPlaintext[plaintextKey(hostKey(host), port)]
}

// plaintextKey is the one spelling of the plaintext set's key; a port of 0 is the legacy
// any-port entry, kept separate so it can't be confused with a port-scoped one.
func plaintextKey(host string, port int) string {
	return host + "\x00" + strconv.Itoa(port)
}

// compileMITMHosts turns configured entries into the three lookups the proxy keeps: which
// hosts are MITM-eligible, each entry's scoped port (0 = legacy any-port), and which name a
// cleartext origin.
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
			// Keyed by host:port, not host: scheme is a property of the port-scoped entry,
			// while ports[h] is last-writer-wins, so a host-keyed flag could re-originate a
			// different port's TLS entry in cleartext.
			plaintext[plaintextKey(h, port)] = true
		}
	}
	return hosts, ports, plaintext
}

// mitmTables is the host and port tables a proxy terminates TLS for, with the credential doors
// that add to them.
type mitmTables struct {
	hosts     map[string]bool
	ports     map[string]int
	plaintext map[string]bool
	patAPI    map[string]PATGrant
	ghAPI     *GitHubAPIGrant
	arm       *azureARMDoor
	sigv4     *awsSigV4Door
	kube      *kubeAPIDoor
}

// buildMITMTables compiles the MITMHosts entries, then adds the hosts each credential door
// terminates, in that order. newProxy and Config.mitmSetOf both call it, so the host set a run
// reports is the host set its proxy builds. patGrants is already canonical.
func buildMITMTables(entries []string, patGrants map[string]PATGrant, gh *GitHubAPIGrant, arm *AzureARMGrant, sigv4 *AWSSigV4Config, kube *KubeAPIGrant) mitmTables {
	t := mitmTables{}
	t.hosts, t.ports, t.plaintext = compileMITMHosts(entries)
	// A git_pat grant with api: true terminates its own host on 443 and nothing else, so the
	// forge API is reached only through the door. An entry the operator already authored keeps its own scope.
	t.patAPI = newPATAPIGrants(patGrants, t.hosts, t.ports)
	t.ghAPI = newGitHubAPIDoor(gh, t.hosts, t.ports)
	t.arm = newAzureARMDoor(arm, t.hosts, t.ports)
	t.sigv4 = newAWSSigV4Door(sigv4, t.hosts, t.ports)
	t.kube = newKubeAPIDoor(kube, t.hosts, t.ports)
	return t
}

// upstreamSchemeFor is the scheme and default port forwardInspectedLLM re-originates a
// MITM'd request in: https/443 unless this host's entry said otherwise (only a plain-HTTP
// test SSO fake does; production is unchanged).
//
// Hard-coding https would dial TLS at a server that serves none, failing every CONNECT with
// dial-failed + 502; not terminating the tunnel isn't the alternative, since a blind tunnel
// carries the sandbox's placeholder through untouched.
func (p *Proxy) upstreamSchemeFor(host string, port int) (scheme string, defaultPort int) {
	if p.mitmPlaintextUpstream(host, port) {
		return "http", 80
	}
	if lm := p.localModelAt(host, port); lm != nil && lm.plaintext {
		return "http", 80
	}
	return "https", 443
}
