// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// TestUpdateToolsRefusesABadVariantName: a derived base's name is a dir under the VM root, so one that is
// not a base name is refused before anything is read or booted.
func TestUpdateToolsRefusesABadVariantName(t *testing.T) {
	root := t.TempDir()
	eng := &macvmtest.Engine{}
	d, _, _ := testDeps(eng)
	var errb lockedBuf
	d.stderr, d.powerSource = &errb, onAC{}
	args := append(runnerdArgs(filepath.Join(root, "vm"), "e1", makeLedger(t, root)), "--"+macvm.FlagVariant, "../bases/0")
	if code := run(context.Background(), args, d); code != 2 || !strings.Contains(errb.String(), "is not a macOS base name") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if n := len(eng.Machines()); n != 0 {
		t.Fatalf("%d VMs booted for a refused name", n)
	}
}
