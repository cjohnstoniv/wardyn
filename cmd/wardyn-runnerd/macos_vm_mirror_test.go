// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const testMirror = "https://mirror.example/UniversalMac_27.0_26A1_Restore.ipsw"

// mirrorRig is a runner with no ready base and a fake helper, and the helper's prepare argv after a start.
func mirrorRig(t *testing.T) (m *macOSVMs, prepareArgv func() []string) {
	t.Helper()
	r := newMacLedgerRig(t)
	r.host.setProcs(admission.Proc{PID: os.Getpid(), UID: 501, Start: 3, Name: "wardyn-runnerd"})
	m = r.open(t)
	if err := os.Remove(m.layout.BaseDir(0) + "/" + macvm.ManifestFile); err != nil {
		t.Fatal(err)
	}
	prev, prevEx := prepareClaimWait, excludeFromBackups
	prepareClaimWait, excludeFromBackups = 2*time.Second, func(string) error { return nil }
	t.Cleanup(func() { prepareClaimWait, excludeFromBackups = prev, prevEx })
	var args string
	m.helper, args = fakeHelper(t, 0)
	return m, func() []string {
		for _, c := range calls(t, args) {
			if f := strings.Fields(c); len(f) > 0 && f[0] == macvm.PrepareCommand {
				return f
			}
		}
		return nil
	}
}

func flagValue(argv []string, name string) string {
	for i, a := range argv {
		if a == "--"+name && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// The helper's prepare is told the mirror and its pin, and not --ipsw; an --ipsw file with a mirror, or a
// mirror with no hash, starts nothing.
func TestStartPrepareMirrorFlags(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	m, argv := mirrorRig(t)
	// The fake helper ends at once, so the start itself reports that; its argv is what is read.
	_ = m.startPrepare(context.Background(), prepareArgs{helper: m.helper, runnerID: "runner-a", cpus: 4, memoryMiB: 8192, ipswURL: testMirror, sha256: strings.ToUpper(hash)})
	got := argv()
	if flagValue(got, macvm.FlagIPSWURL) != testMirror || flagValue(got, macvm.FlagSHA256) != hash || flagValue(got, macvm.FlagIPSW) != "" {
		t.Fatalf("prepare argv %q", got)
	}

	m, argv = mirrorRig(t)
	for name, a := range map[string]prepareArgs{
		"file and mirror": {ipsw: "/x/a.ipsw", ipswURL: testMirror, sha256: hash},
		"no hash":         {ipswURL: testMirror},
		"http":            {ipswURL: "http://mirror.example/a.ipsw", sha256: hash},
	} {
		a.helper, a.runnerID, a.cpus, a.memoryMiB = m.helper, "runner-a", 4, 8192
		if err := m.startPrepare(context.Background(), a); err == nil {
			t.Errorf("%s: started", name)
		}
	}
	if got := argv(); got != nil {
		t.Fatalf("a refused mirror ran the helper: %q", got)
	}
	if err := m.startPrepare(context.Background(), prepareArgs{ipsw: "/x/a.ipsw", ipswURL: testMirror, sha256: hash}); !errors.Is(err, macvm.ErrIPSWAndURL) {
		t.Fatalf("file and mirror: %v", err)
	}
}

// Setup's prepare has no flags: the profile's MacOSVMImageURL and MacOSVMImageHash name the mirror. A prepare
// told an --ipsw file uses that file and not the profile's mirror.
func TestStartPrepareMirrorFromTheProfile(t *testing.T) {
	hash := strings.Repeat("cd", 32)
	withManagedPrefs(t, "darwin", map[string]string{"MacOSVMImageURL": testMirror, "MacOSVMImageHash": hash})
	m, argv := mirrorRig(t)
	_ = m.startPrepare(context.Background(), prepareArgs{helper: m.helper, runnerID: "runner-a", cpus: 4, memoryMiB: 8192})
	if got := argv(); flagValue(got, macvm.FlagIPSWURL) != testMirror || flagValue(got, macvm.FlagSHA256) != hash {
		t.Fatalf("prepare argv %q, want the profile's mirror", got)
	}
}

// Setup's prepare takes the mirror from runnerd.json when the profile names none, and the profile's wins.
func TestStartSetupPrepareMirrorFromRunnerJSON(t *testing.T) {
	hash := strings.Repeat("ef", 32)
	plan := setupPlan{existing: runnerFile{MacOSVMImageURL: testMirror, MacOSVMImageHash: hash}}
	limits := &types.RunnerLimits{PerRun: types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8192}}
	run := func(t *testing.T) []string {
		t.Helper()
		m, argv := mirrorRig(t)
		plan.paths.installed = filepath.Join(filepath.Dir(m.helper), "wardyn-runnerd")
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background())
		cmd.SetOut(io.Discard)
		startSetupPrepare(cmd, m, plan, "runner-a", limits)
		return argv()
	}

	withManagedPrefs(t, "darwin", nil)
	if got := run(t); flagValue(got, macvm.FlagIPSWURL) != testMirror || flagValue(got, macvm.FlagSHA256) != hash {
		t.Fatalf("prepare argv %q, want runnerd.json's mirror", got)
	}

	other := strings.Repeat("12", 32)
	withManagedPrefs(t, "darwin", map[string]string{"MacOSVMImageURL": "https://profile.example/a.ipsw", "MacOSVMImageHash": other})
	if got := run(t); flagValue(got, macvm.FlagIPSWURL) != "https://profile.example/a.ipsw" || flagValue(got, macvm.FlagSHA256) != other {
		t.Fatalf("prepare argv %q, want the profile's mirror over runnerd.json's", got)
	}
}

// --ipsw-url takes its hash from --sha256 only: with none, prepare refuses before any download, and the
// profile's hash, pinned for another image, is not paired with it.
func TestPrepareMirrorFlagTakesItsOwnHash(t *testing.T) {
	profileHash := strings.Repeat("34", 32)
	withManagedPrefs(t, "darwin", map[string]string{"MacOSVMImageHash": profileHash})
	prepare := macOSVMSubCommand(t, "prepare")
	if err := prepare.Flags().Set("ipsw-url", testMirror); err != nil {
		t.Fatal(err)
	}
	url, hash, err := prepareMirror(prepare, "", testMirror, "")
	if err != nil || url != testMirror || hash != "" {
		t.Fatalf("mirror = %q, %q, %v; want the flag's URL and no hash", url, hash, err)
	}
	m, argv := mirrorRig(t)
	if err := m.startPrepare(context.Background(), prepareArgs{helper: m.helper, runnerID: "runner-a", cpus: 4, memoryMiB: 8192, ipswURL: url, sha256: hash}); !errors.Is(err, macvm.ErrIPSWURLNeedsSHA256) {
		t.Fatalf("start = %v, want %v", err, macvm.ErrIPSWURLNeedsSHA256)
	}
	if got := argv(); got != nil {
		t.Fatalf("a mirror with no hash ran the helper: %q", got)
	}
	_, hash, _ = prepareMirror(prepare, "", testMirror, strings.Repeat("56", 32))
	if hash != strings.Repeat("56", 32) {
		t.Fatalf("--sha256 = %q, want the flag's", hash)
	}
}

func macOSVMSubCommand(t *testing.T, name string) *cobra.Command {
	t.Helper()
	for _, c := range newMacOSVMCommand().Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("macos-vm has no %s", name)
	return nil
}
