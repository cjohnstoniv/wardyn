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
	}{
		{"no CA", runner.ProxyConfig{MITMHosts: []string{"artifacts.corp.example"}, MITMLLM: true}, []string{}},
		{"model route and an artifact host", runner.ProxyConfig{MITMCACertPEM: "c", MITMCAKeyPEM: "k", MITMHosts: []string{"artifacts.corp.example"}, MITMLLM: true},
			[]string{"api.anthropic.com", "artifacts.corp.example", "*.corp.example"}},
		{"artifact host only", runner.ProxyConfig{MITMCACertPEM: "c", MITMCAKeyPEM: "k", MITMHosts: []string{"artifacts.corp.example"}},
			[]string{"artifacts.corp.example", "*.corp.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostMarksOf(tc.pc, allowed); !slices.Equal(got.Inspected, tc.want) {
				t.Errorf("inspected = %v, want %v", got.Inspected, tc.want)
			}
		})
	}
}
