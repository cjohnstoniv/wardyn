// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"
	"strings"
)

// Channel hosts are model hosts the proxy inspects as one vendor's request schema without ever
// treating them as a gateway for that vendor: an azure_foundry endpoint speaks the Anthropic or
// OpenAI dialect, and its request bodies belong to the content scanner, but it is not a place the
// brokered /wardyn/llm/* routes may dial. So the map feeds isLLMHost and channelForHost and nothing
// else: never gatewayVendor (which enrols a host in the relaxed private-address vet) and never
// llmUpstreams (which would make /wardyn/llm/* forward to the host with no route gate).

// compileChannelHosts lower-cases and dot-trims the configured host keys, like every host lookup.
func compileChannelHosts(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for host, vendor := range m {
		out[strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")] = vendor
	}
	return out
}

// validateChannelHosts refuses a channel host that is not a bare hostname, or whose vendor is not one
// of the two request schemas the scanner reads.
func (c *Config) validateChannelHosts() error {
	for host, vendor := range c.LLMChannelHosts {
		switch {
		case strings.TrimSpace(host) == "" || strings.ContainsAny(host, ":/@ "):
			return fmt.Errorf("config: llm_channel_hosts[%q]: the key must be a bare hostname", host)
		case vendor != anthropicHost && vendor != openaiHost:
			return fmt.Errorf("config: llm_channel_hosts[%q]: vendor %q must be %q or %q", host, vendor, anthropicHost, openaiHost)
		}
	}
	return nil
}
