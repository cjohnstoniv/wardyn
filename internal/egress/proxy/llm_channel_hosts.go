// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"net/netip"
	"strings"
)

// Channel hosts are model hosts the proxy inspects as one vendor's request schema without ever
// treating them as a gateway for that vendor: an azure_foundry endpoint speaks the Anthropic or
// OpenAI dialect, and a Claude subscription provider's route-through host speaks the Anthropic one.
// Their request bodies belong to the content scanner, but neither is a place the brokered
// /wardyn/llm/* routes may dial. So the map feeds isLLMHost and channelForHost and nothing
// else: never gatewayVendor (which enrols a host in the relaxed private-address vet) and never
// llmUpstreams (which would make /wardyn/llm/* forward to the host with no route gate).

// compileChannelHosts keys the configured hosts by hostKey, like every host lookup.
func compileChannelHosts(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for host, vendor := range m {
		out[hostKey(host)] = vendor
	}
	return out
}

// validateChannelHosts refuses a channel host that is neither a bare hostname nor a bare IPv6
// address, or whose vendor is not one of the two request schemas the scanner reads.
func (c *Config) validateChannelHosts() error {
	for host, vendor := range c.LLMChannelHosts {
		switch {
		case strings.TrimSpace(host) == "" || (strings.ContainsAny(host, ":/@ ") && !bareIPv6(host)):
			return fmt.Errorf("config: llm_channel_hosts[%q]: the key must be a bare hostname or a bare IPv6 address", host)
		case vendor != anthropicHost && vendor != openaiHost:
			return fmt.Errorf("config: llm_channel_hosts[%q]: vendor %q must be %q or %q", host, vendor, anthropicHost, openaiHost)
		}
	}
	return nil
}

// bareIPv6 reports whether host is an IPv6 address and nothing else: no brackets, no zone and no
// port. That is the spelling a request's host has once splitHostPort has taken "[addr]:port" apart,
// so a key in this form is the key a request to the literal is looked up by. Like a hostname key, it
// is matched as lower-cased text, never as a parsed address: the MITM entry that terminates the same
// host is matched that way, and the two are only ever right together.
func bareIPv6(host string) bool {
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Is6() && addr.Zone() == ""
}
