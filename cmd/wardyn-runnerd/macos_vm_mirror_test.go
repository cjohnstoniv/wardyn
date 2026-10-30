// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
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
