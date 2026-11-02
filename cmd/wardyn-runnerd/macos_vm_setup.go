// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// macHostMemory is this Mac's memory (hw.memsize), swapped in tests.
var macHostMemory = func() (uint64, error) { return admission.SystemHost().MemoryBytes() }

// macOSVMHost reports an Apple silicon Mac, where macOS VMs run; swapped in tests.
var macOSVMHost = func(goos string) bool { return goos == "darwin" && runtime.GOARCH == "arm64" }

// checkMacOSVMFlag refuses a --macos-vm setup cannot honour, before any token is spent.
func checkMacOSVMFlag(v, goos string) error {
	switch {
	case v != "" && v != "on" && v != "off":
		return errors.New("--macos-vm is on or off")
	case v == "on" && !macOSVMHost(goos):
		return errors.New("--macos-vm on needs an Apple silicon Mac")
	}
	return nil
}

// askMacOSVM settles setup's macos_vm: the flag, else (on an Apple silicon Mac, at a terminal, with
// macOS VMs not yet on) the person's answer, else the earlier setting. When they are on, it shows this
// Mac's memory sum for the limits just registered.
func askMacOSVM(cmd *cobra.Command, goos string, plan *setupPlan, limits *types.RunnerLimits) error {
	if !macOSVMHost(goos) {
		return nil
	}
	out := cmd.OutOrStdout()
	if plan.macOSVM == "" && !macOSVMOn(plan.existing.MacOSVM) && asksAt(cmd.InOrStdin(), out) {
		fmt.Fprint(out, "Turn on macOS VM sandboxes on this Mac? Preparing their base downloads Apple's macOS (about 25 GB) and needs about 85 GB free. [y/N] ")
		line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		plan.macOSVM = "off"
		if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
			plan.macOSVM = "on"
		}
	}
	if !macOSVMOn(plan.runnerFile().MacOSVM) || limits == nil {
		return nil
	}
	a := macOSVMArithmetic{total: uint64(limits.Total.MemoryMiB) << 20, perRun: uint64(limits.PerRun.MemoryMiB) << 20}
	if m, err := measureHost(cmd.Context()); err == nil {
		a.docker = uint64(max(m.MemoryMiB, 0)) << 20
	}
	host, err := macHostMemory()
	if err != nil {
		return nil // the sum needs the Mac's memory; the ledger checks it at every start anyway
	}
	a.host = host
	for _, l := range a.lines() {
		fmt.Fprintln(out, l)
	}
	return nil
}

// prepareMacOSVMBase starts the base's preparation once the service runs, with the per-run limits just
// registered (prepare's VM never raises them). A rerun, whose limits setup does not know, or a base
// already ready, starts nothing. A prepare that cannot start is said, not fatal: the runner is set up.
func prepareMacOSVMBase(cmd *cobra.Command, plan setupPlan, limits *types.RunnerLimits) {
	out := cmd.OutOrStdout()
	root, err := macvm.DefaultRoot()
	if err != nil {
		fmt.Fprintf(out, "macOS VMs: %v\n", err)
		return
	}
	if _, err := macvm.ReadyBase(macvm.Layout{Root: root}.BaseDir(0)); err == nil {
		return
	}
	if limits == nil {
		keep := ""
		if plan.keepIPSW {
			keep = " --" + macvm.FlagKeepIPSW
		}
		fmt.Fprintln(out, "macOS VMs are on. To build this Mac's macOS base: wardyn-runnerd macos-vm prepare --per-run-cpu <CPUs> --per-run-memory <GB>"+keep+" (the runner's per-run limits).")
		return
	}
	// serve offers no macOS VM without the helper beside the runner setup installed (enableMacOSVM, no_helper),
	// so it would never hand this prepare's stage 2 its network: refused before anything is written.
	if err := noHelperBeside(plan.paths.installed); err != nil {
		fmt.Fprintf(out, "macOS VMs are on, but this Mac's macOS base did not start preparing: %v\n", err)
		return
	}
	id, err := runneridentity.Load(plan.paths.stateDir, plan.org)
	if err != nil {
		fmt.Fprintf(out, "macOS VMs: %v\n", err)
		return
	}
	m := openMacOSVMs(macOSVMSetup{ledgerDir: admission.DefaultDir, root: root, runnerID: id.RunnerID.String(), version: version.String(), uid: os.Getuid(), host: admission.SystemHost(), owner: fileOwner})
	startSetupPrepare(cmd, m, plan, id.RunnerID.String(), limits)
}

// startSetupPrepare is setup's prepare on m: the helper beside the installed runnerd, the limits just
// registered, and setup's --keep-ipsw.
func startSetupPrepare(cmd *cobra.Command, m *macOSVMs, plan setupPlan, runnerID string, limits *types.RunnerLimits) {
	out := cmd.OutOrStdout()
	a := prepareArgs{helper: filepath.Join(filepath.Dir(plan.paths.installed), "wardyn-macvm"), runnerID: runnerID,
		cpus: int(limits.PerRun.CPUMillis / 1000), memoryMiB: uint64(limits.PerRun.MemoryMiB), keepIPSW: plan.keepIPSW}
	if managedPref("MacOSVMImageURL") == "" { // the profile's mirror wins, and startPrepare reads it; else runnerd.json's
		f := plan.runnerFile()
		a.ipswURL, a.sha256 = f.MacOSVMImageURL, f.MacOSVMImageHash
	}
	if err := m.startPrepare(cmd.Context(), a); err != nil {
		fmt.Fprintf(out, "macOS VMs are on, but this Mac's macOS base did not start preparing: %v\n", err)
		return
	}
	fmt.Fprintf(out, "Preparing this Mac's macOS base in the background (hours); progress is in %s and in `wardyn-runnerd macos-vm status`.\n", m.layout.PrepareLog())
}
