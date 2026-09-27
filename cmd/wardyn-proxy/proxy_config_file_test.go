// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func validProxyConfigJSON(t *testing.T) []byte {
	t.Helper()
	// Field names mirror internal/egress/proxy.Config's json tags — a real
	// config the sidecar would otherwise receive over WARDYN_PROXY_CONFIG_JSON.
	return []byte(`{
		"run_id":            "` + uuid.New().String() + `",
		"control_plane_url": "http://127.0.0.1:8080",
		"run_token":         "probe-run-token",
		"listen":            ":3128"
	}`)
}

// TestStageProxyConfigCreatesAnOwnerOnlyFile is the k8s init-container step's
// pinning test (T-28, issue #688): staging a valid config JSON from src to
// dst must leave dst readable, containing the exact bytes read from src, and
// at mode 0400 — owner-read-only, no group or other bit — regardless of the
// process umask or the source file's own (more permissive) mode.
func TestStageProxyConfigCreatesAnOwnerOnlyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source-config.json")
	dst := filepath.Join(dir, "staged", "config.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	want := validProxyConfigJSON(t)
	if err := os.WriteFile(src, want, 0o440); err != nil {
		t.Fatalf("write src: %v", err)
	}

	if err := StageProxyConfig(src, dst); err != nil {
		t.Fatalf("StageProxyConfig: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read staged dst: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("staged content = %q, want %q", got, want)
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o400 {
		t.Errorf("staged file mode = %o, want 0400 (owner-read-only)", mode)
	}

	// No leftover temp file: the atomic rename must not litter dst's directory.
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("staged dir has %d entries, want exactly 1 (the staged file, no temp leftovers): %v", len(entries), entries)
	}
}

// TestStageProxyConfig_RefusesInvalidJSONAndLeavesNoFile is the fail-closed
// half: a src that does not parse as a valid proxy config must not reach
// dst at all — an init container that stages garbage only defers the
// failure to the main container's own boot, one privilege drop later.
func TestStageProxyConfig_RefusesInvalidJSONAndLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "source-config.json")
	dst := filepath.Join(dir, "config.json")
	if err := os.WriteFile(src, []byte(`{"not": "a valid proxy config"}`), 0o440); err != nil {
		t.Fatalf("write src: %v", err)
	}

	if err := StageProxyConfig(src, dst); err == nil {
		t.Fatal("StageProxyConfig: want an error on an invalid config, got nil")
	}

	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst exists after a refused stage: stat err = %v, want IsNotExist", err)
	}
}

// TestStageProxyConfig_MissingSrcFailsClosed proves the same for a source
// that simply is not there — the init container's own volume-mount
// misconfiguration must fail the pod, not silently proceed with no config.
func TestStageProxyConfig_MissingSrcFailsClosed(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "does-not-exist.json")
	dst := filepath.Join(dir, "config.json")

	if err := StageProxyConfig(src, dst); err == nil {
		t.Fatal("StageProxyConfig: want an error on a missing src, got nil")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst exists after a missing-src stage: stat err = %v, want IsNotExist", err)
	}
}
