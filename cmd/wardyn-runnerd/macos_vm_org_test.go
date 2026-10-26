// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Status and prepare on a runner with no runnerd.json say where the file should be and how to write it,
// not the address check's bare refusal; an org the settings or the service definition give is used.
func TestMacVMOrgNamesTheMissingRunnerdJSON(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	path := filepath.Join(cfgDir, "wardyn", "runnerd.json")
	none := func() string { return "" }

	_, err := macVMOrg(runnerConfig{}, none)
	for _, want := range []string{"no runnerd.json was found at " + path, "wardyn-runnerd setup", "org_url"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing file: %v, want %q", err, want)
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := macVMOrg(runnerConfig{}, none); err == nil || !strings.Contains(err.Error(), path+" sets no org_url") || strings.Contains(err.Error(), "no runnerd.json was found") {
		t.Fatalf("file without org_url: %v", err)
	}

	cfg := runnerConfig{{Name: "org_url", Value: "https://org.example"}}
	if org, err := macVMOrg(cfg, func() string { return "https://legacy.example" }); err != nil || org != "https://org.example" {
		t.Fatalf("configured org: %q, %v", org, err)
	}
	if org, err := macVMOrg(runnerConfig{}, func() string { return "https://legacy.example" }); err != nil || org != "https://legacy.example" {
		t.Fatalf("service definition's --org: %q, %v", org, err)
	}
}
