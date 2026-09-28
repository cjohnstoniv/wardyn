// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// TestBuildConfig_RecordFollowsDeps is the docker half of the #1113 fix
// (register.go hardcoded `Record: true` on both drivers): buildConfig — the
// ONLY place init()'s registered "docker" constructor builds a Config from —
// must carry Deps.Record through to Config.Record verbatim. Deps.Record is
// itself derived from the boot recording-store selection
// (substrate.RecordEnabled, wired in cmd/wardynd's buildRunnerFromFlags), so
// this is computed from a store name rather than a literal bool: "off" must
// not record, "pg"/"fs" must.
//
// COUNTERFACTUAL: hardcode `Record: true` in buildConfig (reverting the
// #1113 fix) and this test goes red for the "off" case.
func TestBuildConfig_RecordFollowsDeps(t *testing.T) {
	for _, store := range []string{"off", "pg", "fs"} {
		want := substrate.RecordEnabled(store)
		cfg := buildConfig(substrate.Deps{Record: want}, "")
		if cfg.Record != want {
			t.Errorf("store %q: buildConfig(Deps{Record: %v}, \"\").Record = %v, want %v", store, want, cfg.Record, want)
		}
	}
}

// TestBuildConfig_InternalNetworkFromEnv is T-47 (#707): buildConfig — the
// ONLY place init()'s registered "docker" constructor builds a Config from —
// must carry WARDYN_INTERNAL_NETWORK through to Config.InternalNetwork
// verbatim, including empty (withDefaults then keeps the "wardyn-internal"
// default; this test is about what buildConfig itself reads, not that later
// default).
//
// COUNTERFACTUAL: hardcode InternalNetwork to "" in buildConfig (never reading
// the env var) and the "custom" case below goes red.
func TestBuildConfig_InternalNetworkFromEnv(t *testing.T) {
	for _, tc := range []struct{ env, want string }{
		{"", ""},
		{"wardyn-cl-707", "wardyn-cl-707"},
	} {
		t.Setenv("WARDYN_INTERNAL_NETWORK", tc.env)
		cfg := buildConfig(substrate.Deps{}, "")
		if cfg.InternalNetwork != tc.want {
			t.Errorf("WARDYN_INTERNAL_NETWORK=%q: InternalNetwork = %q, want %q", tc.env, cfg.InternalNetwork, tc.want)
		}
	}
}

// TestBuildConfig_AllowUnenforceableCapsFromEnv is T-47 (#707): buildConfig
// must read WARDYN_ALLOW_UNENFORCEABLE_CAPS through cliutil.EnvBool (the
// shared 1/true/yes/on token set), not a literal "1" compare (#202) — unset
// stays fail-closed (false). cliutil's own TestEnvBool (internal/cliutil)
// covers the garbage-value-exits-2 branch; EnvBool's fatal path exits the
// process via an unexported package var this test cannot reach.
//
// COUNTERFACTUAL: hardcode AllowUnenforceableCaps to false in buildConfig
// (never reading the env var) and the "true" case goes red.
func TestBuildConfig_AllowUnenforceableCapsFromEnv(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{
		{"", false},
		{"true", true},
		{"1", true},
		{"yes", true},
	} {
		t.Setenv("WARDYN_ALLOW_UNENFORCEABLE_CAPS", tc.env)
		cfg := buildConfig(substrate.Deps{}, "")
		if cfg.AllowUnenforceableCaps != tc.want {
			t.Errorf("WARDYN_ALLOW_UNENFORCEABLE_CAPS=%q: AllowUnenforceableCaps = %v, want %v", tc.env, cfg.AllowUnenforceableCaps, tc.want)
		}
	}
}
