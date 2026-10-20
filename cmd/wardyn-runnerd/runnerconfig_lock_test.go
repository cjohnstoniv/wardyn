// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
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
	if locked, _ := securityLocked(func(k string) string { return map[string]string{"WARDYN_LOCK_SECURITY_SETTINGS": "true"}[k] }); locked {
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
