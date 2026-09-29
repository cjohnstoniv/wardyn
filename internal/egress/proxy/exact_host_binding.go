// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The credential-injection host binding — the one policy question asked
// without a port, by the one caller that has no port to offer (buildInjector).
// It lives beside the matcher rather than inside it: evalHost decides what
// the sandbox may REACH, this decides where a secret the sandbox never holds
// may be ATTACHED.

// AllowedExactHost reports whether host is allowed via an EXACT allowlist
// entry (not a wildcard, not approval). SECURITY: credential injection
// requires this stricter match so an injection rule can never widen egress
// nor leak a secret to a wildcard-matched host.
//
// Port-less by contract: its caller holds a rule host and no port, while the
// allowlist entry may be authored PORT-QUALIFIED ("m.corp:443"). Reading only
// the port-less map would make the two contradict and fail NewServer closed
// at boot on any estate with a corporate artifact mirror, so this also
// consults allowedExactAnyPort: did the operator author a port for this host
// that they did not also deny? At least one surviving port means the
// credential has somewhere sanctioned to go; none means every authored port
// was taken back in writing.
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
		// Also matches WILDCARD port-qualified denies (invisible to the checks
		// above), so a blanket wildcard deny can still cancel an authored port.
		if matchWildPort(host, port, p.deniedWildPort) {
			continue
		}
		return true
	}
	return false
}

// exactHostDenied is the two PORT-LESS deny checks both questions below
// share: a bare deny entry, and a wildcard deny. Neither can see a
// port-qualified deny, which is why the any-port arm above shadows it itself.
func (p *Policy) exactHostDenied(host string) bool {
	if _, ok := p.deniedExact[host]; ok {
		return true
	}
	return matchWild(host, p.deniedWild)
}

// AllowedBareExactHost is the STRICTER half of the same question: did the
// operator name this exact host in writing WITHOUT qualifying a port — the
// entry the cleartext port-80 injection arm has always been written under.
//
// AllowedExactHost answers "may a credential bind to this host at all", and a
// port-qualified-only entry answers yes there. That's right for the BINDING,
// but wrong for injectableTransport's port-80 arm, which needs an UNAUTHORED
// port to read as consent: an operator who wrote "vendor.example:8443" DID
// state a port, so silence about port 80 must not read as permission.
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
