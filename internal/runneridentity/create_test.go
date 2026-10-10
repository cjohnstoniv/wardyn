// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneridentity

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

func TestGenerateSaveAndLoadNeverOverwriteIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runner")
	key, err := Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)), OrgURLSHA256: federation.OrgURLSHA256("https://org.example.com")}
	if err := Save(dir, id); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "https://org.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(dir); err == nil {
		t.Fatal("overwrote an existing key")
	}
	if err := Save(dir, id); err == nil {
		t.Fatal("overwrote registration metadata")
	}
	for _, name := range []string{"runner.key", "runner.json"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0600 {
			t.Fatalf("%s permissions:%o", name, st.Mode().Perm())
		}
	}
}

func TestGenerateRefusesSharedOrSymlinkDirectory(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(shared); err == nil {
		t.Fatal("accepted a shared key directory")
	}
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(link); err == nil {
		t.Fatal("accepted a symlink state directory")
	}
}
