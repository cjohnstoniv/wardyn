// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/idna"

	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
)

// Destination is one parsed egress entry — "api.example.com",
// "*.example.com", either with ":port" — in the one normal form every
// comparison of a component host against another destination uses: the
// model-provider veto, the injection-collision check, and dispatch. A gate
// parses and vetoes a component's hosts BEFORE ComponentDefinition.Validate,
// so a spelling variant of a vetoed host is refused for what it reaches, not
// for how it is spelled. The parse is the proxy's own (domainmatch.Classify), so a comparison here can
// never disagree with what the proxy will match.
type Destination struct {
	// Host is lower case with no trailing dot; an IP literal is in its
	// canonical spelling. For a wildcard it is the suffix after "*.".
	Host     string
	Wildcard bool
	// Port 0 means any port, as a bare entry does at the proxy.
	Port int
}

// ParseDestination normalises one entry. It refuses only what cannot be
// compared; whether an entry is valid to author is proxy.ValidDomainEntry's
// question, asked first.
func ParseDestination(entry string) (Destination, error) {
	exact, wild, port := domainmatch.Classify(entry)
	d := Destination{Host: exact, Port: port}
	if wild != "" {
		d = Destination{Host: wild[1:], Wildcard: true, Port: port}
	}
	switch {
	case d.Host == "":
		return Destination{}, fmt.Errorf("%q names no host", entry)
	case strings.Contains(d.Host, "*"):
		return Destination{}, fmt.Errorf("%q: a \"*\" is only supported as a leading \"*.\"", entry)
	// Classify leaves a malformed port attached rather than widen to any port.
	case strings.Contains(d.Host, ":") && (d.Wildcard || net.ParseIP(d.Host) == nil):
		return Destination{}, fmt.Errorf("%q: the port must be a number in 1..65535", entry)
	}
	return d, nil
}

// String is the canonical spelling of d; an entry is in normal form when it
// equals the String of its own parse.
func (d Destination) String() string {
	h := d.Host
	if d.Wildcard {
		h = "*." + h
	}
	if d.Port == 0 {
		return h
	}
	return net.JoinHostPort(h, strconv.Itoa(d.Port))
}

// OverlapsAtAnyPort reports whether some host the proxy would match against
// d it would also match against o, whatever the ports. It is the one
// comparison for both component-host vetoes: a destination a model provider
// serves must not be reached on any port, and the proxy keys an injected
// credential by bare host, so a credential on a host collides on every port.
func (d Destination) OverlapsAtAnyPort(o Destination) bool {
	switch {
	case !d.Wildcard && !o.Wildcard:
		return d.Host == o.Host
	case d.Wildcard && o.Wildcard:
		// Both suffixes start at a label boundary, so the longer one lies
		// inside the shorter one's zone or they are disjoint.
		return strings.HasSuffix("."+d.Host, "."+o.Host) || strings.HasSuffix("."+o.Host, "."+d.Host)
	case d.Wildcard:
		return domainmatch.MatchWild(o.Host, []string{"." + d.Host})
	default:
		return domainmatch.MatchWild(d.Host, []string{"." + o.Host})
	}
}

// refuseAddressLiteral refuses a destination that names an address rather
// than a DNS name. The proxy trusts an exact IP in an allowlist as one an
// operator typed, dialing it directly past the private-range guard and any
// corporate upstream, so a person may not author one. A name that only
// RESOLVES to a private address is still caught by that guard at dial time.
//
// Resolvers read far more spellings as an address than net.ParseIP does
// (127.1, 2130706433, 0x7f.1, 0177.0.0.1), so this applies the URL
// standard's rule: a host whose last label is a number is an address.
func refuseAddressLiteral(d Destination) error {
	if net.ParseIP(d.Host) != nil || strings.ContainsAny(d.Host, ":[]%") {
		return errors.New("must be a DNS name, not an IP address")
	}
	labels := strings.Split(d.Host, ".")
	if numericLabel(labels[len(labels)-1]) {
		return errors.New("must be a DNS name, not an IP address (a name ending in a number reads as one)")
	}
	return nil
}

// numericHost reports whether a resolver may read every label of h as a number: an address when
// net.ParseIP reads it, else one in a spelling the proxy refuses in a request (builtin:ip-spelling).
func numericHost(h string) bool {
	return !slices.ContainsFunc(strings.Split(h, "."), func(l string) bool { return !numericLabel(l) })
}

// numericLabel reports whether a resolver may read s as a number: decimal
// (which covers octal), or hex after "0x", including a bare "0x".
func numericLabel(s string) bool {
	digits := "0123456789"
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		s, digits = s[2:], "0123456789abcdefABCDEF"
		if s == "" {
			return true
		}
	}
	return s != "" && strings.Trim(s, digits) == ""
}

// nonASCIIHost reports whether h has a character outside ASCII, the test the proxy's step 0 refuses a
// request host by (builtin:non-ascii-host). Bytes that are not UTF-8 count as outside it.
func nonASCIIHost(h string) bool {
	return strings.ContainsFunc(h, func(r rune) bool { return r > unicode.MaxASCII })
}

// NonASCIIHostWhy is the reason a host written with a character outside ASCII is refused where it is
// written: the proxy refuses every request host with one, so it would match nothing. It names the form to
// write instead where IDNA gives one (ASCIIHostForm).
func NonASCIIHostWhy(host string) string {
	if a, ok := ASCIIHostForm(host); ok {
		return "a host is written in ASCII: its ASCII form is " + a
	}
	return "a host is written in ASCII: a name as its xn-- form, an IP address in standard form"
}

// ASCIIHostForm is the host to write in place of host, which has a character outside ASCII: the form
// IDNA's UTS 46 mapping (idna.Lookup) gives it, when that is a form the proxy takes in a request. It only
// words a refusal: no lookup or match maps a host. A leading "*." is kept.
func ASCIIHostForm(host string) (string, bool) {
	h, wild := strings.CutPrefix(host, "*.")
	// A byte that is not UTF-8 names no character, so nothing maps it. IDNA refuses U+FFFD, which
	// lower-casing turns such a byte into.
	if !utf8.ValidString(h) {
		return "", false
	}
	a, err := idna.Lookup.ToASCII(h)
	a = strings.TrimRight(a, ".")
	if err != nil || a == "" || nonASCIIHost(a) || numericHost(a) && net.ParseIP(a) == nil {
		return "", false
	}
	if wild {
		a = "*." + a
	}
	return a, true
}
