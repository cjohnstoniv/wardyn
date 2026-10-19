// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --no-service is refused, before the token is spent or the environment file written, while a unit
// is installed under $XDG_CONFIG_HOME/systemd/user, where a service setup puts it when the user
// manager searches there first: paths() alone names only the HOME unit.
func TestSetupNoServiceRefusesWhileAnXDGUnitIsInstalled(t *testing.T) {
	f := newSetupFixture(t, "linux")
	f.host.xdgConfig = filepath.Join(filepath.Dir(f.host.home), "xdg")
	unit := filepath.Join(f.host.xdgConfig, "systemd", "user", serviceUnit)
	if err := os.MkdirAll(filepath.Dir(unit), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Unit]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _ := f.host.paths()

	_, err := f.run(setupOptions{tokenFile: f.tokenFile, noService: true})
	if err == nil || !strings.Contains(err.Error(), "already installed") || !strings.Contains(err.Error(), unit) {
		t.Fatalf("--no-service beside an XDG unit = %v, want a refusal naming %s", err, unit)
	}
	if n := f.count("POST /api/v1/runners/register"); n != 0 {
		t.Errorf("the refusal came after %d registrations", n)
	}
	if _, err := os.Stat(p.noServiceEnvFile()); err == nil {
		t.Error("the refusal came after the environment file was written")
	}
}
