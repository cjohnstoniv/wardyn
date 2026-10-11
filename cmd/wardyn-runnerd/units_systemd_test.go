// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && linux

package main

import (
	"crypto/ed25519"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

const unitOrg = "http://127.0.0.1:9"

// run runs a command and returns its trimmed output; systemctl exits non-zero
// for "inactive", so callers that expect that read the output and ignore err.
func run(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TestRunnerdUserUnitStartsAndStops installs the real per-user unit with
// deploy/runner/install.sh and drives it through the invoking user's systemd
// manager, so it only runs where that is disposable (CI's runner-units job).
func TestRunnerdUserUnitStartsAndStops(t *testing.T) {
	if os.Getenv("WARDYN_TEST_SYSTEMD_USER") != "1" {
		t.Skip("set WARDYN_TEST_SYSTEMD_USER=1 on a host whose user systemd manager may be used")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local", "share")
	}
	unitFile := filepath.Join(cfg, "systemd", "user", "wardyn-runnerd.service")
	envFile := filepath.Join(home, ".config", "wardyn", "runnerd.env")
	installed := filepath.Join(dataHome, "wardyn", "bin", "wardyn-runnerd")
	stateDir, err := runneridentity.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{unitFile, installed, filepath.Join(stateDir, "runner.json")} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("refusing to overwrite an existing install: %s", p)
		}
	}

	bin := filepath.Join(t.TempDir(), "wardyn-runnerd")
	if out, err := run(t, "go", "build", "-tags", "docker", "-o", bin, "."); err != nil {
		t.Fatalf("build wardyn-runnerd: %v\n%s", err, out)
	}

	// A registered identity bound to an unreachable organisation: serve keeps
	// retrying, so the unit must stay up rather than exit.
	key, err := runneridentity.Generate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = run(t, "systemctl", "--user", "disable", "--now", "wardyn-runnerd.service")
		for _, p := range []string{unitFile, envFile, installed, filepath.Join(stateDir, "runner.json"), filepath.Join(stateDir, "runner.key")} {
			_ = os.Remove(p)
		}
		_ = os.Remove(stateDir)
	})
	id := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)), OrgURLSHA256: federation.OrgURLSHA256(unitOrg)}
	if err := runneridentity.Save(stateDir, id); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join("..", "..", "deploy", "runner", "install.sh")
	if out, err := run(t, "bash", script, "--org", unitOrg, "--binary", bin, "--proxy-image", "busybox:latest"); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, out)
	}

	state := func() string { s, _ := run(t, "systemctl", "--user", "is-active", "wardyn-runnerd"); return s }
	mainPID := func() string {
		s, _ := run(t, "systemctl", "--user", "show", "-p", "MainPID", "--value", "wardyn-runnerd")
		return s
	}
	if got := state(); got != "active" {
		t.Fatalf("after install the unit is %q, want active", got)
	}
	pid := mainPID()
	if pid == "" || pid == "0" {
		t.Fatalf("no MainPID after install: %q", pid)
	}
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(time.Second) {
		if got, now := state(), mainPID(); got != "active" || now != pid {
			t.Fatalf("unit left service: state=%q MainPID %s -> %s", got, pid, now)
		}
	}

	if out, err := run(t, "systemctl", "--user", "stop", "wardyn-runnerd"); err != nil {
		t.Fatalf("stop: %v\n%s", err, out)
	}
	if got := state(); got != "inactive" {
		t.Fatalf("after stop the unit is %q, want inactive", got)
	}

	if out, err := run(t, "bash", script, "--uninstall"); err != nil {
		t.Fatalf("install.sh --uninstall: %v\n%s", err, out)
	}
	for _, p := range []string{unitFile, envFile, installed} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("uninstall left %s", p)
		}
	}
	for _, f := range []string{"runner.json", "runner.key"} {
		if _, err := os.Stat(filepath.Join(stateDir, f)); err != nil {
			t.Errorf("uninstall removed the runner identity file %s: %v", f, err)
		}
	}
}
