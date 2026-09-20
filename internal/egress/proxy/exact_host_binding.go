// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The credential-injection host binding — the one policy question that is asked
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
// Port-less by contract, and it consults allowedExactAnyPort for that reason
// (B10-F1): its one caller (buildInjector) holds a rule host and no port, while
// the producer that authors both halves writes the allowlist entry
// PORT-QUALIFIED ("m.corp:443", F106) and the injection rule BARE. Reading only
// the port-less map would make those two contradict, so buildInjector — and
// therefore NewServer, and therefore the sidecar of every run on an estate with
// a corporate artifact mirror — would fail closed at boot. An entry the operator
// port-qualified is the same host named in writing; the port clamps that guard
// the CREDENTIAL live where a port exists to check (injectableTransport,
// mitmPortAllowed), and egress itself is unchanged (evalHost never reads this).
//
// Deny stays symmetric with allow. The two port-less deny checks below cannot see
// a port-qualified deny (CompilePolicy routes those to deniedExactPort alone), so
// widening the allow side to "any authored port" without widening the deny side
// would have made "allow m.corp:443 + deny m.corp:443" BUILD an injector that the
// port-less-on-both-sides code refused. The port-qualified arm therefore asks the
// only question that is meaningful once ports are in play: did the operator
// author a port for this host that they did not also deny? At least one
// surviving port means the credential has somewhere the operator sanctioned to
// go; none means every authored port was taken back in writing.
func (p *Policy) AllowedExactHost(host string) bool {
	if p == nil {
		return false
	}
	host = canonHost(host)
	if p.exactHostDenied(host) {
		return false
	}
	if _, ok := p.allowedExact[host]; ok {
		return true
	}
	for port := range p.allowedExactAnyPort[host] {
		if _, denied := p.deniedExactPort[hostPortKey(host, port)]; denied {
			continue
		}
		// This also matches WILDCARD port-qualified denies, exactly as
		// AuthoredPortFor reads them. CompilePolicy routes "*.corp:8443" to
		// deniedWildPort alone, which neither the port-less checks above nor the
		// exact-port lookup on the line before can see — so without this a
		// blanket "deny *.corp:8443" could not cancel the one port the operator
		// authored for m.corp, and the credential would stay bound to a host
		// whose every authored port had been taken back in writing.
		if matchWildPort(host, port, p.deniedWildPort) {
			continue
		}
		return true
	}
	return false
}

// exactHostDenied is the two PORT-LESS deny checks both questions below share:
// a bare deny entry, and a wildcard deny. Neither can see a port-qualified deny
// (CompilePolicy routes those to deniedExactPort/deniedWildPort), which is why
// the any-port arm above shadows those itself.
func (p *Policy) exactHostDenied(host string) bool {
	if _, ok := p.deniedExact[host]; ok {
		return true
	}
	return matchWild(host, p.deniedWild)
}

// AllowedBareExactHost is the STRICTER half of the same question: did the
// operator name this exact host in writing WITHOUT qualifying a port — the
// "silent about the port" entry the cleartext port-80 injection arm has always
// been written under.
//
// AllowedExactHost answers "may a credential bind to this host at all", and
// since B10-F1 a port-qualified-only entry answers yes. That is right for the
// BINDING — the operator named the host — and wrong for the one caller that has
// to read an UNAUTHORED port as consent: injectableTransport's port-80 arm,
// whose whole justification is that a bare entry says nothing about the port,
// so port 80 is the default port of a plaintext connector rather than the
// sandbox choosing a transport. An operator who wrote "vendor.example:8443" DID
// state a port; reading their silence about port 80 as permission inverts it.
// So that arm asks this, and every other port asks AuthoredPortFor.
func (p *Policy) AllowedBareExactHost(host string) bool {
	if p == nil {
		return false
	}
	host = canonHost(host)
	if p.exactHostDenied(host) {
		return false
	}
	_, ok := p.allowedExact[canonHost(host)]
	return ok
}
