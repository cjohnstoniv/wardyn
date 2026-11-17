// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// A start with macOS VMs off and the setting off removes this Mac's inspection CA through enableMacOSVM.
func TestEnableMacOSVMOffDropsTheInspectionCA(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, err := macvm.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := macvm.Layout{Root: root}.InspectionCADir()
	if _, err := macvm.OpenInspectionCA(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	off := false
	if m := enableMacOSVM(context.Background(), nil, "", "", false, macOSVMSettings{inspectionCA: &off}); m != nil {
		t.Fatalf("enableMacOSVM returned a handle with macOS VMs off")
	}
	if _, err := os.Lstat(filepath.Join(dir, "key.pem")); !os.IsNotExist(err) {
		t.Errorf("key.pem is still there: %v", err)
	}
}
