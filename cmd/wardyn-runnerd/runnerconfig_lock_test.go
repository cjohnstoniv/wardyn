// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The security settings the managed profile can lock, and a managed value for each.
var (
	lockedNames = []string{"org_url", "proxy_image", "trusted_ca_file", "org_ca_file", "recording_store"}
	lockManaged = map[string]string{"OrgURL": "https://m.example", "ProxyImage": "m-img", "TrustedCAFile": "/m/ca.pem", "OrgCAFile": "/m/org.pem", "RecordingStore": "pg"}
	lockFlags   = map[string]string{"org": "f-org", "proxy-image": "f-img", "trusted-ca-file": "/f/ca.pem", "org-ca-file": "/f/org.pem", "recording-store": "fs"}
	lockEnv     = map[string]string{"WARDYN_PROXY_IMAGE": "e-img", "WARDYN_TRUSTED_CA_FILE": "/e/ca.pem", "WARDYN_ORG_CA_FILE": "/e/org.pem", "WARDYN_RECORDING_STORE": "off"}
)

func withLock(lock string) map[string]string {
	m := map[string]string{lockSettingKey: lock}
	for k, v := range lockManaged {
		m[k] = v
	}
	return m
}

func fiveKeys(in runnerInputs) string {
	var b strings.Builder
	cfg := resolveRunnerConfig(in)
	for _, n := range lockedNames {
		f := cfg.field(n)
		fmt.Fprintf(&b, "%s=%s (%s)\n", f.Name, f.Value, f.Source)
	}
	return b.String()
}

// Without the lock the order is what it was before the lock existed, for all five security settings.
func TestSecurityLockOffKeepsTodaysOrder(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "runnerconfig_lock_off.golden"))
	if err != nil {
		t.Fatal(err)
	}
	for _, lock := range []string{"", "false", "0"} {
		got := "# flags, environment and managed preferences\n" + fiveKeys(inputs(lockFlags, lockEnv, withLock(lock), runnerFile{})) +
			"# environment and managed preferences\n" + fiveKeys(inputs(nil, lockEnv, withLock(lock), runnerFile{})) +
			"# environment only\n" + fiveKeys(inputs(nil, lockEnv, nil, runnerFile{}))
		if got != string(want) {
			t.Errorf("lock %q changed the order:\n%s\nwant:\n%s", lock, got, want)
		}
		if err := checkSecurityLock(inputs(lockFlags, lockEnv, withLock(lock), runnerFile{})); err != nil {
			t.Errorf("lock %q refused a disagreeing flag: %v", lock, err)
		}
	}
}

// With the lock the managed value is the value, over a flag and an environment variable alike, for exactly
// the five security settings.
func TestSecurityLockManagedOutranksFlagAndEnv(t *testing.T) {
	var locked []string
	for _, f := range runnerFields {
		if f.locked {
			locked = append(locked, f.name)
		}
	}
	if !slices.Equal(locked, lockedNames) {
		t.Fatalf("locked settings = %v, want %v", locked, lockedNames)
	}
	names := map[string]string{"org_url": "OrgURL", "proxy_image": "ProxyImage", "trusted_ca_file": "TrustedCAFile", "org_ca_file": "OrgCAFile", "recording_store": "RecordingStore"}
	for _, in := range []runnerInputs{inputs(lockFlags, lockEnv, withLock("true"), runnerFile{}), inputs(nil, lockEnv, withLock("1"), runnerFile{})} {
		cfg := resolveRunnerConfig(in)
		for _, n := range lockedNames {
			if got := cfg.field(n); got.Value != lockManaged[names[n]] || got.Source != "managed preference "+names[n]+" (locked)" {
				t.Errorf("%s = %+v, want the managed value", n, got)
			}
		}
	}
	// A key the profile leaves empty is not locked: its flag still applies.
	m := withLock("true")
	delete(m, "ProxyImage")
	if got := resolveRunnerConfig(inputs(lockFlags, lockEnv, m, runnerFile{})).field("proxy_image"); got.Value != "f-img" {
		t.Errorf("an unset managed key was locked: %+v", got)
	}
}

func serveConfigErr(t *testing.T, args ...string) error {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cmd := newServeCommand(nil)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	_, err := serveConfig(cmd, "")
	return err
}

// With the lock, start refuses a flag or environment value that disagrees, names the setting and the
// disagreeing source, and repeats no value; an agreeing one is accepted.
func TestSecurityLockRefusesDisagreeingFlagOrEnv(t *testing.T) {
	withManagedPrefs(t, "darwin", withLock("true"))
	for flag, value := range lockFlags {
		err := serveConfigErr(t, "--"+flag, value)
		if err == nil || !strings.Contains(err.Error(), lockSettingKey) || !strings.Contains(err.Error(), "--"+flag) || strings.Contains(err.Error(), value) {
			t.Errorf("--%s: %v; want a refusal naming %s and the flag, without the value", flag, err, lockSettingKey)
		}
	}
	for env, value := range lockEnv {
		t.Setenv(env, value)
		err := serveConfigErr(t)
		if err == nil || !strings.Contains(err.Error(), lockSettingKey) || !strings.Contains(err.Error(), env) || strings.Contains(err.Error(), value) {
			t.Errorf("%s: %v; want a refusal naming %s and the variable, without the value", env, err, lockSettingKey)
		}
		t.Setenv(env, "")
	}
	if err := serveConfigErr(t, "--proxy-image", "m-img", "--org", " https://m.example "); err != nil {
		t.Errorf("a flag that agrees with the profile was refused: %v", err)
	}
	// The whole command stops at start, before it contacts anything.
	cmd := newServeCommand(nil)
	cmd.SetArgs([]string{"--state-dir", t.TempDir(), "--proxy-image", "other-img"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), lockSettingKey) {
		t.Errorf("serve start: %v; want a refusal naming %s", err, lockSettingKey)
	}
}

// A value that is neither true nor false in the profile locks and refuses to start, so a typo never leaves the
// settings open. Off a Mac there is no profile, so nothing is locked.
func TestSecurityLockMalformedAndOffMac(t *testing.T) {
	withManagedPrefs(t, "darwin", withLock("ture"))
	if err := serveConfigErr(t); err == nil || !strings.Contains(err.Error(), lockSettingKey) {
		t.Errorf("a malformed lock value: %v; want a refusal naming %s", err, lockSettingKey)
	}
	if got := resolveRunnerConfig(inputs(lockFlags, nil, withLock("ture"), runnerFile{})).get("proxy_image"); got != "m-img" {
		t.Errorf("a malformed lock value left proxy_image open: %q", got)
	}
	withManagedPrefs(t, "linux", withLock("true"))
	if err := serveConfigErr(t, "--proxy-image", "other-img"); err != nil {
		t.Errorf("a non-Mac read a lock: %v", err)
	}
}

// With the lock, a setting that is not one of the five keeps flag > managed > environment.
func TestSecurityLockLeavesLowImpactKeysAlone(t *testing.T) {
	m := withLock("true")
	m["InternalNetwork"], m["RelayListen"], m["RelayHostTarget"], m["DockerHost"] = "m-net", "m-listen", "m-target", "m-docker"
	flags := map[string]string{"internal-network": "f-net", "relay-listen": "f-listen", "relay-host-target": "f-target"}
	env := map[string]string{"DOCKER_HOST": "e-docker", "WARDYN_INTERNAL_NETWORK": "e-net"}
	if err := checkSecurityLock(inputs(flags, env, m, runnerFile{})); err != nil {
		t.Fatalf("a low-impact flag was refused: %v", err)
	}
	cfg := resolveRunnerConfig(inputs(flags, env, m, runnerFile{}))
	for n, want := range map[string]string{"internal_network": "f-net", "relay_listen": "f-listen", "relay_host_target": "f-target", "docker_host": "m-docker"} {
		if got := cfg.get(n); got != want {
			t.Errorf("%s = %q, want %q", n, got, want)
		}
	}
	if got := resolveRunnerConfig(inputs(nil, env, m, runnerFile{})).field("internal_network"); got.Value != "m-net" || strings.Contains(got.Source, "locked") {
		t.Errorf("internal_network without a flag = %+v, want the managed value as before", got)
	}
}

// Only the managed profile can set the lock: no flag, environment variable or runnerd.json field reads it.
func TestSecurityLockHasNoFlagEnvOrFileTwin(t *testing.T) {
	withManagedPrefs(t, "darwin", lockManaged)
	for _, name := range []string{"lock-security-settings", "lock_security_settings"} {
		if newServeCommand(nil).Flags().Lookup(name) != nil || newSetupCommand().Flags().Lookup(name) != nil {
			t.Errorf("--%s exists", name)
		}
	}
	for _, env := range []string{"WARDYN_LOCK_SECURITY_SETTINGS", "LOCK_SECURITY_SETTINGS", "lock_security_settings"} {
		t.Setenv(env, "true")
	}
	if err := serveConfigErr(t, "--proxy-image", "other-img"); err != nil {
		t.Errorf("an environment variable switched the lock on: %v", err)
	}
	if locked, _ := securityLocked(runnerInputs{managed: func(k string) string { return map[string]string{"WARDYN_LOCK_SECURITY_SETTINGS": "true"}[k] }}); locked {
		t.Error("the lock read something other than the managed key")
	}
	if _, err := loadRunnerFile(writeConfig(t, `{"lock_security_settings": true}`), true); err == nil {
		t.Error("runnerd.json accepted lock_security_settings")
	}
	for _, f := range runnerFields {
		if f.name == lockSettingKey || f.managed == lockSettingKey {
			t.Errorf("%s is a runnerd setting", f.name)
		}
	}
}

// A profile that is there but cannot be read must not read as one without the lock: start is refused, naming
// the setting, whichever of the two profile files fails. Other keys keep reading a failed profile as unset.
func TestSecurityLockUnreadableProfileRefusesStart(t *testing.T) {
	withManagedPrefs(t, "darwin", withLock("false"))
	prefsRoot = "/Managed Preferences"
	for _, failing := range []string{"user", "device"} {
		prefsExtract = func(file, key string) (string, error) {
			if (failing == "device") == (filepath.Dir(file) == prefsRoot) {
				return "", fmt.Errorf("permission denied")
			}
			return "", nil
		}
		if err := serveConfigErr(t); err == nil || !strings.Contains(err.Error(), lockSettingKey) {
			t.Errorf("%s profile unreadable: %v; want a refusal naming %s", failing, err, lockSettingKey)
		}
	}
	if got := managedPref("OrgURL"); got != "" {
		t.Errorf("a failed read of OrgURL = %q, want unset", got)
	}
}

// writePlutil is a stand-in for /usr/bin/plutil whose -extract and -lint each run the given shell command.
func writePlutil(t *testing.T, extract, lint string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	old := plutilPath
	t.Cleanup(func() { plutilPath = old })
	plutilPath = filepath.Join(t.TempDir(), "plutil")
	script := "#!/bin/sh\ncase \"$1\" in\n-extract) " + extract + " ;;\n-lint) " + lint + " ;;\nesac\n"
	if err := os.WriteFile(plutilPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Only a missing file, or a readable plist without the key, is unset; every other failure is an error.
func TestPlutilExtractFailsClosed(t *testing.T) {
	plist := filepath.Join(t.TempDir(), "p.plist")
	if err := os.WriteFile(plist, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	writePlutil(t, "echo true", "exit 0")
	if v, err := plutilExtract(plist, lockSettingKey); v != "true" || err != nil {
		t.Errorf("a set key = %q, %v", v, err)
	}
	if v, err := plutilExtract(plist+".missing", lockSettingKey); v != "" || err != nil {
		t.Errorf("a missing file = %q, %v; want unset", v, err)
	}
	if _, err := plutilExtract(filepath.Join(plist, "below-a-file"), lockSettingKey); err == nil {
		t.Error("a stat error other than not-exist read as unset")
	}
	writePlutil(t, "exit 1", "exit 0")
	if v, err := plutilExtract(plist, lockSettingKey); v != "" || err != nil {
		t.Errorf("an absent key in a readable plist = %q, %v; want unset", v, err)
	}
	writePlutil(t, "exit 1", "exit 1")
	if _, err := plutilExtract(plist, lockSettingKey); err == nil {
		t.Error("a plist plutil cannot read read as unset")
	}
	writePlutil(t, "kill -9 $$", "exit 0")
	if _, err := plutilExtract(plist, lockSettingKey); err == nil {
		t.Error("an extract a signal ended read as unset")
	}
	writePlutil(t, "exit 2", "exit 0")
	if _, err := plutilExtract(plist, lockSettingKey); err == nil {
		t.Error("an extract that exited 2 read as unset")
	}
	plutilPath = filepath.Join(t.TempDir(), "no-such-plutil")
	if _, err := plutilExtract(plist, lockSettingKey); err == nil {
		t.Error("a plutil that could not run read as unset")
	}
}

// With the lock on, setup refuses a disagreeing flag before it dials out, naming the setting.
func TestSecurityLockSetupRefusesDisagreeingFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	withManagedPrefs(t, "darwin", withLock("true"))
	for _, args := range [][]string{{"--org", "https://other.example"}, {"--org", "https://m.example", "--proxy-image", "other-img"}} {
		c := newSetupCommand()
		c.SetArgs(args)
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err == nil || !strings.Contains(err.Error(), lockSettingKey) {
			t.Errorf("setup %v: %v; want a refusal naming %s", args, err, lockSettingKey)
		}
	}
}
