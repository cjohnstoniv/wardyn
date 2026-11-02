// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package guestagent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCAFullIsSystemRootsThenTheRunCA(t *testing.T) {
	dir := t.TempDir()
	roots := filepath.Join(dir, "cert.pem")
	if err := os.WriteFile(roots, []byte("ROOTS"), 0o644); err != nil { // no trailing newline
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "ca-bundle.pem")
	if err := writeCAFull(roots, []byte("RUNCA\n"), dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "ROOTS\nRUNCA\n" {
		t.Errorf("bundle = %q", b)
	}
	if err := writeCAFull(filepath.Join(dir, "missing"), []byte("x"), dst); err == nil {
		t.Error("a missing system roots file was accepted")
	}
}

func TestWriteCAFullDoesNotFollowALinkAtTheTempName(t *testing.T) {
	dir := t.TempDir()
	roots := filepath.Join(dir, "cert.pem")
	_ = os.WriteFile(roots, []byte("ROOTS\n"), 0o644)
	victim := filepath.Join(dir, "victim")
	_ = os.WriteFile(victim, []byte("keep"), 0o600)
	dst := filepath.Join(dir, "ca-bundle.pem")
	if err := os.Symlink(victim, dst+".wardyn-tmp"); err != nil {
		t.Fatal(err)
	}
	if err := writeCAFull(roots, []byte("RUNCA\n"), dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("the write followed the link: victim = %q", b)
	}
	if b, _ := os.ReadFile(dst); string(b) != "ROOTS\nRUNCA\n" {
		t.Errorf("bundle = %q", b)
	}
}
