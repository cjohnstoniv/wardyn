// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The boot hosts are the shipped list, read once.
func TestMacOSBootHostsAreTheShippedList(t *testing.T) {
	want := []string{"gdmf.apple.com", "mesu.apple.com", "gateway.icloud.com", "init-kt.apple.com"}
	if got := macOSBootHosts(); !slices.Equal(got, want) {
		t.Fatalf("macOSBootHosts() = %q, want %q", got, want)
	}
}

// bootVerdict is the proxy's own policy verdict for host on 443, so these tests judge by what the proxy does.
func bootVerdict(t *testing.T, spec types.RunPolicySpec, host string) egress.HostVerdict {
	t.Helper()
	v, err := proxy.NewBuiltinEvaluator(spec).EvaluateHost(context.Background(), egress.Request{Host: host, Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// permissiveness orders the proxy's verdicts, deny the least.
func permissiveness(v egress.HostVerdict) int {
	switch v {
	case egress.VerdictDeny:
		return 0
	case egress.VerdictUnknown:
		return 1
	}
	return 2
}

// A macOS VM run denies Apple's background hosts quietly: each is denied (the proxy never raises an
// approval for a denied host), its own allow keeps the normal handling, and a developer-facing Apple
// host is untouched and still held for first use.
func TestAppendMacOSBootDenials(t *testing.T) {
	base := types.RunPolicySpec{AllowedDomains: []string{"github.com"}, DeniedDomains: []string{"evil.example"}}
	for _, tc := range []struct {
		name       string
		edit       func(*types.RunPolicySpec)
		wantDenied []string // boot hosts denied after
		wantAllow  []string // boot hosts the policy itself allows, normal handling
	}{
		{name: "no allow covers one", wantDenied: macOSBootHosts()},
		{name: "an exact allow", edit: func(p *types.RunPolicySpec) { p.AllowedDomains = append(p.AllowedDomains, "mesu.apple.com") },
			wantDenied: []string{"gdmf.apple.com", "gateway.icloud.com", "init-kt.apple.com"}, wantAllow: []string{"mesu.apple.com"}},
		{name: "a wildcard allow", edit: func(p *types.RunPolicySpec) { p.AllowedDomains = append(p.AllowedDomains, "*.apple.com") },
			wantDenied: []string{"gateway.icloud.com"}, wantAllow: []string{"gdmf.apple.com", "mesu.apple.com", "init-kt.apple.com"}},
		{name: "a port-qualified allow", edit: func(p *types.RunPolicySpec) { p.AllowedDomains = append(p.AllowedDomains, "gdmf.apple.com:443") },
			wantDenied: []string{"mesu.apple.com", "gateway.icloud.com", "init-kt.apple.com"}, wantAllow: []string{"gdmf.apple.com"}},
		{name: "a wildcard of another domain", edit: func(p *types.RunPolicySpec) { p.AllowedDomains = append(p.AllowedDomains, "*.notapple.com") },
			wantDenied: macOSBootHosts()},
		{name: "already denied", edit: func(p *types.RunPolicySpec) { p.DeniedDomains = append(p.DeniedDomains, "GDMF.apple.com") },
			wantDenied: macOSBootHosts()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			spec.AllowedDomains, spec.DeniedDomains = slices.Clone(base.AllowedDomains), slices.Clone(base.DeniedDomains)
			if tc.edit != nil {
				tc.edit(&spec)
			}
			before := slices.Clone(spec.DeniedDomains)
			spec.DeniedDomains = appendMacOSBootDenials(spec)
			for _, h := range tc.wantDenied {
				if v := bootVerdict(t, spec, h); v != egress.VerdictDeny {
					t.Errorf("%s: verdict %q, want deny (no approval raised)", h, v)
				}
			}
			for _, h := range tc.wantAllow {
				if v := bootVerdict(t, spec, h); v != egress.VerdictAllow {
					t.Errorf("%s: verdict %q, want the policy's own allow", h, v)
				}
			}
			// Everything denied before is denied still, and nothing is listed twice.
			for _, d := range before {
				if !slices.ContainsFunc(spec.DeniedDomains, func(o string) bool { return strings.EqualFold(o, d) }) {
					t.Errorf("denied %q was dropped: %q", d, spec.DeniedDomains)
				}
			}
			seen := map[string]bool{}
			for _, d := range spec.DeniedDomains {
				if seen[strings.ToLower(d)] {
					t.Errorf("%q is denied twice: %q", d, spec.DeniedDomains)
				}
				seen[strings.ToLower(d)] = true
			}
			// A developer-facing Apple host is not on the list: it is still held for first use.
			if v := bootVerdict(t, spec, "gsa.apple.com"); tc.name != "a wildcard allow" && v != egress.VerdictUnknown {
				t.Errorf("gsa.apple.com: verdict %q, want unknown (first-use approval)", v)
			}
		})
	}
}

// An allow-all run asks about nothing, so it is left as it is, and the argument is never written through.
func TestAppendMacOSBootDenials_AllowAllAndNoAliasing(t *testing.T) {
	open := types.RunPolicySpec{AllowAllEgress: true, DeniedDomains: []string{"evil.example"}}
	if got := appendMacOSBootDenials(open); !slices.Equal(got, []string{"evil.example"}) {
		t.Errorf("allow-all: denied = %q, want it unchanged", got)
	}
	backing := make([]string, 1, 16)
	backing[0] = "evil.example"
	spec := types.RunPolicySpec{DeniedDomains: backing}
	_ = appendMacOSBootDenials(spec)
	if backing[:2][1] != "" {
		t.Errorf("the caller's backing array was written through: %q", backing[:2])
	}
}

// THE SECURITY TERM: nothing on the boot-host list ever widens a run's egress. For every shape of policy,
// the function only adds denied domains, leaves the allowed ones alone, and no host (the boot hosts and a
// spread of others) is judged more permissively after than before.
func TestAppendMacOSBootDenials_NeverWidensEgress(t *testing.T) {
	hosts := append([]string{"github.com", "gsa.apple.com", "developer.apple.com", "ocsp.apple.com", "evil.example", "icloud.com", "apple.com"}, macOSBootHosts()...)
	for _, spec := range []types.RunPolicySpec{
		{},
		{AllowedDomains: []string{"github.com", "*.apple.com"}},
		{AllowedDomains: []string{"gateway.icloud.com:443", "*.icloud.com"}, DeniedDomains: []string{"evil.example", "*.bad.example"}},
		{AllowAllEgress: true, DeniedDomains: []string{"evil.example"}},
		{AllowAllEgress: true},
		{DeniedDomains: []string{"apple.com", "*.apple.com"}},
	} {
		allowed := slices.Clone(spec.AllowedDomains)
		after := spec
		after.DeniedDomains = appendMacOSBootDenials(spec)
		if !slices.Equal(after.AllowedDomains, allowed) || after.AllowAllEgress != spec.AllowAllEgress {
			t.Errorf("policy %+v: allowed domains or allow-all changed", spec)
		}
		for _, d := range spec.DeniedDomains {
			if !slices.Contains(after.DeniedDomains, d) {
				t.Errorf("policy %+v: denied %q was removed", spec, d)
			}
		}
		for _, h := range hosts {
			if b, a := permissiveness(bootVerdict(t, spec, h)), permissiveness(bootVerdict(t, after, h)); a > b {
				t.Errorf("policy %+v: %s went from %d to %d, a wider verdict", spec, h, b, a)
			}
		}
	}
}

// Dispatch adds the denials for a macOS VM run and for no other kind: the call sits behind IsMacOSVM
// and writes only DeniedDomains (read from the source, since dispatch itself needs a database).
func TestDispatchDeniesBootHostsOnlyOnMacOSVM(t *testing.T) {
	src, err := os.ReadFile("runs_dispatch.go")
	if err != nil {
		t.Fatal(err)
	}
	const gated = "if run.MachineKind.IsMacOSVM() {\n\t\tpolicy.DeniedDomains = appendMacOSBootDenials(policy)\n\t}"
	if strings.Count(string(src), "appendMacOSBootDenials(") != 1 || !strings.Contains(string(src), gated) {
		t.Fatal("dispatchRun must call appendMacOSBootDenials once, behind run.MachineKind.IsMacOSVM(), assigning policy.DeniedDomains alone")
	}
}
