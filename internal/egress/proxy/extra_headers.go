// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/cjohnstoniv/wardyn/internal/armscope"
	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// Extra gateway headers are the non-secret headers an admin sets on a custom_endpoint provider, for a
// gateway that routes on a header (Portkey reads x-portkey-provider). They are set on every request to
// that provider's gateway host, after the credential, so a sandbox header of the same name is
// overwritten. The default is none, and the rules below keep them from becoming a second credential or
// a way to change how the proxy frames the request: the same rules run when the admin saves the
// provider and when the proxy loads its config.
const (
	// MaxExtraHeaders is how many headers one gateway host takes.
	MaxExtraHeaders = 8
	// MaxExtraHeaderValue is the longest value, in bytes.
	MaxExtraHeaderValue = 2048
)

// framingHeaders are the headers the proxy and the Go transport own: the request's framing, its body
// encoding and its content negotiation.
var framingHeaders = []string{
	"Host", "Content-Length", "Content-Type", "Content-Encoding", "Transfer-Encoding", "Expect",
	"Accept-Encoding", "Range", "If-Range",
}

// credentialNameWords make a name read as a credential. The gateway's credential is the injected one; an
// admin who needs another secret on the wire uses a second integration, not a plain-text header.
var credentialNameWords = []string{"key", "token", "secret", "auth", "password", "cookie", "credential"}

// skKeyValue is the shape of the common "sk-" API keys, which the content scanner's catalog leaves out.
var skKeyValue = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)

// ReservedExtraHeader returns why name may not be an extra gateway header, or nil. own is the credential
// header names that provider and its harnesses send (their auth_header values), which are the
// credential's alone.
func ReservedExtraHeader(name string, own ...string) error {
	if !egress.ValidHeaderName(name) {
		return fmt.Errorf("%q is not a valid HTTP header name", name)
	}
	lower := strings.ToLower(name)
	isListed := func(list []string) bool {
		return slices.ContainsFunc(list, func(n string) bool { return strings.EqualFold(n, name) })
	}
	switch {
	case isListed(strippedCredentialHeaders), lower == "proxy-authorization":
		return fmt.Errorf("%q is a credential header the proxy owns", name)
	case isListed(hopByHopHeaders):
		return fmt.Errorf("%q is a hop-by-hop header", name)
	case isListed(framingHeaders):
		return fmt.Errorf("%q is set by the proxy", name)
	case strings.HasPrefix(lower, "x-wardyn-"):
		return fmt.Errorf("%q is a Wardyn header", name)
	case isListed(own):
		return fmt.Errorf("%q is this provider's own credential header", name)
	case armscope.MethodOverrideHeader(name):
		return fmt.Errorf("%q is reserved: it can make a host run another method than the request line's", name)
	case armscope.RouteOverrideHeader(name):
		return fmt.Errorf("%q is reserved: it can make a host serve another path or host than the request line's", name)
	}
	// "portkey" is a product name, not a key: without it the name of the gateway this exists for
	// (x-portkey-provider) would read as a credential. x-portkey-api-key still does, by "key" in "api-key".
	scan := strings.ReplaceAll(lower, "portkey", "")
	for _, word := range credentialNameWords {
		if strings.Contains(scan, word) {
			return fmt.Errorf("%q reads as a credential (its name contains %q)", name, word)
		}
	}
	return nil
}

// ExtraHeaderValueRefusal returns why value may not be an extra gateway header's value, or nil. The
// reason never carries the value: a pasted secret must not come back in a response or a log.
func ExtraHeaderValueRefusal(value string) error {
	switch {
	case value == "":
		return fmt.Errorf("the value is empty")
	case len(value) > MaxExtraHeaderValue:
		return fmt.Errorf("the value is longer than %d bytes", MaxExtraHeaderValue)
	case !httpguts.ValidHeaderFieldValue(value):
		return fmt.Errorf("the value has characters a header cannot carry")
	case skKeyValue.MatchString(value):
		return fmt.Errorf("the value looks like an API key")
	}
	if format, ok := contentscan.MatchSecretFormat(value); ok {
		return fmt.Errorf("the value looks like a credential (%s)", format)
	}
	return nil
}

// validateLLMHeaders holds Config.LLMHeaders to the rules above and to the gateway hosts LLMUpstreams
// names: headers for any other host would never be sent, which is a config that was not authored here.
func (c *Config) validateLLMHeaders() error {
	for _, host := range slices.Sorted(maps.Keys(c.LLMHeaders)) {
		hs := c.LLMHeaders[host]
		switch {
		case strings.TrimSpace(host) == "" || (strings.ContainsAny(host, ":/@ ") && !bareIPv6(host)):
			return fmt.Errorf("config: llm_headers[%q]: the key must be a bare hostname or a bare IPv6 address", host)
		case !c.isLLMGatewayHost(host):
			return fmt.Errorf("config: llm_headers[%q]: the host is not the host of any llm_upstreams entry", host)
		case len(hs) > MaxExtraHeaders:
			return fmt.Errorf("config: llm_headers[%q]: %d headers, at most %d", host, len(hs), MaxExtraHeaders)
		}
		seen := map[string]bool{}
		for _, name := range slices.Sorted(maps.Keys(hs)) {
			lower := strings.ToLower(name)
			if seen[lower] {
				return fmt.Errorf("config: llm_headers[%q]: header %q is listed twice (names are case-insensitive)", host, lower)
			}
			seen[lower] = true
			if err := ReservedExtraHeader(name); err != nil {
				return fmt.Errorf("config: llm_headers[%q]: %w", host, err)
			}
			if err := ExtraHeaderValueRefusal(hs[name]); err != nil {
				return fmt.Errorf("config: llm_headers[%q]: header %q: %w", host, name, err)
			}
		}
	}
	return nil
}

// isLLMGatewayHost reports whether host is the host of one of c.LLMUpstreams' addresses.
func (c *Config) isLLMGatewayHost(host string) bool {
	want := hostKey(host)
	for _, raw := range c.LLMUpstreams {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" && sameHostIP(hostKey(u.Hostname()), want) {
			return true
		}
	}
	return false
}

// setLLMHeaders keys Config.LLMHeaders by hostKey, like every host lookup. Called once, before the proxy serves.
func (i *injector) setLLMHeaders(m map[string]map[string]string) {
	if len(m) == 0 {
		return
	}
	i.llmHeaders = make(map[string]map[string]string, len(m))
	for host, hs := range m {
		i.llmHeaders[hostKey(host)] = maps.Clone(hs)
	}
}

// llmHeadersFor is the extra headers configured for host; nil when there are none, and on a nil injector.
func (i *injector) llmHeadersFor(host string) map[string]string {
	if i == nil {
		return nil
	}
	return i.llmHeaders[hostKey(host)]
}

// applyExtraHeaders sets host's extra headers on h, last, once the gateway strip and the credential are
// done: a sandbox header of the same name is overwritten. The header the credential rule owns is never
// replaced, whatever a config says, and a host that is no configured gateway's gets nothing.
func (p *Proxy) applyExtraHeaders(h http.Header, host, ownedHeader string) {
	hs := p.inject.llmHeadersFor(host)
	if len(hs) == 0 {
		return
	}
	if _, isGateway := p.gatewayVendorOf(host); !isGateway {
		return
	}
	for name, value := range hs {
		if ownedHeader != "" && strings.EqualFold(name, ownedHeader) {
			continue
		}
		h.Set(name, value)
	}
}
