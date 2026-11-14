// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A process that may exit right after a secret store Put (wardynd's run, whose
// boot steps write keys, and the key rotations through withRotationStore)
// closes its pool only after the Put's clean-ups: pool.Close alone cuts one
// that has not yet taken its connection, and the value it was removing stays.
// run's wait is the whole budget until serveAndShutdown returns, and then
// cleanUpWaitAfterServe's: none once the shutdown sequence has waited.
func TestPoolsCloseAfterTheSecretStoresCleanUps(t *testing.T) {
	for file, want := range map[string][]string{
		"main.go": {
			"cleanUpWait := api.BackgroundShutdownBudget",
			"defer func() { secretstorepg.CloseAfterCleanUps(pool, cleanUpWait) }()",
			"cleanUpsWaited, err := serveAndShutdown(",
			"\tcleanUpWait = cleanUpWaitAfterServe(cleanUpsWaited)\n\treturn err\n}",
		},
		"rotate_gateway_identity_key.go": {"defer secretstorepg.CloseAfterCleanUps(pool, api.BackgroundShutdownBudget)"},
	} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "wardynd", file))
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, w := range want {
			if !strings.Contains(src, w) {
				t.Errorf("%s lacks %q: its pool must close after the secret store's clean-ups (secretstorepg.CloseAfterCleanUps)", file, w)
			}
		}
		if strings.Contains(src, "defer pool.Close()") {
			t.Errorf("%s closes its pool without waiting for the secret store's clean-ups", file)
		}
		if n := strings.Count(src, "cleanUpWait = "); n != strings.Count(strings.Join(want, ""), "cleanUpWait = ") {
			t.Errorf("%s sets cleanUpWait %d times; want it set only from cleanUpWaitAfterServe after serveAndShutdown", file, n)
		}
	}
}
