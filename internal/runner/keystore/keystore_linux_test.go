// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package keystore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	prevDir, prevSet, prevGet, prevDel, prevAll := runtimeDir, keyringSet, keyringGet, keyringDelete, keyringDeleteAll
	t.Cleanup(func() {
		runtimeDir, keyringSet, keyringGet, keyringDelete, keyringDeleteAll = prevDir, prevSet, prevGet, prevDel, prevAll
	})
	runtimeDir = func() string { return empty }
	reached := false
	keyringSet = func(service, account, value string) error { reached = true; return prevSet(service, account, value) }
	keyringGet = func(service, account string) (string, error) { reached = true; return prevGet(service, account) }
	keyringDelete = func(service, account string) error { reached = true; return prevDel(service, account) }
	keyringDeleteAll = func(service string) error { reached = true; return prevAll(service) }

	s := For("runner")
	if err := s.Precheck("run/grant", "value"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Precheck = %v, want ErrUnavailable", err)
	}
	if err := s.Set("run/grant", "value"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Set = %v, want ErrUnavailable", err)
	}
	if _, err := s.Get("run/grant"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Get = %v, want ErrUnavailable", err)
	}
	if err := s.Delete("run/grant"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Delete = %v, want ErrUnavailable", err)
	}
	if err := s.DeleteAll(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("DeleteAll = %v, want ErrUnavailable", err)
	}
	if reached {
		t.Fatal("the keyring was reached with no session bus")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("dbus-launch ran")
	}
}

// Every item lives under its runner's own service, so two runner daemons for
// one OS user never write or delete each other's items, and DeleteAll removes
// only its own runner's.
func TestStore_DeleteAllScopedToRunner(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent")
	prevSet, prevGet, prevDel, prevAll := keyringSet, keyringGet, keyringDelete, keyringDeleteAll
	t.Cleanup(func() { keyringSet, keyringGet, keyringDelete, keyringDeleteAll = prevSet, prevGet, prevDel, prevAll })
	var services []string
	keyringSet = func(service, _, _ string) error { services = append(services, service); return nil }
	keyringGet = func(service, _ string) (string, error) { services = append(services, service); return "", nil }
	keyringDelete = func(service, _ string) error { services = append(services, service); return nil }
	keyringDeleteAll = func(service string) error { services = append(services, service); return nil }
	for _, id := range []string{"runner-a", "runner-b"} {
		s := For(id)
		if err := s.Set("run/grant", "v"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get("run/grant"); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete("run/grant"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteAll(); err != nil {
			t.Fatal(err)
		}
	}
	a, b := "wardyn-runnerd:runner-a", "wardyn-runnerd:runner-b"
	want := []string{a, a, a, a, b, b, b, b}
	if strings.Join(services, ",") != strings.Join(want, ",") {
		t.Fatalf("services = %v, want %v", services, want)
	}
}
