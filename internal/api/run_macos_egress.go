// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"

	macosguest "github.com/cjohnstoniv/wardyn/deploy/macos-guest"
	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// macOSBootHosts is deploy/macos-guest/boot-hosts.json: the hosts a macOS guest calls in the background at
// boot. A list that cannot be read is empty, which leaves today's behaviour (the hosts are held for
// approval), never a wider one.
var macOSBootHosts = sync.OnceValue(func() []string {
	var doc struct {
		Hosts []string `json:"hosts"`
	}
	if err := json.Unmarshal(macosguest.BootHostsJSON, &doc); err != nil {
		slog.Error("wardynd: the macOS boot-host list cannot be read; its hosts keep first-use approval", slog.Any("err", err))
		return nil
	}
	return doc.Hosts
})

// appendMacOSBootDenials is a macOS VM run's denied domains with Apple's background boot hosts added, so the
// proxy denies them at once and raises no approval for them (a denied host is never a first-use
// candidate). It is a deny list only: it never touches the run's allowed domains, so nothing on it can
// widen the run's egress, and a host an allowed domain already covers (exactly, or under a wildcard) is
// left out, so a person's own allow gets the normal handling. A run that allows all egress asks about
// nothing, so it is left as it is, and so is every run that is not a macOS VM. The result is a new slice;
// the argument is not written through.
func appendMacOSBootDenials(kind types.MachineKind, policy types.RunPolicySpec) []string {
	if !kind.IsMacOSVM() || policy.AllowAllEgress {
		return policy.DeniedDomains
	}
	have := map[string]bool{}
	for _, d := range policy.DeniedDomains {
		have[strings.ToLower(strings.TrimSpace(d))] = true
	}
	out := append([]string(nil), policy.DeniedDomains...)
	for _, host := range macOSBootHosts() {
		if have[host] || allowCovers(policy.AllowedDomains, host) {
			continue
		}
		out = append(out, host)
	}
	return out
}

// allowCovers reports whether any allowed domain entry (an exact host or a "*." wildcard, with or without a
// port) names host or a suffix of it.
func allowCovers(allowed []string, host string) bool {
	host = domainmatch.CanonHost(host)
	for _, entry := range allowed {
		exact, wild, _ := domainmatch.Classify(entry)
		if exact == host || (wild != "" && domainmatch.MatchWild(host, []string{wild})) {
			return true
		}
	}
	return false
}

// activityIgnoreHosts is the hosts whose allowed requests are not the run working: a macOS VM run's boot
// hosts, which its guest calls in the background whatever the policy says, and none for any other kind. A
// copy, since the caller's config is written through. The control plane judges activity by the same list.
// The guest's other background requests skip the run's proxy settings, arrive on the vmnet relay lane and
// never count (egress.Request.Relayed).
func activityIgnoreHosts(kind types.MachineKind) []string {
	if !kind.IsMacOSVM() {
		return nil
	}
	return slices.Clone(macOSBootHosts())
}
