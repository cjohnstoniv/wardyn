// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The 30-second bound on the boot-key reads starts after the secret store is
// open: opening it (row conversion, key-domain proof) runs under rootCtx with
// no bound, and a deadline made before it would be spent before the first key
// is read.
func TestBootDeadlineStartsAfterTheSecretStoreOpens(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "wardynd", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	open := strings.Index(src, "secrets, err := openSecretStore(rootCtx")
	deadline := strings.Index(src, "bootCtx, cancel := context.WithTimeout(rootCtx, 30*time.Second)")
	first := strings.Index(src, "armSubjectKeyed(bootCtx")
	if !(open >= 0 && open < deadline && deadline < first) {
		t.Errorf("main.go order: openSecretStore at %d, bootCtx deadline at %d, first boot-key read at %d; the boot-key deadline must start after the secret store is open, so opening it cannot use up the budget", open, deadline, first)
	}
	if n := strings.Count(src, "bootCtx, cancel :="); n != 1 {
		t.Errorf("main.go creates bootCtx %d times; want once", n)
	}
}
