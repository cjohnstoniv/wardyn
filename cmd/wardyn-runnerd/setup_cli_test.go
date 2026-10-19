// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
)

// beside puts a stand-in wardyn CLI next to the fixture's downloaded runnerd, as the release bundle does.
func (f *setupFixture) beside(content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(f.host.exe), "wardyn"), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *setupFixture) cliPaths() setupPaths {
	f.t.Helper()
	p, err := f.host.paths()
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

// A CLI that sits beside the runner is installed next to it, executable, and the CLI profile is pointed at
// the organisation; setup downloads nothing.
func TestSetupInstallsTheCLIBesideItAndSeedsTheProfile(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			f := newSetupFixture(t, goos)
			f.beside("cli-binary")
			out, err := f.run(setupOptions{tokenFile: f.tokenFile})
			if err != nil {
				t.Fatalf("setup: %v\n%s", err, out)
			}
			p := f.cliPaths()
			if got := f.read(p.cliInstalled); got != "cli-binary" {
				t.Errorf("installed CLI = %q, want the one beside the runner", got)
			}
			if st, _ := os.Stat(p.cliInstalled); st == nil || st.Mode().Perm() != 0o755 {
				t.Errorf("installed CLI mode = %v, want 0755", st)
			}
			if got, err := cliprofile.Read(p.cliProfile); err != nil || got != f.org {
				t.Errorf("CLI profile = %q, %v; want the organisation %s", got, err, f.org)
			}
			if !strings.Contains(out, "Installed the wardyn CLI") || !strings.Contains(out, "wardyn auth set-url") {
				t.Errorf("setup did not say what it installed and seeded:\n%s", out)
			}
		})
	}
}

// No CLI beside the runner: nothing is installed or fetched, and the profile is still pointed at the
// organisation (the CLI may be installed some other way).
func TestSetupWithNoCLIBesideItInstallsNone(t *testing.T) {
	f := newSetupFixture(t, "linux")
	if out, err := f.run(setupOptions{tokenFile: f.tokenFile}); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	p := f.cliPaths()
	if _, err := os.Stat(p.cliInstalled); err == nil {
		t.Errorf("setup installed a CLI that was not beside it: %s", p.cliInstalled)
	}
	if got, _ := cliprofile.Read(p.cliProfile); got != f.org {
		t.Errorf("CLI profile = %q, want %s", got, f.org)
	}
}

// An installed CLI of a later release is left alone; an older one is replaced. A profile that already names
// a URL is the person's choice and stays.
func TestSetupLeavesANewerCLIAndAProfileAlone(t *testing.T) {
	f := newSetupFixture(t, "linux")
	f.beside("new-cli")
	p := f.cliPaths()
	if err := os.MkdirAll(filepath.Dir(p.cliInstalled), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.cliInstalled, []byte("later-cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cliprofile.Write(p.cliProfile, "https://other.example.com"); err != nil {
		t.Fatal(err)
	}
	reported := "wardyn version 99.0.0"
	inner := f.host.run
	f.host.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == p.cliInstalled {
			return []byte(reported), nil
		}
		return inner(ctx, name, args...)
	}
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if got := f.read(p.cliInstalled); got != "later-cli" {
		t.Errorf("a newer installed CLI was replaced by %q", got)
	}
	if got, _ := cliprofile.Read(p.cliProfile); got != "https://other.example.com" {
		t.Errorf("a profile the person set was overwritten with %q", got)
	}

	reported = "wardyn version 0.1.0"
	if out, err := f.run(setupOptions{tokenFile: f.tokenFile}); err != nil {
		t.Fatalf("setup again: %v\n%s", err, out)
	}
	if got := f.read(p.cliInstalled); got != "new-cli" {
		t.Errorf("an older installed CLI was kept: %q", got)
	}
}

func TestNewerThanThisRelease(t *testing.T) {
	for line, want := range map[string]bool{
		"wardyn version 99.0.0":       true,
		"wardyn version 0.9.5+abc123": false,
		"wardyn version 0.1.0":        false,
		"":                            false,
		"not a version":               false,
	} {
		if got := newerThanThisRelease(line); got != want {
			t.Errorf("newerThanThisRelease(%q) = %v, want %v", line, got, want)
		}
	}
}
