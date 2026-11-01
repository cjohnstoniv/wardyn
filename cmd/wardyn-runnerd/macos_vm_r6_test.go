// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// G1: `setup --keep-ipsw` keeps Apple's restore image through the prepare setup starts (the rental
// window's P3 runs against a second base, DESIGN.r6 §3.4 step 9); without the flag, setup's prepare
// deletes it. The flag's help is prepare's own.
func TestSetupKeepIPSWReachesItsPrepare(t *testing.T) {
	prev, prevEx := prepareClaimWait, excludeFromBackups
	prepareClaimWait, excludeFromBackups = 2*time.Second, func(string) error { return nil }
	t.Cleanup(func() { prepareClaimWait, excludeFromBackups = prev, prevEx })
	for _, keep := range []bool{false, true} {
		r := newMacLedgerRig(t)
		r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
		m := r.open(t)
		_ = os.Remove(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile)) // no base yet
		helper, args := fakeHelper(t, 0)
		var plan setupPlan
		plan.keepIPSW = keep
		plan.paths.installed = filepath.Join(filepath.Dir(helper), "wardyn-runnerd")
		cmd := &cobra.Command{}
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetContext(context.Background())
		startSetupPrepare(cmd, m, plan, "runner-a", &types.RunnerLimits{PerRun: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8192}})
		var prep []string
		for _, c := range calls(t, args) {
			if f := strings.Fields(c); len(f) > 0 && f[0] == macvm.PrepareCommand {
				prep = f
			}
		}
		if prep == nil {
			t.Fatalf("keep %v: setup started no prepare: %q", keep, calls(t, args))
		}
		if _, set := helperFlags(t, prep); (set[macvm.FlagKeepIPSW] == "true") != keep {
			t.Fatalf("setup --keep-ipsw=%v: prepare %v", keep, prep)
		}
	}
	setup, prepare := newSetupCommand().Flags().Lookup("keep-ipsw"), macOSVMSubFlag(t, "prepare", "keep-ipsw")
	if setup == nil || setup.Usage != prepare.Usage {
		t.Fatalf("setup --keep-ipsw %+v; want prepare's help %q", setup, prepare.Usage)
	}
}

// macOSVMSubFlag is `macos-vm <sub>`'s flag name.
func macOSVMSubFlag(t *testing.T, sub, name string) *pflag.Flag {
	t.Helper()
	for _, c := range newMacOSVMCommand().Commands() {
		if c.Name() == sub {
			if f := c.Flags().Lookup(name); f != nil {
				return f
			}
		}
	}
	t.Fatalf("macos-vm %s has no --%s", sub, name)
	return nil
}

// G1: a setup rerun, which does not know the limits, prints the prepare command with --keep-ipsw when
// setup had it.
func TestSetupRerunPrintsPrepareWithKeepIPSW(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, keep := range []bool{false, true} {
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		var plan setupPlan
		plan.keepIPSW = keep
		prepareMacOSVMBase(cmd, plan, nil)
		if got := strings.Contains(out.String(), "--per-run-memory <GB> --keep-ipsw ("); got != keep || !strings.Contains(out.String(), "wardyn-runnerd macos-vm prepare") {
			t.Fatalf("keep %v: %q", keep, out.String())
		}
	}
}

// #3870: setup's prepare is refused with the no-helper words, before the VM directory is made, when no helper is
// beside the runner setup installed.
func TestSetupPrepareRefusesWithNoHelperBesideTheInstalledRunner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	var plan setupPlan
	plan.paths.installed = filepath.Join(t.TempDir(), "wardyn-runnerd")
	prepareMacOSVMBase(cmd, plan, &types.RunnerLimits{PerRun: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8192}})
	if !strings.Contains(out.String(), macOSVMNoHelper) {
		t.Fatalf("no refusal with the no-helper words: %q", out.String())
	}
	root, err := macvm.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err == nil {
		t.Fatalf("the refused prepare made %s", root)
	}
}

// #3870: prepare looks for the serving runner's helper where that runner's serve does: beside its binary,
// symlinks resolved.
func TestNoHelperBesideLooksWhereServeLooks(t *testing.T) {
	pkg, link := t.TempDir(), t.TempDir()
	bin := filepath.Join(pkg, "wardyn-runnerd")
	if err := os.WriteFile(bin, []byte("runnerd"), 0o755); err != nil {
		t.Fatal(err)
	}
	onPath := filepath.Join(link, "wardyn-runnerd")
	if err := os.Symlink(bin, onPath); err != nil {
		t.Fatal(err)
	}
	for _, exe := range []string{bin, onPath} {
		if err := noHelperBeside(exe); err == nil || !strings.Contains(err.Error(), macOSVMNoHelper) || !strings.Contains(err.Error(), "wardyn-runnerd setup") {
			t.Fatalf("%s with no helper: %v", exe, err)
		}
	}
	if err := os.WriteFile(filepath.Join(pkg, "wardyn-macvm"), []byte("helper"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, exe := range []string{bin, onPath} {
		if err := noHelperBeside(exe); err != nil {
			t.Fatalf("%s with its helper: %v", exe, err)
		}
	}
}

// Seam audit X4: every phase the helper's prepare reports (internal/macvm's Phase constants, read from
// its source) is in prepareOrder, but ready, which ends it; and progress never falls back through
// stage 2.
func TestMacOSVMPrepareOrderHoldsEveryHelperPhase(t *testing.T) {
	dir := filepath.Join("..", "..", "internal", "macvm")
	mac := build.Default
	mac.GOOS, mac.GOARCH, mac.CgoEnabled = "darwin", "arm64", true
	pkg, err := mac.ImportDir(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	var phases []string
	for _, name := range append(pkg.GoFiles, pkg.CgoFiles...) {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Phase") || i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, _ := strconv.Unquote(lit.Value)
					phases = append(phases, v)
				}
			}
			return true
		})
	}
	if len(phases) < 10 {
		t.Fatalf("read only %v from %s", phases, dir)
	}
	for _, p := range phases {
		if p != macvm.PhaseReady && !slices.Contains(prepareOrder, p) {
			t.Errorf("the helper reports phase %q, which prepareOrder lacks (progress falls to 0)", p)
		}
	}
	log := filepath.Join(t.TempDir(), "prepare.log")
	last := 0
	for _, p := range prepareOrder {
		_ = os.WriteFile(log, []byte(`{"phase":"`+p+`"}`+"\n"), 0o600)
		if got := prepareProgress(log); got < last {
			t.Errorf("progress falls from %d to %d at %s", last, got, p)
		} else {
			last = got
		}
	}
	at := func(p string) int { return slices.Index(prepareOrder, p) }
	if at(macvm.PhaseShutdown) > at(macvm.PhaseTools) || at(macvm.PhaseProvision) > at(macvm.PhaseSetupByHand) || at(macvm.PhaseSetupByHand) > at(macvm.PhaseBootstrap) {
		t.Errorf("not the helper's order (setup by hand between provision and bootstrap, tools after shutdown): %v", prepareOrder)
	}
}
