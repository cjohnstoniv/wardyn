// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneridentity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func identityFixture(t *testing.T) (string, Identity) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(pub), OrgURLSHA256: federation.OrgURLSHA256("https://org.example.com")}
	dir := t.TempDir()
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"runner.json": b, "runner.key": key} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, id
}

func TestLoadBindsKeyToOrganisationAndFingerprint(t *testing.T) {
	dir, want := identityFixture(t)
	got, err := Load(dir, "https://ORG.example.com:443/")
	if err != nil || got.RunnerID != want.RunnerID || !got.PrivateKey.Equal(want.PrivateKey) {
		t.Fatalf("identity mismatch: %v", err)
	}
	for _, org := range []string{"https://other.example.com", "https://org.example.com/other", "http://org.example.com", "https://user:password@org.example.com", "https://org.example.com?q=x", "https://org.example.com#x"} {
		if _, err := Load(dir, org); err == nil {
			t.Errorf("accepted %q", org)
		}
	}
}

func TestLoadRefusesUnsafeOrCorruptIdentity(t *testing.T) {
	cases := map[string]func(*testing.T, string){
		"key permissions": func(t *testing.T, d string) {
			t.Helper()
			if err := os.Chmod(filepath.Join(d, "runner.key"), 0644); err != nil {
				t.Fatal(err)
			}
		},
		"metadata permissions": func(t *testing.T, d string) {
			t.Helper()
			if err := os.Chmod(filepath.Join(d, "runner.json"), 0644); err != nil {
				t.Fatal(err)
			}
		},
		"wrong key": func(t *testing.T, d string) {
			t.Helper()
			_, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "runner.key"), key, 0600); err != nil {
				t.Fatal(err)
			}
		},
		"short key": func(t *testing.T, d string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(d, "runner.key"), []byte("bad"), 0600); err != nil {
				t.Fatal(err)
			}
		},
		"key symlink": func(t *testing.T, d string) {
			t.Helper()
			if err := os.Rename(filepath.Join(d, "runner.key"), filepath.Join(d, "other.key")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("other.key", filepath.Join(d, "runner.key")); err != nil {
				t.Fatal(err)
			}
		},
		"unregistered": func(t *testing.T, d string) {
			t.Helper()
			b, _ := json.Marshal(Identity{OrgURLSHA256: federation.OrgURLSHA256("https://org.example.com")})
			if err := os.WriteFile(filepath.Join(d, "runner.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := identityFixture(t)
			mutate(t, dir)
			if _, err := Load(dir, "https://org.example.com"); err == nil {
				t.Fatal("unsafe identity accepted")
			}
		})
	}
}
