// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// edit-base needs runnerd's ledger entry, and with no ready base answers the approved refusal on its last
// stderr line, before anything boots.
func TestEditBaseRefusesBeforeBooting(t *testing.T) {
	root := t.TempDir()
	eng := &macvmtest.Engine{}
	d, _, errb := testDeps(eng)
	d.firstBoot = &macvmtest.FirstBoot{}
	args := []string{macvm.EditBaseCommand, "--" + macvm.FlagRoot, root, "--" + macvm.FlagCPUs, "2", "--" + macvm.FlagMemoryMiB, "4096"}
	if code := run(context.Background(), args, d); code != 2 || !strings.Contains(errb.String(), "--"+macvm.FlagLedgerEntry) {
		t.Fatalf("no entry: exit %d: %s", code, errb)
	}
	errb.Reset()
	args = append(args, "--"+macvm.FlagRunnerID, "r1", "--"+macvm.FlagLedgerEntry, "0123456789abcdef")
	if code := run(context.Background(), args, d); code != 1 || strings.TrimSpace(errb.String()) != "wardyn-macvm: "+string(macvm.ErrEditNoBase) {
		t.Fatalf("no base: exit %d: %q", code, errb)
	}
	if len(eng.Machines()) != 0 {
		t.Fatal("a refused edit built a VM")
	}
}

// silentNet hands each stage-2 boot its network through the boot's own ctl.sock, so the boot starts with
// no runnerd network, once per boot.
func TestSilentNetGivesEachStage2BootItsNetwork(t *testing.T) {
	eng := &macvmtest.Engine{}
	sockRoot := macvmtest.SockRoot(t)
	entry := "0123456789abcdef"
	boot := macvm.Stage2BootConfig{Engine: eng, SockRoot: sockRoot, SockKey: macvm.JobSockKey(entry), SockFstat: macvmtest.SockFstat, NetWait: 10 * time.Second}
	tb := silentNet(boot.ToolsBoot(), macvm.JobControlSock(sockRoot, entry))
	dir := t.TempDir()
	for _, f := range []string{macvm.DiskFile, macvm.AuxFile} {
		_ = os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600)
	}
	_ = os.WriteFile(filepath.Join(dir, macvm.PairKeyFile), []byte("wardyn-test-pair-key-0123456789!"), 0o600)
	spec := macvm.MachineSpec{CPUs: 2, MemoryBytes: 4 << 30, MinCPUs: 2, MinMemoryBytes: 4 << 30, HardwareModel: []byte("hw"),
		MachineIdentifier: []byte("id"), DiskPath: filepath.Join(dir, macvm.DiskFile), AuxPath: filepath.Join(dir, macvm.AuxFile)}
	for i := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, stop, err := tb(ctx, spec)
		if err != nil {
			cancel()
			t.Fatalf("boot %d: %v", i, err)
		}
		if err := stop(ctx); err != nil {
			t.Errorf("stop %d: %v", i, err)
		}
		cancel()
	}
	if len(eng.Machines()) != 2 {
		t.Fatalf("%d VMs built, want 2", len(eng.Machines()))
	}
}
