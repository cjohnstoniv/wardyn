// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// prepare runs its install in this helper's install-base process (#3699).
func TestPrepareInstallsInAHelperProcess(t *testing.T) {
	d, _, errb := testDeps(&macvmtest.Engine{})
	var got []string
	d.prepare = func(o *macvm.PrepareOptions, _ *slog.Logger) error {
		got = o.InstallProcess
		return errors.New("stop here")
	}
	run(context.Background(), []string{"prepare", "--root", t.TempDir(), "--cpus", "4", "--memory-mib", "8192"}, d)
	exe, _ := os.Executable()
	if len(got) != 2 || got[0] != exe || got[1] != macvm.InstallCommand {
		t.Fatalf("InstallProcess = %q (%s)", got, errb.String())
	}
}

func TestInstallBaseRunsOneInstall(t *testing.T) {
	dir := t.TempDir()
	disk, aux := filepath.Join(dir, "disk.img"), filepath.Join(dir, "aux.img")
	for _, p := range []string{disk, aux} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	req, _ := json.Marshal(map[string]any{
		"Spec": macvm.MachineSpec{CPUs: 4, MemoryBytes: 8 << 30, MinCPUs: 4, MinMemoryBytes: 4 << 30,
			HardwareModel: []byte("hw"), MachineIdentifier: []byte("id"), DiskPath: disk, AuxPath: aux},
		"IPSW": filepath.Join(dir, "restore.ipsw"),
	})
	eng := &macvmtest.Engine{}
	d, out, errb := testDeps(eng)
	d.stdin = strings.NewReader(string(req))
	if code := run(context.Background(), []string{macvm.InstallCommand}, d); code != 0 || eng.Installs() != 1 ||
		!strings.Contains(out.String(), `{"fraction":1}`) {
		t.Fatalf("exit %d, %d installs, stdout %q, stderr %q", code, eng.Installs(), out.String(), errb.String())
	}
	d, _, errb = testDeps(eng)
	d.stdin = strings.NewReader("not a request")
	if code := run(context.Background(), []string{macvm.InstallCommand}, d); code != 1 || !strings.Contains(errb.String(), "install request") {
		t.Fatalf("a bad request: exit %d, stderr %q", code, errb.String())
	}
}
