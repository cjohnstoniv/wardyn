// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"maps"
	"net/textproto"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A custom_endpoint provider can carry non-secret headers the run proxy sets on every request to its
// gateway, for a gateway that routes on a header (Portkey's x-portkey-provider). The default is none, and
// a change removes every person's stored credential, as a base_url change does. The name and value rules
// live with the proxy (proxy.ReservedExtraHeader, proxy.ExtraHeaderValueRefusal), which runs the same
// rules when it loads a run's config.
const (
	mp400HeadersUnused = "model_providers: %q: extra_headers applies only to a custom_endpoint provider"
	mp400HeadersMany   = "model_providers: %q: extra_headers: %d headers, at most %d"
	mp400HeaderDup     = "model_providers: %q: extra_headers: %q is listed twice (names are case-insensitive)"
	mp400HeaderName    = "model_providers: %q: extra_headers: %w"
	mp400HeaderValue   = "model_providers: %q: extra_headers: header %q: %w"
)

// normalizeProviderHeaders puts each name in its canonical case and trims each value, so one header has
// one spelling; no headers is nil, the same as absent.
func normalizeProviderHeaders(mp *types.ModelProvider) {
	for i := range mp.ExtraHeaders {
		h := &mp.ExtraHeaders[i]
		h.Name, h.Value = textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(h.Name)), strings.TrimSpace(h.Value)
	}
	if len(mp.ExtraHeaders) == 0 {
		mp.ExtraHeaders = nil
	}
}

// validateProviderHeaders holds a provider's extra headers to the proxy's name and value rules. No
// refusal carries a value: the field is non-secret, and a pasted secret must not come back in a response.
func validateProviderHeaders(mp types.ModelProvider) error {
	if len(mp.ExtraHeaders) == 0 {
		return nil
	}
	if mp.Kind != types.ModelProviderCustomEndpoint {
		return fmt.Errorf(mp400HeadersUnused, mp.ID)
	}
	if len(mp.ExtraHeaders) > proxy.MaxExtraHeaders {
		return fmt.Errorf(mp400HeadersMany, mp.ID, len(mp.ExtraHeaders), proxy.MaxExtraHeaders)
	}
	var own []string // the credential headers this provider sends: the credential's alone
	if mp.Auth != nil {
		own = append(own, mp.Auth.Header)
	}
	for _, h := range mp.Harnesses {
		own = append(own, h.AuthHeader)
	}
	seen := map[string]bool{}
	for _, h := range mp.ExtraHeaders {
		lower := strings.ToLower(h.Name)
		if seen[lower] {
			return fmt.Errorf(mp400HeaderDup, mp.ID, h.Name)
		}
		seen[lower] = true
		if err := proxy.ReservedExtraHeader(h.Name, own...); err != nil {
			return fmt.Errorf(mp400HeaderName, mp.ID, err)
		}
		if err := proxy.ExtraHeaderValueRefusal(h.Value); err != nil {
			return fmt.Errorf(mp400HeaderValue, mp.ID, h.Name, err)
		}
	}
	return nil
}

// extraHeadersAddress is a provider's extra headers as one string, in name order, for the address rule: a
// header changes where, and how, a person's credential is received.
func extraHeadersAddress(p types.ModelProvider) string {
	hs := slices.SortedFunc(slices.Values(p.ExtraHeaders), func(a, b types.ProviderHeader) int { return strings.Compare(a.Name, b.Name) })
	var b strings.Builder
	for _, h := range hs {
		b.WriteString(h.Name + ":" + h.Value + "\n")
	}
	return b.String()
}

// extraHeaderAuditNames is each provider's header names by provider id, for the audit row; never a value.
// Nil when no provider has any.
func extraHeaderAuditNames(block types.ModelProviders) map[string][]string {
	var out map[string][]string
	for _, p := range block.Providers {
		if len(p.ExtraHeaders) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		for _, h := range p.ExtraHeaders {
			out[p.ID] = append(out[p.ID], h.Name)
		}
		slices.Sort(out[p.ID])
	}
	return out
}

// addExtraHeaders records lane l's provider's extra headers under its gateway host, for
// ProxyConfig.LLMHeaders. Only a custom_endpoint lane that goes to its own address has any; every other
// lane adds nothing, so a run's config carries no key unless its provider has headers. One provider holds
// a host in a run (laneConflict), so a host never has two header sets.
func (d *providerDispatch) addExtraHeaders(l providerKeyLane) {
	if l.provider.Kind != types.ModelProviderCustomEndpoint || len(l.provider.ExtraHeaders) == 0 || l.upstream == "" {
		return
	}
	hs := make(map[string]string, len(l.provider.ExtraHeaders))
	for _, h := range l.provider.ExtraHeaders {
		hs[h.Name] = h.Value
	}
	if d.extraHeaders == nil {
		d.extraHeaders = map[string]map[string]string{}
	}
	d.extraHeaders[strings.ToLower(l.host)] = maps.Clone(hs)
}
