// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
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

// TestUpdateToolsRefusesAProfileInAVariantList: a derived base's list holds no profile item (nothing would
// remove it), so the hand-run helper refuses one, as runnerd does, before anything is booted.
func TestUpdateToolsRefusesAProfileInAVariantList(t *testing.T) {
	placeGuestAgent(t)
	root := t.TempDir()
	list := filepath.Join(root, "xcode.json")
	item := `{"kind":"profile","name":"edr","path":"/opt/edr.mobileconfig","identifier":"com.example.edr","sha256":"` + strings.Repeat("a", 64) + `"}`
	if err := os.WriteFile(list, []byte(`{"version":1,"items":[`+item+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	eng := &macvmtest.Engine{}
	d, _, _ := testDeps(eng)
	var errb lockedBuf
	d.stderr, d.powerSource = &errb, onAC{}
	args := append(runnerdArgs(filepath.Join(root, "vm"), "e1", makeLedger(t, root)), "--"+macvm.FlagVariant, "xcode-16", "--"+macvm.FlagBaseFile, list)
	want := `--variant xcode-16: item "edr" is a profile, which only Standard's list can hold`
	if code := run(context.Background(), args, d); code != 2 || !strings.Contains(errb.String(), want) {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if n := len(eng.Machines()); n != 0 {
		t.Fatalf("%d VMs booted for a refused list", n)
	}
}
