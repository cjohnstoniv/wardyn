// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

const installHelperEnv = "WARDYN_MACVM_TEST_INSTALL"

// TestInstallProcessHelper is the install process the tests below start: this
// test binary running macvm.RunInstall on a fake engine whose machines lock
// their aux storage until the process exits.
func TestInstallProcessHelper(t *testing.T) {
	mode := os.Getenv(installHelperEnv)
	if mode == "" {
		t.Skip("run by the install process tests")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	eng := &macvmtest.Engine{LockAux: true}
	switch mode {
	case "fail":
		eng.InstallFunc = func(context.Context, *macvmtest.Machine, string, func(float64)) error {
			return errors.New("installer failed")
		}
	case "noprogress":
		// Lines that are not progress: none may move the install's fraction.
		fmt.Println(`{}`)
		fmt.Println(`{"other":1}`)
		fmt.Println(`{"fraction":0.5}`)
	case "hang":
		eng.InstallFunc = func(ctx context.Context, _ *macvmtest.Machine, _ string, progress func(float64)) error {
			progress(0.5)
			<-ctx.Done()
			return ctx.Err()
		}
	}
	if err := macvm.RunInstall(ctx, eng, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func installInProcess(t *testing.T, f *prepFixture, mode string) {
	t.Setenv(installHelperEnv, mode)
	f.opts.InstallProcess = []string{os.Args[0], "-test.run=^TestInstallProcessHelper$"}
	f.eng.LockAux = true // an install in this process would keep the aux locked
	f.fb.LockAux = true
}

// The framework keeps the installer's VM, and its lock on the aux storage, for
// the life of the process that built it, so the install runs in a process of
// its own and the first boot starts once that process has exited (#3699).
func TestPrepareFirstBootFollowsTheInstallProcess(t *testing.T) {
	defer macvm.SetAuxLockBackoff(time.Millisecond)()
	f := newPrep(t)
	installInProcess(t, f, "ok")
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if n := len(f.eng.Machines()); n != 0 || f.fb.Boots() != 1 {
		t.Fatalf("this process built %d installer VMs; %d first boots", n, f.fb.Boots())
	}
	if evs := f.ev.phase(macvm.PhaseInstall); len(evs) == 0 || evs[len(evs)-1].Fraction != 1 {
		t.Fatalf("the install process's progress did not reach the events: %+v", evs)
	}
}

func TestPrepareInstallProcessFailures(t *testing.T) {
	f := newPrep(t)
	installInProcess(t, f, "fail")
	_, err := macvm.Prepare(context.Background(), f.opts)
	if err == nil || !strings.Contains(err.Error(), macvm.InstallFailurePrefix+"installer failed") || f.fb.Boots() != 0 {
		t.Fatalf("a failed install process: %v, %d first boots", err, f.fb.Boots())
	}

	g := newPrep(t)
	installInProcess(t, g, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := g.opts.Progress
	g.opts.Progress = func(ev macvm.PrepareEvent) {
		progress(ev)
		if ev.Phase == macvm.PhaseInstall && ev.Fraction == 0.5 {
			cancel()
		}
	}
	if _, err := macvm.Prepare(ctx, g.opts); err == nil || g.fb.Boots() != 0 {
		t.Fatalf("a cancelled install process: %v, %d first boots", err, g.fb.Boots())
	}
}

// The framework's per-VM process may let go of the aux storage a moment after
// the install process exits: the first boot retries a few times on exactly
// that refusal, and on nothing else.
func TestPrepareFirstBootRetriesOnTheAuxLock(t *testing.T) {
	defer macvm.SetAuxLockBackoff(time.Millisecond)()
	f := newPrep(t)
	locked := 2
	f.fb.AuxLocked = func() bool { locked--; return locked >= 0 }
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if f.fb.Boots() != 3 {
		t.Fatalf("first boots = %d, want 3", f.fb.Boots())
	}

	g := newPrep(t)
	g.fb.AuxLocked = func() bool { return true }
	if _, err := macvm.Prepare(context.Background(), g.opts); err == nil || !strings.Contains(err.Error(), "Failed to lock auxiliary storage") {
		t.Fatalf("an aux storage that stays locked: %v", err)
	}
	if g.fb.Boots() != 6 {
		t.Fatalf("first boots = %d, want 6", g.fb.Boots())
	}

	h := newPrep(t)
	h.fb.BootErr = errors.New("some other refusal")
	if _, err := macvm.Prepare(context.Background(), h.opts); err == nil || h.fb.Boots() != 1 {
		t.Fatalf("another start error was retried: %v, %d boots", err, h.fb.Boots())
	}
}

// Only a line with a fraction is progress: {} cannot send the install's
// fraction back to 0.
func TestPrepareInstallProgressNeedsAFraction(t *testing.T) {
	f := newPrep(t)
	installInProcess(t, f, "noprogress")
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	var got []float64
	for _, ev := range f.ev.phase(macvm.PhaseInstall) {
		got = append(got, ev.Fraction)
	}
	if !slices.Equal(got, []float64{0, 0.5, 0.5, 1}) {
		t.Fatalf("install fractions %v, want [0 0.5 0.5 1]", got)
	}
}
