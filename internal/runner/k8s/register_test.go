// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"os"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// TestBuildConfig_RecordFollowsDeps is the #1113 registration-level pin:
// buildConfig — the ONLY place init()'s registered "k8s" constructor builds a
// Config from — must carry Deps.Record through to Config.Record verbatim.
// Deps.Record is itself derived from the boot recording-store selection
// (substrate.RecordEnabled, wired in cmd/wardynd's buildRunnerFromFlags), so
// this is computed from a store name rather than a literal bool: "off" must
// not record, "pg"/"fs" must.
//
// COUNTERFACTUAL: hardcode `Record: true` in buildConfig (reverting the
// #1113 fix) and this test goes red for the "off" case.
func TestBuildConfig_RecordFollowsDeps(t *testing.T) {
	for _, store := range []string{"off", "pg", "fs"} {
		want := substrate.RecordEnabled(store)
		cfg := buildConfig(substrate.Deps{Record: want})
		if cfg.Record != want {
			t.Errorf("store %q: buildConfig(Deps{Record: %v}).Record = %v, want %v", store, want, cfg.Record, want)
		}
	}
}

// TestBuildConfig_SandboxPlacementFromEnv: buildConfig carries WARDYN_K8S_SANDBOX_PLACEMENT through
// to Config.SandboxPlacement verbatim.
//
// COUNTERFACTUAL: never read the env var in buildConfig and the "set" case goes red.
func TestBuildConfig_SandboxPlacementFromEnv(t *testing.T) {
	for _, want := range []string{"", `{"nodeSelector":{"pool":"sandbox"}}`} {
		t.Setenv("WARDYN_K8S_SANDBOX_PLACEMENT", want)
		if got := buildConfig(substrate.Deps{}).SandboxPlacement; got != want {
			t.Errorf("SandboxPlacement = %q, want %q", got, want)
		}
	}
}

// TestBuildConfig_ImagePullSecretFromEnv is T-47 (#707): buildConfig — the
// ONLY place init()'s registered "k8s" constructor builds a Config from —
// must carry WARDYN_K8S_IMAGE_PULL_SECRET through to Config.ImagePullSecret
// verbatim.
//
// COUNTERFACTUAL: hardcode ImagePullSecret to "" in buildConfig (never reading
// the env var) and the "set" case goes red.
func TestBuildConfig_ImagePullSecretFromEnv(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", ""},
		{"regcred", "regcred"},
	} {
		t.Setenv("WARDYN_K8S_IMAGE_PULL_SECRET", tc.env)
		cfg := buildConfig(substrate.Deps{})
		if cfg.ImagePullSecret != tc.want {
			t.Errorf("WARDYN_K8S_IMAGE_PULL_SECRET=%q: ImagePullSecret = %q, want %q", tc.env, cfg.ImagePullSecret, tc.want)
		}
	}
}

// TestBuildConfig_AckAmbientDefaultDenyFromEnv is T-47 (#707): buildConfig
// must read WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY (canary.go's B1 override, see
// runEgressCanary) through cliutil.EnvBool, not a literal "1" compare — unset
// stays fail-closed (false). Same for its sibling
// WARDYN_K8S_ALLOW_UNENFORCED_NETPOL. cliutil's own TestEnvBool covers the
// garbage-value-exits-2 branch; EnvBool's fatal path exits the process via an
// unexported package var this test cannot reach.
//
// COUNTERFACTUAL: hardcode either field to false in buildConfig (never reading
// its env var) and that field's "true" case goes red.
func TestBuildConfig_AckAmbientDefaultDenyFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"", false},
		{"true", true},
		{"1", true},
	} {
		t.Setenv("WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY", tc.env)
		cfg := buildConfig(substrate.Deps{})
		if cfg.AckAmbientDefaultDeny != tc.want {
			t.Errorf("WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY=%q: AckAmbientDefaultDeny = %v, want %v", tc.env, cfg.AckAmbientDefaultDeny, tc.want)
		}
	}
}

func TestBuildConfig_AllowUnenforcedNetPolFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"", false},
		{"true", true},
		{"1", true},
	} {
		t.Setenv("WARDYN_K8S_ALLOW_UNENFORCED_NETPOL", tc.env)
		cfg := buildConfig(substrate.Deps{})
		if cfg.AllowUnenforcedNetPol != tc.want {
			t.Errorf("WARDYN_K8S_ALLOW_UNENFORCED_NETPOL=%q: AllowUnenforcedNetPol = %v, want %v", tc.env, cfg.AllowUnenforcedNetPol, tc.want)
		}
	}
}

// TestBuildConfig_NamespaceFromEnv is T-47 (#707): buildConfig resolves
// Namespace through resolveNamespace(os.Getenv("WARDYN_K8S_NAMESPACE")) — an
// explicit env value must win over the "default" fallback.
//
// COUNTERFACTUAL: hardcode Namespace to "default" in buildConfig (never
// reading the env var) and the "set" case goes red.
func TestBuildConfig_NamespaceFromEnv(t *testing.T) {
	t.Setenv("WARDYN_K8S_NAMESPACE", "wardyn-run-ns")
	if cfg := buildConfig(substrate.Deps{}); cfg.Namespace != "wardyn-run-ns" {
		t.Errorf("WARDYN_K8S_NAMESPACE=wardyn-run-ns: Namespace = %q, want wardyn-run-ns", cfg.Namespace)
	}
}

// TestResolveNamespace is T-47 (#707): WARDYN_K8S_NAMESPACE's documented
// fallback chain. An explicit env value always wins; the serviceaccount
// projection file (/var/run/secrets/kubernetes.io/serviceaccount/namespace)
// is a fixed path this test cannot redirect, so the "no explicit value"
// branch below only proves the case that path is absent (true in this
// sandbox, and true of any out-of-cluster `go test` run) — resolveNamespace
// then falls back to "default".
//
// COUNTERFACTUAL: return "" instead of "default" when both the env value and
// the projected file are absent, and the second case goes red.
func TestResolveNamespace(t *testing.T) {
	if got := resolveNamespace("explicit-ns"); got != "explicit-ns" {
		t.Errorf("resolveNamespace(explicit-ns) = %q, want explicit-ns (explicit always wins)", got)
	}
	if _, err := os.Stat(serviceAccountNamespaceFile); err == nil {
		t.Skipf("%s exists on this host; the no-explicit-value branch cannot be isolated here", serviceAccountNamespaceFile)
	}
	if got := resolveNamespace(""); got != "default" {
		t.Errorf("resolveNamespace(\"\") without a serviceaccount projection = %q, want default", got)
	}
}
