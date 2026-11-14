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
func TestPoolsCloseAfterTheSecretStoresCleanUps(t *testing.T) {
	for _, file := range []string{"main.go", "rotate_gateway_identity_key.go"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "wardynd", file))
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if !strings.Contains(src, "defer secretstorepg.CloseAfterCleanUps(pool, api.BackgroundShutdownBudget)") || strings.Contains(src, "defer pool.Close()") {
			t.Errorf("%s closes its pool without waiting for the secret store's clean-ups (secretstorepg.CloseAfterCleanUps)", file)
		}
	}
}
