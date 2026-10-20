// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// withManagedPrefs makes this a Mac whose profile holds prefs; extracted records the files read.
func withManagedPrefs(t *testing.T, goos string, prefs map[string]string) {
	t.Helper()
	oldOS, oldExtract := prefsOS, prefsExtract
	t.Cleanup(func() { prefsOS, prefsExtract = oldOS, oldExtract })
	prefsOS = goos
	prefsExtract = func(file, key string) (string, error) {
		if filepath.Base(file) != prefsDomain+".plist" {
			t.Errorf("read %s, not the profile's preference file", file)
		}
		return prefs[key], nil
	}
}

func serveDefaults(t *testing.T, args ...string) (org string, opts serveOptions) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cmd := newServeCommand(nil)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	cfg, err := serveConfig(cmd, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg.applyTo(&org, new(string), &opts)
	return org, opts
}

func TestServeDefaultsFromManagedPreferences(t *testing.T) {
	withManagedPrefs(t, "darwin", map[string]string{"OrgURL": "https://wardyn.example", "ProxyImage": "ghcr.io/x/wardyn-proxy:1.2.3",
		"RelayListen": "127.0.0.1", "InternalNetwork": "corp-net", "TrustedCAFile": "/Library/Wardyn/ca.pem", "RecordingStore": "off"})
	t.Setenv("WARDYN_PROXY_IMAGE", "from-env")
	t.Setenv("WARDYN_RECORDING_STORE", "fs")
	org, o := serveDefaults(t)
	if org != "https://wardyn.example" || o.proxyImage != "ghcr.io/x/wardyn-proxy:1.2.3" || o.relayListen != "127.0.0.1" ||
		o.network != "corp-net" || o.trustedCAFile != "/Library/Wardyn/ca.pem" || o.recording != "off" {
		t.Fatalf("managed preferences were not the defaults: org=%q %+v", org, o)
	}
	// A flag outranks the profile.
	if org, _ := serveDefaults(t, "--org", "https://other.example"); org != "https://other.example" {
		t.Fatalf("--org lost to the profile: %q", org)
	}
}

func TestServeDefaultsFallBackToEnvironment(t *testing.T) {
	withManagedPrefs(t, "darwin", map[string]string{"OrgURL": "https://wardyn.example"})
	t.Setenv("WARDYN_PROXY_IMAGE", "from-env")
	t.Setenv("WARDYN_INTERNAL_NETWORK", "env-net")
	_, o := serveDefaults(t)
	if o.proxyImage != "from-env" || o.network != "env-net" || o.recording != "pg" {
		t.Fatalf("unset preferences must fall back to the environment, then the built-in default: %+v", o)
	}
}

func TestManagedPreferencesIgnoredOffMac(t *testing.T) {
	withManagedPrefs(t, "linux", map[string]string{"OrgURL": "https://wardyn.example"})
	if org, _ := serveDefaults(t); org != "" {
		t.Fatalf("a non-Mac read a managed preference: %q", org)
	}
}

func TestManagedPreferenceOrgSatisfiesRequiredFlag(t *testing.T) {
	cmd := func() error {
		c := newServeCommand(nil)
		c.SetArgs([]string{"--state-dir", t.TempDir()})
		return c.Execute()
	}
	withManagedPrefs(t, "darwin", nil)
	if err := cmd(); err == nil || err.Error() != `required flag(s) "org" not set` {
		t.Fatalf("without a profile --org stays required: %v", err)
	}
	withManagedPrefs(t, "darwin", map[string]string{"OrgURL": "https://wardyn.example"})
	if err := cmd(); err == nil || err.Error() == `required flag(s) "org" not set` {
		t.Fatalf("a profile's OrgURL did not satisfy --org: %v", err)
	}
}

func TestManagedPreferencesAreReadOnlyWhenServeRuns(t *testing.T) {
	reads := 0
	withManagedPrefs(t, "darwin", map[string]string{"OrgURL": "https://wardyn.example"})
	counted := prefsExtract
	prefsExtract = func(file, key string) (string, error) { reads++; return counted(file, key) }
	newServeCommand(nil)
	newSetupCommand()
	if reads != 0 {
		t.Fatalf("building the commands read %d managed preferences; only serve and setup runs should", reads)
	}
}

func TestSetupOrgDefaultsToManagedPreference(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	run := func() error {
		c := newSetupCommand()
		c.SetArgs(nil)
		c.SilenceUsage, c.SilenceErrors = true, true
		return c.Execute()
	}
	withManagedPrefs(t, "darwin", nil)
	if err := run(); err == nil || err.Error() != `required flag(s) "org" not set` {
		t.Fatalf("without a profile --org stays required: %v", err)
	}
	withManagedPrefs(t, "darwin", map[string]string{"OrgURL": "https://wardyn.example"})
	if err := run(); err == nil || err.Error() == `required flag(s) "org" not set` {
		t.Fatalf("a profile's OrgURL did not satisfy setup's --org: %v", err)
	}
}

func TestServeTakesTheSingleInstanceLock(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "runner")
	key, err := runneridentity.Generate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)), OrgURLSHA256: federation.OrgURLSHA256("https://org.example")}
	if err := runneridentity.Save(stateDir, identity); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockSingleInstance(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	cmd := newServeCommand(func(context.Context, runneridentity.Identity, string, serveOptions) (runnerRuntime, error) {
		called = true
		return runnerRuntime{}, nil
	})
	cmd.SetArgs([]string{"--org", "https://org.example", "--state-dir", stateDir})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "already running") || called {
		t.Fatalf("a second serve was not refused: %v called=%v", err, called)
	}
	unlock()
	if again, err := lockSingleInstance(stateDir); err != nil {
		t.Fatalf("the lock was not released: %v", err)
	} else {
		again()
	}
}

// A proxy image from the managed profile is an administrator's pin, release tag or not; with no profile
// value and no flag, a release-tagged image is only a derived default.
func TestManagedProxyImageIsAPin(t *testing.T) {
	image := releaseProxyRepo + ":1.2.3"
	withManagedPrefs(t, "darwin", map[string]string{"ProxyImage": image})
	given := func() bool {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		cfg, err := effectiveRunnerConfig("", nil)
		if err != nil {
			t.Fatal(err)
		}
		return proxyImageGiven(cfg)
	}
	if !proxyImagePinned(image, "", given(), func(string) string { return "" }) {
		t.Error("a profile's release-tagged proxy image was not a pin")
	}
	withManagedPrefs(t, "darwin", map[string]string{})
	if proxyImagePinned(image, "", given(), func(string) string { return "" }) {
		t.Error("with no profile value and no flag, a release-tagged image was a pin")
	}
}
