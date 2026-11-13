// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
)

// ipSpellingEntryWarning is the warning LoadConfigBytes logs for each config entry refuseIPSpellingEntries
// names. A test asserts on it.
const ipSpellingEntryWarning = "wardyn-proxy: config entry writes an IP address in a non-standard spelling and matches no request; a config that holds one does not load"

// refuseIPSpellingEntries is the load-time half of the write-time refusals of a destination written in a
// non-standard IP spelling (ValidDomainEntry, types.ValidateHostRuleSets, types.MCPURLDestination), for a
// config stored before them: policy.allowed_domains, policy.denied_domains, the live denies a live egress
// edit recorded, the policy's host rule hosts and the MCP server URLs. Step 0 refuses every request written
// that way and the standard spelling is another key, so such an entry matches nothing, and a deny that binds
// nothing is worse than a run that does not start. Each entry is named in a warning, and the config is
// refused with each one's standard form. Only an LDH host is read here: one with a colon, a percent sign or
// another character is the field's own check's. A live deny is read as ValidDomainEntry reads it, so one
// that loads still passes NormalizeLiveDenies, as it did when it was recorded: a live edit relies on that.
func (c *Config) refuseIPSpellingEntries() error {
	var errs []error
	refuse := func(field, entry, host string) {
		h := strings.TrimRight(strings.ToLower(host), ".")
		if !ldhHost(h) || !nonStandardIPSpelling(h) {
			return
		}
		std := ipSpellingStandardForm(h)
		slog.Warn(ipSpellingEntryWarning, slog.String("field", field), slog.String("entry", entry), slog.String("standard_form", std))
		errs = append(errs, fmt.Errorf("config: %s: %q writes an IP address in a non-standard spelling, which the proxy "+
			"refuses in every request, so the entry matches none; %s", field, entry, std))
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
			exact, _, _ := domainmatch.Classify(d)
			refuse(fmt.Sprintf("%s[%d]", l.field, i), d, exact)
		}
	}
	for i, s := range c.Policy.HostRules {
		for j, r := range s.Rules {
			refuse(fmt.Sprintf("policy.host_rules[%d].rules[%d].host", i, j), r.Host, r.Host)
		}
	}
	// An MCP server is named by its URL's host alone: the rest of the URL can carry a secret.
	for i, s := range c.MCPServers {
		if u, err := url.Parse(s.URL); err == nil {
			refuse(fmt.Sprintf("mcp_servers[%d].url", i), u.Host, u.Hostname())
		}
	}
	return errors.Join(errs...)
}

// ipSpellingStandardForm says what an all-numeric host is in standard form: the address inet_aton reads
// (a libc resolver's reading) and, when the host is four decimal parts, the address a decimal reader takes
// (leading zeros dropped). Two different readings are both named, since programs differ on which is meant.
func ipSpellingStandardForm(host string) string {
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
	switch len(forms) {
	case 0:
		return "it has no standard form, so write the address you mean as four decimal numbers from 0 to 255 with no leading zeros"
	case 1:
		return "its standard form is " + forms[0]
	}
	return "its standard form is " + forms[0] + " or " + forms[1] + ", as programs differ on which address it names"
}
