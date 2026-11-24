// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestHostMarksOf: the mark comes from the proxy config dispatch hands the sidecar, by the proxy's
// own rule, so the same allowed hosts read differently as the config changes.
func TestHostMarksOf(t *testing.T) {
	allowed := []string{"api.anthropic.com", "artifacts.corp.example", "*.corp.example", "pypi.org"}
	listed := types.RunPolicySpec{AllowedDomains: allowed}
	openWide := types.RunPolicySpec{AllowedDomains: allowed, AllowAllEgress: true}
	for _, tc := range []struct {
		name string
		pc   runner.ProxyConfig
		want []string
		part map[string][]string
		term []string
		out  []string
	}{
		{"no CA", runner.ProxyConfig{MITMHosts: []string{"artifacts.corp.example"}, MITMLLM: true}, []string{}, nil, []string{}, []string{}},
		{"model route and an artifact host", runner.ProxyConfig{MITMCACertPEM: "c", MITMCAKeyPEM: "k", MITMHosts: []string{"artifacts.corp.example"}, MITMLLM: true, Policy: listed},
			[]string{"api.anthropic.com", "artifacts.corp.example"}, map[string][]string{"*.corp.example": {"artifacts.corp.example"}},
			[]string{"api.anthropic.com", "api.openai.com", "artifacts.corp.example"}, []string{}},
		{"allow-all lists the model route the allowlist lacks", runner.ProxyConfig{MITMCACertPEM: "c", MITMCAKeyPEM: "k", MITMHosts: []string{"artifacts.corp.example"}, MITMLLM: true, Policy: openWide},
			[]string{"api.anthropic.com", "artifacts.corp.example"}, map[string][]string{"*.corp.example": {"artifacts.corp.example"}},
			[]string{"api.anthropic.com", "api.openai.com", "artifacts.corp.example"}, []string{"api.anthropic.com", "api.openai.com", "artifacts.corp.example"}},
		{"allow-all lists a door host the allowlist names", runner.ProxyConfig{MITMCACertPEM: "c", MITMCAKeyPEM: "k", GitHubAPI: &proxy.GitHubAPIGrant{Grants: map[string]uuid.UUID{"o": uuid.New()}},
			Policy: types.RunPolicySpec{AllowedDomains: []string{"api.github.com:443"}, AllowAllEgress: true}},
			[]string{"api.github.com:443"}, nil, []string{"api.github.com:443"}, []string{"api.github.com:443"}},
		{"artifact host only", runner.ProxyConfig{MITMCACertPEM: "c", MITMCAKeyPEM: "k", MITMHosts: []string{"artifacts.corp.example"}, Policy: listed},
			[]string{"artifacts.corp.example"}, map[string][]string{"*.corp.example": {"artifacts.corp.example"}}, []string{"artifacts.corp.example"}, []string{}},
		// One rendered policy holds an opened host and a tunneled sibling under the same wildcard, and a host
		// opened on one port only: both are partial, never Inspected, and name what is opened.
		{"a wildcard with a tunneled sibling, and a host opened on one port", runner.ProxyConfig{MITMCACertPEM: "c", MITMCAKeyPEM: "k", MITMHosts: []string{"artifacts.corp.example", "git.corp.example:8443"},
			Policy: types.RunPolicySpec{AllowedDomains: []string{"artifacts.corp.example", "build.corp.example", "*.corp.example", "git.corp.example", "pypi.org"}}},
			[]string{"artifacts.corp.example"}, map[string][]string{"*.corp.example": {"artifacts.corp.example", "git.corp.example:8443"}, "git.corp.example": {"git.corp.example:8443"}},
			[]string{"artifacts.corp.example", "git.corp.example:8443"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			named := allowed
			if tc.pc.Policy.AllowedDomains != nil {
				named = tc.pc.Policy.AllowedDomains
			}
			got := hostMarksOf(tc.pc, named)
			if !slices.Equal(got.Inspected, tc.want) {
				t.Errorf("inspected = %v, want %v", got.Inspected, tc.want)
			}
			if !maps.EqualFunc(got.Partial, tc.part, slices.Equal[[]string]) {
				t.Errorf("partial = %v, want %v", got.Partial, tc.part)
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

// TestScopedHostMarks: an envelope from before partial claimed a wildcard or a host with no port was Inspected
// on any overlap. Read back, it names what its terminated list shows is opened, says "some, not known which"
// where that list cannot place the entry, and leaves a host and port, or an envelope with partial, as it is.
func TestScopedHostMarks(t *testing.T) {
	legacy := &hostMarks{
		Inspected:  []string{"*.corp.example", "*.corp.example:443", "git.corp.example", "artifacts.corp.example", "pkg.corp.example:8443", "unlisted.example", "*.unlisted.example"},
		Terminated: []string{"artifacts.corp.example:8443", "git.corp.example", "pkg.corp.example:8443", "[fd00::1]:6443"},
		Outside:    []string{},
	}
	got := scopedHostMarks(legacy)
	if want := []string{"git.corp.example", "pkg.corp.example:8443"}; !slices.Equal(got.Inspected, want) {
		t.Errorf("inspected = %v, want %v", got.Inspected, want)
	}
	for entry, want := range map[string][]string{
		"*.corp.example":         {"artifacts.corp.example:8443", "git.corp.example", "pkg.corp.example:8443"},
		"*.corp.example:443":     {"git.corp.example"},
		"artifacts.corp.example": {"artifacts.corp.example:8443"},
		"unlisted.example":       {},
		"*.unlisted.example":     {},
	} {
		if sub, ok := got.Partial[entry]; !ok || sub == nil || !slices.Equal(sub, want) {
			t.Errorf("partial[%q] = %#v (present %v), want %v", entry, sub, ok, want)
		}
	}
	if len(legacy.Inspected) != 7 || legacy.Partial != nil {
		t.Error("the stored envelope was changed")
	}
	if again := scopedHostMarks(got); again != got {
		t.Error("an envelope that has partial was changed")
	}
	if scopedHostMarks(nil) != nil {
		t.Error("nil marks became something")
	}
}
