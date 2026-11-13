// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The warnings LoadConfigBytes logs for each config entry refuseIPSpellingEntries names. Tests assert on them.
const (
	// ipSpellingEntryWarning is an all-numeric spelling in an entry the proxy matches requests against.
	ipSpellingEntryWarning = "wardyn-proxy: config entry writes an IP address in a non-standard spelling and matches no request; a config that holds one does not load"
	// configHostEntryWarning is every other entry the rule refuses.
	configHostEntryWarning = "wardyn-proxy: config entry is written in a spelling the proxy refuses in every request; a config that holds one does not load"
)

// hostRefusal is what the config host rule says about a host step 0 would refuse in a request: what is wrong
// with its spelling, and the form to write instead.
type hostRefusal struct {
	numeric bool // an all-numeric spelling
	what    string
	form    string
}

// configHostRefusal reads a host written in config as step 0 reads a request's host (literalIPGuard): a
// character outside ASCII first (nonASCIIHost), then an address in a non-standard spelling, all-numeric or
// with a colon or a percent sign (nonStandardIPSpelling). ok is false for a host step 0 passes. A port is
// taken off first and case is ignored; the verdict is step 0's own, on the host step 0 would see, so load
// passes every spelling step 0 takes. The text reads the host without brackets or trailing dots.
func configHostRefusal(host string) (r hostRefusal, ok bool) {
	h := strings.ToLower(strings.TrimSpace(host))
	if hp, _, err := net.SplitHostPort(h); err == nil {
		h = hp
	}
	n := strings.TrimSuffix(strings.TrimPrefix(strings.TrimRight(trimLiteralHost(h), "."), "["), "]")
	switch {
	case nonASCIIHost(h):
		form := "it has no ASCII form, so write the host you mean in ASCII: a name as its xn-- form, an IP address in standard form"
		if a, ok := types.ASCIIHostForm(n); ok {
			form = "its ASCII form is " + a
		}
		return hostRefusal{what: "has a character outside ASCII", form: form}, true
	case !nonStandardIPSpelling(h):
		return hostRefusal{}, false
	case ldhHost(n):
		return hostRefusal{numeric: true, what: "writes an IP address in a non-standard spelling", form: ipSpellingStandardForm(n)}, true
	}
	return hostRefusal{what: "is written with a colon or a percent sign", form: colonSpellingStandardForm(n)}, true
}

// ValidConfigHost refuses a host written in config that step 0 refuses in every request: a character outside
// ASCII, or an address in a non-standard spelling. It is the write door of every configured host
// refuseIPSpellingEntries reads at load, so a stored host passes both or neither; host may carry a port.
func ValidConfigHost(host string) error {
	if r, ok := configHostRefusal(host); ok {
		return fmt.Errorf("%q %s, which the proxy refuses in every request; %s", host, r.what, r.form)
	}
	return nil
}

// refuseIPSpellingEntries is the load-time half of the write-time refusals of a host written in a spelling
// step 0 refuses in every request (ValidDomainEntry, types.ValidateHostRuleSets, types.MCPURLDestination,
// ValidConfigHost), for a config stored before them. An entry the proxy matches requests against binds
// nothing in such a spelling, since the standard spelling is another key, and a deny that binds nothing is
// worse than a run that does not start. A host a door dials is one programs read differently, and the door
// dials it with no step 0 of its own. Each entry is named in a warning, and the config is refused with each
// one's standard or ASCII form. A live deny is read as ValidDomainEntry reads it, so one that loads still
// passes NormalizeLiveDenies, as it did when it was recorded: a live edit relies on that.
func (c *Config) refuseIPSpellingEntries() error {
	var errs []error
	for _, e := range c.configHosts() {
		r, ok := configHostRefusal(e.host)
		if !ok {
			continue
		}
		consequence, warning := "which the proxy refuses in every request, so the entry matches none", configHostEntryWarning
		if e.dialed {
			consequence = "which programs read differently, so the proxy does not dial it"
		} else if r.numeric {
			warning = ipSpellingEntryWarning
		}
		slog.Warn(warning, slog.String("field", e.field), slog.String("entry", e.entry), slog.String("standard_form", r.form))
		errs = append(errs, fmt.Errorf("config: %s: %q %s, %s; %s", e.field, e.entry, r.what, consequence, r.form))
	}
	return errors.Join(errs...)
}

// configHost is one host a config names: the field as a config path, the entry as the operator reads it,
// the host the rule reads, and whether a door dials it rather than matching requests against it.
type configHost struct {
	field, entry, host string
	dialed             bool
}

// configHosts lists every host this config names for the sandbox's egress, in a fixed order. A URL is named
// by its host alone: the rest can carry a secret. Not listed: the proxy's own control-plane and decision
// sink hops, the runner's hard_deny (built from addresses it reads and its cluster's domain), and
// activity_ignore_hosts (a list built into wardynd).
func (c *Config) configHosts() []configHost {
	var out []configHost
	add := func(field, entry, host string, dialed bool) {
		out = append(out, configHost{field: field, entry: entry, host: host, dialed: dialed})
	}
	list := func(field string, entries []string, dialed bool) {
		for i, e := range entries {
			add(fmt.Sprintf("%s[%d]", field, i), e, e, dialed)
		}
	}
	keys := func(field string, entries []string, dialed bool) {
		for _, e := range slices.Sorted(slices.Values(entries)) {
			add(fmt.Sprintf("%s[%q]", field, e), e, e, dialed)
		}
	}
	urlHost := func(field, raw string, dialed bool) {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			add(field, u.Host, u.Hostname(), dialed)
		}
	}
	for _, l := range []struct {
		field   string
		entries []string
	}{
		{"policy.allowed_domains", c.Policy.AllowedDomains},
		{"policy.denied_domains", c.Policy.DeniedDomains},
		{"live_denies", c.LiveDenies},
	} {
		for i, d := range l.entries {
			add(fmt.Sprintf("%s[%d]", l.field, i), d, domainEntryHost(d), false)
		}
	}
	for i, s := range c.Policy.HostRules {
		for j, r := range s.Rules {
			add(fmt.Sprintf("policy.host_rules[%d].rules[%d].host", i, j), r.Host, r.Host, false)
		}
	}
	for i, s := range c.MCPServers {
		urlHost(fmt.Sprintf("mcp_servers[%d].url", i), s.URL, false)
	}
	for i, inj := range c.Injection {
		add(fmt.Sprintf("injection[%d].host", i), inj.Host, inj.Host, false)
	}
	list("mitm_hosts", c.MITMHosts, false)
	keys("git_push_withheld", slices.Collect(maps.Keys(c.GitPushWithheld)), false)
	keys("pat_grants", slices.Collect(maps.Keys(c.PATGrants)), true)
	if c.ADOGrant != nil {
		list("ado_grant.hosts", c.ADOGrant.Hosts, true)
	}
	for i, g := range c.AzureGates {
		add(fmt.Sprintf("azure_gates[%d].host", i), g.Host, g.Host, false)
	}
	if g := c.AWSSigV4; g != nil {
		add("aws_sigv4.portal_host", g.PortalHost, g.PortalHost, true)
		list("aws_sigv4.hosts", g.Hosts, false)
		for i, gw := range g.Gateways {
			add(fmt.Sprintf("aws_sigv4.gateways[%d].host", i), gw.Host, gw.Host, false)
		}
	}
	if c.KubeAPI != nil {
		add("kube_api.host", c.KubeAPI.Host, c.KubeAPI.Host, false)
	}
	urlHost("upstream_proxy_url", c.UpstreamProxyURL, true)
	for i, e := range c.UpstreamProxyNoProxy {
		if _, _, err := net.ParseCIDR(normalizeNoProxyEntry(e)); err != nil {
			add(fmt.Sprintf("upstream_proxy_no_proxy[%d]", i), e, normalizeNoProxyEntry(e), false)
		}
	}
	for i, h := range c.InternalHosts {
		add(fmt.Sprintf("internal_hosts[%d].host_suffix", i), h.HostSuffix, h.HostSuffix, false)
	}
	for _, v := range slices.Sorted(maps.Keys(c.LLMUpstreams)) {
		urlHost(fmt.Sprintf("llm_upstreams[%q]", v), c.LLMUpstreams[v], true)
	}
	keys("llm_channel_hosts", slices.Collect(maps.Keys(c.LLMChannelHosts)), false)
	keys("llm_headers", slices.Collect(maps.Keys(c.LLMHeaders)), false)
	for _, raw := range slices.Sorted(maps.Keys(c.LocalModels)) {
		urlHost(fmt.Sprintf("local_models[%q]", raw), raw, true)
	}
	keys("bedrock_routes", slices.Collect(maps.Keys(c.BedrockRoutes)), false)
	for i, rt := range c.GatewayIdentity {
		add(fmt.Sprintf("gateway_identity[%d].host", i), rt.Host, rt.Host, false)
	}
	list("forge_hosts", c.ForgeHosts, false)
	for i, rt := range c.ViaOrgRoutes {
		add(fmt.Sprintf("via_org_routes[%d].host", i), rt.Host, rt.Host, false)
	}
	for i, m := range c.ResidentMints {
		if m.Host != "" {
			add(fmt.Sprintf("resident_mints[%d].host", i), m.Host, m.Host, false)
		}
	}
	return out
}

// domainEntryHost is the host a domain list entry names, read as ValidDomainEntry reads it: through
// domainmatch.Classify, with a port it left attached taken off, so "example.com:0" is the port's mistake
// and "010.0.0.1:0" the spelling's. A wildcard keeps its "*.": no address is written that way, so only
// a character outside ASCII refuses one.
func domainEntryHost(d string) string {
	exact, wild, _ := domainmatch.Classify(d)
	if wild != "" {
		exact = "*" + wild
	}
	if h, _, err := net.SplitHostPort(exact); err == nil {
		return h
	}
	return exact
}

// ipSpellingStandardForm says what an all-numeric host is in standard form: the address inet_aton reads
// (a libc resolver's reading) and, when the host is four decimal parts, the address a decimal reader takes
// (leading zeros dropped). Two different readings are both named, since programs differ on which is meant.
func ipSpellingStandardForm(host string) string {
	return standardFormText(ipv4Readings(host),
		"it has no standard form, so write the address you mean as four decimal numbers from 0 to 255 with no leading zeros")
}

// colonSpellingStandardForm says what a host with a colon or a percent sign is in standard form: the address
// with its zone left off (a zone picks an interface, never another address), or, for an IPv4 part in a
// non-standard spelling, the address each reading of that part gives (ipv4Readings).
func colonSpellingStandardForm(host string) string {
	addr, _, _ := strings.Cut(host, "%")
	var forms []string
	if ip := net.ParseIP(addr); ip != nil {
		forms = append(forms, ip.String())
	} else if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		for _, v4 := range ipv4Readings(addr[i+1:]) {
			if ip := net.ParseIP(addr[:i+1] + v4); ip != nil && !slices.Contains(forms, ip.String()) {
				forms = append(forms, ip.String())
			}
		}
	}
	return standardFormText(forms,
		"it has no standard form, so write the address you mean as an IPv6 address with no zone, or as four decimal numbers from 0 to 255 with no leading zeros")
}

// ipv4Readings are the IPv4 addresses programs read an all-numeric host as: inet_aton's, and for four
// decimal parts the decimal one, each once.
func ipv4Readings(host string) []string {
	var forms []string
	if ip := nonCanonicalIPv4(host); ip != nil {
		forms = append(forms, ip.String())
	}
	if parts := strings.Split(host, "."); len(parts) == 4 {
		ip := make(net.IP, 0, net.IPv4len)
		for _, p := range parts {
			if n, err := strconv.ParseUint(p, 10, 8); err == nil {
				ip = append(ip, byte(n))
			}
		}
		if len(ip) == net.IPv4len && !slices.Contains(forms, ip.String()) {
			forms = append(forms, ip.String())
		}
	}
	return forms
}

// standardFormText names the standard forms found, or says there is none.
func standardFormText(forms []string, none string) string {
	switch len(forms) {
	case 0:
		return none
	case 1:
		return "its standard form is " + forms[0]
	}
	return "its standard form is " + forms[0] + " or " + forms[1] + ", as programs differ on which address it names"
}
