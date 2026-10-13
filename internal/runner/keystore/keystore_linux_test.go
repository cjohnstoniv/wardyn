// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keystore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// With no session bus address and no user bus socket, the keystore refuses
// before the keyring is touched, so godbus never falls back to running
// dbus-launch: a dbus-launch first on PATH would leave a marker if it ran.
func TestKeystore_NoSessionBusRefusesWithoutRunningATool(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "ran")
	if err := os.WriteFile(filepath.Join(bin, "dbus-launch"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	empty := t.TempDir()
	prevDir, prevSet, prevGet, prevDel := runtimeDir, keyringSet, keyringGet, keyringDelete
	t.Cleanup(func() { runtimeDir, keyringSet, keyringGet, keyringDelete = prevDir, prevSet, prevGet, prevDel })
	runtimeDir = func() string { return empty }
	reached := false
	keyringSet = func(service, account, value string) error { reached = true; return prevSet(service, account, value) }
	keyringGet = func(service, account string) (string, error) { reached = true; return prevGet(service, account) }
	keyringDelete = func(service, account string) error { reached = true; return prevDel(service, account) }

	if err := Set("run/grant", "value"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Set = %v, want ErrUnavailable", err)
	}
	if _, err := Get("run/grant"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Get = %v, want ErrUnavailable", err)
	}
	if err := Delete("run/grant"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Delete = %v, want ErrUnavailable", err)
	}
	if reached {
		t.Fatal("the keyring was reached with no session bus")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("dbus-launch ran")
	}
}
