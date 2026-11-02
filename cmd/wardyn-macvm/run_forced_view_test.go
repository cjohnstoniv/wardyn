// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

const forcedViewRunID = "0a1b2c3d-4e5f-4061-8273-8495a6b7c8d9"

// forcedViewBase writes a base directory whose manifest says desktop.
func forcedViewBase(t *testing.T, dir, desktop string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, macvm.ManifestFile), []byte(`{"format":`+strconv.Itoa(macvm.BaseFormat)+`,"state":"ready","desktop":"`+desktop+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func forcedViewShim(t *testing.T, base string, forced bool) (*macvm.RFBShim, string) {
	t.Helper()
	pw, err := macvm.NewDesktopSecret()
	if err != nil {
		t.Fatal(err)
	}
	dir := macvm.Layout{Root: t.TempDir()}.RunDir(forcedViewRunID)
	if err := macvm.WriteRunConfig(dir, macvm.RunConfig{RunID: forcedViewRunID, BaseDir: base, DesktopViewOnly: forced}); err != nil {
		t.Fatal(err)
	}
	return runShim(dir, pw, nil), dir
}

// A forced run's filter is fixed when the helper starts, whatever the base says then or later: a base with
// control, one replaced while the run lives, or one replaced between the shim's construction and the clone.
func TestRunShimForcedIgnoresTheBase(t *testing.T) {
	for _, first := range []string{macvm.DesktopControl, macvm.DesktopObserve} {
		for _, then := range []string{macvm.DesktopControl, macvm.DesktopObserve} {
			base := t.TempDir()
			forcedViewBase(t, base, first)
			shim, _ := forcedViewShim(t, base, true)
			forcedViewBase(t, base, then)
			if !shim.ViewOnly || !shim.ForceViewOnly || !shim.DesktopForcedViewOnly() {
				t.Errorf("%s base replaced by %s: %+v reports forced %v, want view only and forced", first, then, shim, shim.DesktopForcedViewOnly())
			}
		}
	}
}

// A run on a variant base reads the variant's manifest, and forced holds whichever of the two has control.
func TestRunShimForcedOnAVariantDifferingFromStandard(t *testing.T) {
	for _, c := range []struct {
		standard, variant string
		forced, wantView  bool
	}{
		{macvm.DesktopObserve, macvm.DesktopControl, true, true},
		{macvm.DesktopControl, macvm.DesktopObserve, true, true},
		{macvm.DesktopObserve, macvm.DesktopControl, false, false},
		{macvm.DesktopControl, macvm.DesktopObserve, false, true},
	} {
		standard, variant := t.TempDir(), t.TempDir()
		forcedViewBase(t, standard, c.standard)
		forcedViewBase(t, variant, c.variant)
		shim, _ := forcedViewShim(t, variant, c.forced)
		if shim.ViewOnly != c.wantView || shim.DesktopForcedViewOnly() != c.forced {
			t.Errorf("%+v: view only %v, forced %v", c, shim.ViewOnly, shim.DesktopForcedViewOnly())
		}
	}
}

// A boot without the setting (a revive that lost it, a run.json from an older runner) is not forced even
// where an earlier boot of the same run was, and a run.json it can't read forces nothing but stays view only.
func TestRunShimWithoutTheSettingIsNotForced(t *testing.T) {
	base := t.TempDir()
	forcedViewBase(t, base, macvm.DesktopControl)
	if earlier, _ := forcedViewShim(t, base, true); !earlier.DesktopForcedViewOnly() {
		t.Fatal("the earlier boot was not forced")
	}
	later, _ := forcedViewShim(t, base, false)
	if later.ViewOnly || later.DesktopForcedViewOnly() {
		t.Errorf("a later boot without the setting: view only %v, forced %v", later.ViewOnly, later.DesktopForcedViewOnly())
	}
	pw, err := macvm.NewDesktopSecret()
	if err != nil {
		t.Fatal(err)
	}
	if s := runShim(t.TempDir(), pw, nil); !s.ViewOnly || s.DesktopForcedViewOnly() {
		t.Errorf("an unreadable run.json: view only %v, forced %v", s.ViewOnly, s.DesktopForcedViewOnly())
	}
}

// The real helper's ctl.sock answers from its running shim, on the path runnerd reads.
func TestRunStatusReportsTheRunningFilter(t *testing.T) {
	for _, forced := range []bool{true, false} {
		root := t.TempDir()
		ledger := makeLedger(t, root)
		l, err := admission.Open(admission.Config{Dir: ledger, UID: os.Getuid(), RunnerID: "r1", Host: host{}, Fstat: rootFstat})
		if err != nil {
			t.Fatal(err)
		}
		e, err := l.Admit(admission.Request{Kind: admission.KindRun, GuestMemory: 8 << 30, DiskReserve: 1 << 30, OwnerPID: os.Getpid(), OwnerStart: 7})
		if err != nil {
			t.Fatal(err)
		}
		layout := macvm.Layout{Root: filepath.Join(root, "vm")}
		base := layout.BaseDir(0)
		_ = os.MkdirAll(base, 0o700)
		for n, b := range map[string]string{macvm.DiskFile: "d", macvm.AuxFile: "a", macvm.MachineIDFile: "base-id", macvm.PairKeyFile: "wardyn-test-pair-key-0123456789!"} {
			_ = os.WriteFile(filepath.Join(base, n), []byte(b), 0o600)
		}
		_ = os.WriteFile(filepath.Join(base, macvm.ManifestFile), []byte(`{"format":2,"state":"ready","hardware_model":"aHc=","desktop":"control"}`), 0o600)
		dir := layout.RunDir(forcedViewRunID)
		if err := macvm.WriteRunConfig(dir, macvm.RunConfig{RunID: forcedViewRunID, BaseDir: base, CPUs: 2, MemoryMiB: 4096, Identity: macvm.IdentityTravel,
			RunnerID: "r1", LedgerEntry: e.ID, LedgerDir: ledger, SockRoot: macvmtest.SockRoot(t), DesktopViewOnly: forced}); err != nil {
			t.Fatal(err)
		}
		eng := &macvmtest.Engine{}
		d, _, errb := testDeps(eng)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan int, 1)
		go func() { done <- run(ctx, []string{"run", "--dir", dir}, d) }()

		sd, out, _ := testDeps(eng)
		deadline := time.Now().Add(5 * time.Second)
		for run(context.Background(), []string{"status", "--dir", dir}, sd) != 0 {
			if time.Now().After(deadline) {
				t.Fatalf("status never answered: %s", errb.String())
			}
			out.Reset()
			time.Sleep(10 * time.Millisecond)
		}
		var st macvm.Status
		if err := json.Unmarshal(out.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		if st.DesktopForcedViewOnly != forced {
			t.Errorf("run.json forced=%v: status says %v", forced, st.DesktopForcedViewOnly)
		}
		cancel()
		if code := <-done; code != 0 {
			t.Fatalf("run exit %d: %s", code, errb.String())
		}
	}
}
