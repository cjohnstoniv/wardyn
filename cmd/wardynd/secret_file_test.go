// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"log/slog"
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
		// An INTERNAL blank line is part of the value (a PEM or JSON body
		// keeps its shape) — only the TRAILING run of line endings is
		// checked, and here there is exactly one.
		{"line1\n\nline3\n", "line1\n\nline3"},
		{" " + testSecretValue + " \n", " " + testSecretValue + " "},
	} {
		v, err := resolveOne(t, "", writeSecret(t, tc.content, 0o440))
		if err != nil || v != tc.want {
			t.Fatalf("content %q: got (%q, %v), want %q", tc.content, v, err, tc.want)
		}
	}
}

// A SECOND trailing line ending refuses naming the var and the path, never
// the content: a value that still ends in "\n" after one trim is a hidden
// extra byte a writer appended by mistake (T-60, #720), not part of the
// secret's own shape.
func TestSecretFile_TwoTrailingNewlinesRefuses(t *testing.T) {
	for _, content := range []string{
		testSecretValue + "\n\n",
		testSecretValue + "\r\n\r\n",
		testSecretValue + "\n\n\n",
		"line1\n\nline3\n\n",
		// A stray bare "\r" left over after the ONE full "\r\n"/"\n" trim
		// (F7, PR #1245 review): the first trim removes the trailing "\n",
		// the second removes only the LAST "\r", leaving one behind.
		testSecretValue + "\r\r\n",
	} {
		p := writeSecret(t, content, 0o440)
		_, err := resolveOne(t, "", p)
		if err == nil || !strings.Contains(err.Error(), "WARDYN_TEST_STR_FILE") || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "trailing line ending") {
			t.Fatalf("content %q: want a refusal naming the var, the path and \"trailing line ending\", got %v", content, err)
		}
		assertNoValue(t, err)
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

// Not just the returned error (assertNoValue, above) — nothing resolveSecretFiles
// or readSecretFile does anywhere along the way ever hands the secret value to
// slog, success or refusal (PR #1245 review F4, #720's own "slog captured,
// value never logged"). Captures the process's actual default logger, the
// same one every real slog.Info/Warn call in this package writes through.
func TestSecretFile_ValueNeverLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// One success (the value resolves) and every refusal shape this file
	// covers.
	resolveOne(t, "", writeSecret(t, testSecretValue+"\n", 0o440))
	resolveOne(t, "", writeSecret(t, testSecretValue+"\n\n", 0o440)) // two trailing newlines
	resolveOne(t, testSecretValue, writeSecret(t, "other\n", 0o400)) // both set
	resolveOne(t, "", writeSecret(t, "", 0o400))                     // empty
	resolveOne(t, "", writeSecret(t, testSecretValue, 0o666))        // world-writable
	resolveOne(t, "", filepath.Join(t.TempDir(), "does-not-exist"))  // unreadable

	if strings.Contains(buf.String(), testSecretValue) {
		t.Fatalf("the secret value reached the log: %s", buf.String())
	}
}

// Every secretFileSettings entry's boot-flag field starts empty in the state
// a real, unconfigured boot reaches before resolveSecretFiles runs, and stays
// empty through a resolveSecretFiles call with no _FILE var set (PR #1245
// review F4, #720's own "every secretFileSettings default is empty").
func TestSecretFileSettings_EveryDefaultIsEmpty(t *testing.T) {
	f := &bootFlags{
		dsn: new(string), migrateDSN: new(string), adminToken: new(string), ageKey: new(string),
		oidcClientSecret: new(string), dirSecret: new(string), auditSinks: new(string), approvalNotify: new(string),
		orgEnrolToken: new(string), scimToken: new(string), scimTokenNext: new(string),
	}
	settings := secretFileSettings(f)
	for _, s := range settings {
		t.Setenv(s.fileVar, "") // isolate from whatever the real process env holds
		if *s.value != "" {
			t.Errorf("%s starts %q, want empty", s.name, *s.value)
		}
	}
	if err := resolveSecretFiles(settings); err != nil {
		t.Fatalf("resolveSecretFiles with nothing configured: %v", err)
	}
	for _, s := range settings {
		if *s.value != "" {
			t.Errorf("%s = %q after resolveSecretFiles with no _FILE set, want still empty", s.name, *s.value)
		}
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
		oidcClientSecret: new(string), dirSecret: new(string), auditSinks: new(string), approvalNotify: new(string),
		orgEnrolToken: new(string),
	}
	t.Setenv("WARDYN_AGE_KEY_FILE", writeSecret(t, "AGE-SECRET-KEY-1TEST\n", 0o400))
	if err := resolveSecretFiles(secretFileSettings(f)); err != nil {
		t.Fatal(err)
	}
	if *f.ageKey != "AGE-SECRET-KEY-1TEST" {
		t.Fatalf("ageKey = %q, want the file's value", *f.ageKey)
	}
}
