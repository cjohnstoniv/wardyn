// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSecretValue = "s3cret-VALUE-never-echoed"

// writeSecret writes content to a fresh file with mode perm and returns its path.
func writeSecret(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil { // umask-proof
		t.Fatal(err)
	}
	return p
}

// resolveOne runs resolveSecretFiles over one synthetic setting whose _FILE
// twin points at path, with plain as the already-resolved plain value.
func resolveOne(t *testing.T, plain, path string) (string, error) {
	t.Helper()
	t.Setenv("WARDYN_TEST_STR_FILE", path)
	v := plain
	err := resolveSecretFiles([]secretFileSetting{{"WARDYN_TEST_STR", "WARDYN_TEST_STR_FILE", &v}})
	return v, err
}

func assertNoValue(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), testSecretValue) {
		t.Fatalf("error echoes the secret value: %v", err)
	}
}

func TestSecretFile_UnsetKeepsPlainValue(t *testing.T) {
	v, err := resolveOne(t, "from-env", "")
	if err != nil || v != "from-env" {
		t.Fatalf("got (%q, %v), want (\"from-env\", nil)", v, err)
	}
}

func TestSecretFile_ReadsAndTrimsOneTrailingNewline(t *testing.T) {
	for _, tc := range []struct{ content, want string }{
		{testSecretValue + "\n", testSecretValue},
		{testSecretValue + "\r\n", testSecretValue},
		{testSecretValue, testSecretValue},
		// Only ONE line ending goes; a second is part of the value (a PEM or
		// JSON body keeps its shape), as it would be in the env var.
		{testSecretValue + "\n\n", testSecretValue + "\n"},
		{" " + testSecretValue + " \n", " " + testSecretValue + " "},
	} {
		v, err := resolveOne(t, "", writeSecret(t, tc.content, 0o440))
		if err != nil || v != tc.want {
			t.Fatalf("content %q: got (%q, %v), want %q", tc.content, v, err, tc.want)
		}
	}
}

func TestSecretFile_BothSetRefusesAndNamesBoth(t *testing.T) {
	_, err := resolveOne(t, testSecretValue, writeSecret(t, "other\n", 0o400))
	if err == nil {
		t.Fatal("plain value AND _FILE both set: want a boot refusal, got nil")
	}
	for _, name := range []string{"WARDYN_TEST_STR ", "WARDYN_TEST_STR_FILE"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("refusal %q does not name %q", err, name)
		}
	}
	assertNoValue(t, err)
}

func TestSecretFile_EmptyFileRefuses(t *testing.T) {
	for _, content := range []string{"", "\n", "  \r\n"} {
		p := writeSecret(t, content, 0o400)
		_, err := resolveOne(t, "", p)
		if err == nil || !strings.Contains(err.Error(), "WARDYN_TEST_STR_FILE") || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "empty") {
			t.Fatalf("content %q: want an 'empty' refusal naming the var and path, got %v", content, err)
		}
	}
}

func TestSecretFile_UnreadableFileRefuses(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	_, err := resolveOne(t, "", missing)
	if err == nil || !strings.Contains(err.Error(), "WARDYN_TEST_STR_FILE") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing file: want a refusal naming the var and path, got %v", err)
	}

	// A directory stats fine and fails the read — the "exists but cannot be
	// read" branch, without depending on running as non-root.
	dir := t.TempDir()
	_, err = resolveOne(t, "", dir)
	if err == nil || !strings.Contains(err.Error(), "WARDYN_TEST_STR_FILE") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("directory: want a refusal naming the var and path, got %v", err)
	}

	if os.Geteuid() != 0 {
		p := writeSecret(t, testSecretValue, 0o000)
		_, err = resolveOne(t, "", p)
		if err == nil || !strings.Contains(err.Error(), p) {
			t.Fatalf("mode 0000: want a refusal naming the path, got %v", err)
		}
		assertNoValue(t, err)
	}
}

func TestSecretFile_GroupOrWorldWritableRefuses(t *testing.T) {
	for _, perm := range []os.FileMode{0o666, 0o620, 0o602} {
		p := writeSecret(t, testSecretValue, perm)
		_, err := resolveOne(t, "", p)
		if err == nil || !strings.Contains(err.Error(), "writable") || !strings.Contains(err.Error(), p) {
			t.Fatalf("mode %04o: want a writable refusal naming the path, got %v", perm, err)
		}
		assertNoValue(t, err)
	}
}

// The delivery shapes a supported mechanism produces must boot; the hand-made
// host file wardynd's own uid owns and anyone can read must not.
func TestSecretFile_ModeRuleByOwner(t *testing.T) {
	const euid = 65532
	for _, tc := range []struct {
		name   string
		perm   os.FileMode
		owner  int
		refuse bool
	}{
		{"kubelet Secret volume, root 0440 under fsGroup", 0o440, 0, false},
		{"Secrets Store CSI, root 0644", 0o644, 0, false},
		{"Vault Agent default, uid 100 0644", 0o644, 100, false},
		{"own file 0600", 0o600, euid, false},
		{"own file 0640", 0o640, euid, false},
		{"own file 0644", 0o644, euid, true},
		{"root-owned group-writable 0460", 0o460, 0, true},
	} {
		err := checkSecretFileMode("WARDYN_TEST_STR_FILE", "/p", tc.perm, tc.owner, euid)
		if (err != nil) != tc.refuse {
			t.Errorf("%s: err = %v, want refuse=%v", tc.name, err, tc.refuse)
		}
	}
	// Running as root, nothing is "wardynd's own" — root can read anything.
	if err := checkSecretFileMode("WARDYN_TEST_STR_FILE", "/p", 0o644, 0, 0); err != nil {
		t.Errorf("euid 0, root-owned 0644: %v", err)
	}
	err := checkSecretFileMode("WARDYN_TEST_STR_FILE", "/p", 0o644, euid, euid)
	if err == nil || !strings.Contains(err.Error(), "chmod 640") || !strings.Contains(err.Error(), "agent-inject-perms") {
		t.Fatalf("own 0644 refusal must name chmod 640 and agent-inject-perms, got %v", err)
	}
}

// End to end on a real file this test's own uid owns: 0644 refuses, 0640 boots.
func TestSecretFile_OwnOtherReadableRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root the own-file rule does not apply")
	}
	if _, err := resolveOne(t, "", writeSecret(t, testSecretValue, 0o644)); err == nil || !strings.Contains(err.Error(), "chmod 640") {
		t.Fatalf("own 0644: want a refusal, got %v", err)
	}
	if v, err := resolveOne(t, "", writeSecret(t, testSecretValue, 0o640)); err != nil || v != testSecretValue {
		t.Fatalf("own 0640: got (%q, %v), want the value", v, err)
	}
}

// Every _FILE twin must be its setting's name plus "_FILE" (the documented
// convention the chart and docs rely on), and every setting must be distinct.
func TestSecretFileSettings_NamingConvention(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range secretFileSettings(&bootFlags{}) {
		if s.fileVar != s.name+"_FILE" {
			t.Errorf("%s: twin %q is not %q", s.name, s.fileVar, s.name+"_FILE")
		}
		if seen[s.name] {
			t.Errorf("%s listed twice", s.name)
		}
		seen[s.name] = true
	}
}

// The table is wired to the real flag fields: a _FILE env var fills the field
// the rest of boot reads.
func TestSecretFileSettings_FillsBootFlag(t *testing.T) {
	f := &bootFlags{
		dsn: new(string), migrateDSN: new(string), adminToken: new(string), ageKey: new(string),
		oidcClientSecret: new(string), dirSecret: new(string), auditSinks: new(string),
	}
	t.Setenv("WARDYN_AGE_KEY_FILE", writeSecret(t, "AGE-SECRET-KEY-1TEST\n", 0o400))
	if err := resolveSecretFiles(secretFileSettings(f)); err != nil {
		t.Fatal(err)
	}
	if *f.ageKey != "AGE-SECRET-KEY-1TEST" {
		t.Fatalf("ageKey = %q, want the file's value", *f.ageKey)
	}
}
