// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
)

func (f *setupFixture) cliPaths() setupPaths {
	f.t.Helper()
	p, err := f.host.paths()
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

// Setup points the CLI profile at the organisation, and installs no CLI of its own: a wardyn binary that
// happens to sit beside the runner is left where it is (the CLI is the person's, installed before the runner).
func TestSetupSeedsTheCLIProfile(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSetupFixture(t, goos)
			beside := filepath.Join(filepath.Dir(f.host.exe), "wardyn")
			if err := os.WriteFile(beside, []byte("cli-binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			out, err := f.run(setupOptions{tokenFile: f.tokenFile})
			if err != nil {
				t.Fatalf("setup: %v\n%s", err, out)
			}
			p := f.cliPaths()
			if got, err := cliprofile.Read(p.cliProfile); err != nil || got != f.org {
				t.Errorf("CLI profile = %q, %v; want the organisation %s", got, err, f.org)
			}
			if !strings.Contains(out, "wardyn login <url>") {
				t.Errorf("setup did not say how to change the seeded profile:\n%s", out)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(p.installed), "wardyn")); err == nil {
				t.Errorf("setup installed a wardyn CLI beside the runner")
			}
		})
	}
}

// A profile that already names a URL is the person's choice and stays, and so does its artifact base.
func TestSetupSeedKeepsAnExistingProfile(t *testing.T) {
	f := newSetupFixture(t, "linux")
	p := f.cliPaths()
	if err := cliprofile.Write(p.cliProfile, "https://other.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := cliprofile.WriteArtifactBase(p.cliProfile, "https://downloads.example.com"); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if got, _ := cliprofile.Read(p.cliProfile); got != "https://other.example.com" {
		t.Errorf("a profile the person set was overwritten with %q", got)
	}
	if got, _ := cliprofile.ReadArtifactBase(p.cliProfile); got != "https://downloads.example.com" {
		t.Errorf("the artifact base was lost: %q", got)
	}
	if !strings.Contains(out, "left it as it is") {
		t.Errorf("setup did not say it kept the profile:\n%s", out)
	}
}

// A profile with no URL (only an artifact base) is seeded, and the base is kept.
func TestSetupSeedFillsAMissingURLAndKeepsTheBase(t *testing.T) {
	f := newSetupFixture(t, "linux")
	p := f.cliPaths()
	if err := cliprofile.WriteArtifactBase(p.cliProfile, "https://downloads.example.com"); err != nil {
		t.Fatal(err)
	}
	if out, err := f.run(setupOptions{tokenFile: f.tokenFile}); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if got, _ := cliprofile.Read(p.cliProfile); got != f.org {
		t.Errorf("url = %q, want the organisation %s", got, f.org)
	}
	if got, _ := cliprofile.ReadArtifactBase(p.cliProfile); got != "https://downloads.example.com" {
		t.Errorf("the artifact base was lost: %q", got)
	}
}
