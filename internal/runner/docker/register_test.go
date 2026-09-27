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
