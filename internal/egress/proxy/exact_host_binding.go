// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The CREDENTIAL-INJECTION host binding — the one policy question that is asked
// without a port, by the one caller that has no port to offer (buildInjector,
// inject.go). It lives beside the matcher rather than inside it because it is a
// different question: evalHost decides what the sandbox may REACH, this decides
// where a secret the sandbox never holds may be ATTACHED. (Split out of
// policy.go, which is at the 1000-line gate.)

// AllowedExactHost reports whether host is allowed via an EXACT allowlist
// entry (not a wildcard, not approval). Credential injection requires this
// stricter match so an injection rule can never widen egress nor leak a
// secret to a wildcard-matched host.
//
// PORT-LESS BY CONTRACT, and it consults allowedExactAnyPort for that reason
// (B10-F1): its one caller (buildInjector) holds a rule host and no port, while
// the producer that authors both halves writes the allowlist entry
// PORT-QUALIFIED ("m.corp:443", F106) and the injection rule BARE. Reading only
// the port-less map made those two contradict, so buildInjector — and therefore
// NewServer, and therefore the sidecar of every run on an estate with a
// corporate artifact mirror — failed closed at boot. An entry the operator
// port-qualified is the same host named in writing; the port clamps that guard
// the CREDENTIAL live where a port exists to check (injectableTransport,
// mitmPortAllowed), and egress itself is unchanged (evalHost never reads this).
//
// DENY STAYS SYMMETRIC WITH ALLOW. The two port-less deny checks below cannot see
// a port-qualified deny (CompilePolicy routes those to deniedExactPort alone), so
// widening the allow side to "any authored port" without widening the deny side
// would have made "allow m.corp:443 + deny m.corp:443" BUILD an injector that the
// port-less-on-both-sides code refused. The port-qualified arm therefore asks the
// only question that is meaningful once ports are in play: did the operator
// author a port for this host that they did not also deny? At least one
// surviving port means the credential has somewhere the operator sanctioned to
// go; none means every authored port was taken back in writing.
func (p *Policy) AllowedExactHost(host string) bool {
	host = canonHost(host)
	if _, ok := p.deniedExact[host]; ok {
		return false
	}
	if matchWild(host, p.deniedWild) {
		return false
	}
	if _, ok := p.allowedExact[host]; ok {
		return true
	}
	for port := range p.allowedExactAnyPort[host] {
		if _, denied := p.deniedExactPort[hostPortKey(host, port)]; !denied {
			return true
		}
	}
	return false
}
