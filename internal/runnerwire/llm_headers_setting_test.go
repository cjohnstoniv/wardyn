// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A run that carries a provider's extra gateway headers names llm_headers, so an older remote runner,
// which would drop them, is refused the run instead of calling the gateway without its routing headers.
func TestProxySettingsSet_NamesLLMHeaders(t *testing.T) {
	if got := ProxySettingsSet(runner.ProxyConfig{}); len(got) != 0 {
		t.Fatalf("an unset config names %v", got)
	}
	pc := runner.ProxyConfig{LLMHeaders: map[string]map[string]string{"gw.corp.example": {"X-Team": "platform"}}}
	if got := ProxySettingsSet(pc); !slices.Equal(got, []string{"llm_headers"}) {
		t.Fatalf("ProxySettingsSet = %v, want [llm_headers]", got)
	}
	if !slices.Contains(ProxySettings, ProxySettingLLMHeaders) {
		t.Fatalf("ProxySettings = %v, want it to list %s", ProxySettings, ProxySettingLLMHeaders)
	}
}
