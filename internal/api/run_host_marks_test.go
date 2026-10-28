// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// TestHostMarksOf: the mark comes from the proxy config dispatch hands the sidecar, by the proxy's
// own rule, so the same allowed hosts read differently as the config changes.
func TestHostMarksOf(t *testing.T) {
	allowed := []string{"api.anthropic.com", "artifacts.corp.example", "*.corp.example", "pypi.org"}
	for _, tc := range []struct {
		name string
		pc   runner.ProxyConfig
		want []string
		term []string
		out  []string
	}{
		{"no CA", runner.ProxyConfig{MITMHosts: []string{"artifacts.corp.example"}, MITMLLM: true}, []string{}, []string{}, []string{}},
		{"model route and an artifact host", runner.ProxyConfig{MITMCACertPEM: "c", MITMCAKeyPEM: "k", MITMHosts: []string{"artifacts.corp.example"}, MITMLLM: true},
			[]string{"api.anthropic.com", "artifacts.corp.example", "*.corp.example"},
			[]string{"api.anthropic.com", "api.openai.com", "artifacts.corp.example"}, []string{"api.openai.com"}},
		{"artifact host only", runner.ProxyConfig{MITMCACertPEM: "c", MITMCAKeyPEM: "k", MITMHosts: []string{"artifacts.corp.example"}},
			[]string{"artifacts.corp.example", "*.corp.example"}, []string{"artifacts.corp.example"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hostMarksOf(tc.pc, allowed)
			if !slices.Equal(got.Inspected, tc.want) {
				t.Errorf("inspected = %v, want %v", got.Inspected, tc.want)
			}
			if got.Terminated == nil || !slices.Equal(got.Terminated, tc.term) {
				t.Errorf("terminated = %#v, want %v", got.Terminated, tc.term)
			}
			if got.Outside == nil || !slices.Equal(got.Outside, tc.out) {
				t.Errorf("outside = %#v, want %v", got.Outside, tc.out)
			}
		})
	}
}
