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
// cleanUpWaitAfterServe's: what is left of the shutdown sequence's own wait.
func TestPoolsCloseAfterTheSecretStoresCleanUps(t *testing.T) {
	for file, want := range map[string][]string{
		"main.go": {
			"var cleanUpsBy time.Time",
			"defer func() { secretstorepg.CloseAfterCleanUps(pool, cleanUpWaitAfterServe(cleanUpsBy)) }()",
			"\tcleanUpsBy, err = serveAndShutdown(rootCtx, f, posture, srv, idp.Name(), fan, denials, feats.hop, orgFederation)\n\treturn err\n}",
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
		if n := strings.Count(src, "cleanUpsBy = ") + strings.Count(src, "cleanUpsBy, err = "); n != strings.Count(strings.Join(want, ""), "cleanUpsBy, err = ") {
			t.Errorf("%s sets cleanUpsBy %d times; want it set only from serveAndShutdown, as run's last step", file, n)
		}
	}
}
