// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package cliprofile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadRoundTripIsWorldReadableAndHoldsOnlyTheURL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wardyn")
	if got, err := Read(dir); err != nil || got != "" {
		t.Fatalf("Read with no profile = %q, %v; want empty and no error", got, err)
	}
	if err := Write(dir, "https://wardyn.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, "https://org.example/wardyn"); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(dir); err != nil || got != "https://org.example/wardyn" {
		t.Fatalf("Read = %q, %v; want the second URL", got, err)
	}
	st, err := os.Stat(filepath.Join(dir, FileName))
	if err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("profile mode = %v, %v; want 0644", st.Mode(), err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, FileName)); string(b) != "{\"url\":\"https://org.example/wardyn\"}\n" {
		t.Errorf("profile = %q; it holds the URL and nothing else", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".cli.json.*")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestReadNamesTheFileItCouldNotParse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("Read of a malformed profile succeeded")
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"https://wardyn.example.com":          "https://wardyn.example.com",
		" https://wardyn.example.com/ ":       "https://wardyn.example.com",
		"https://wardyn.example.com/w/?x=1#f": "https://wardyn.example.com/w",
		"http://localhost:8080/":              "http://localhost:8080",
	} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "wardyn.example.com", "ftp://wardyn.example.com", "https://", "https://u:p@wardyn.example.com"} {
		if got, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) = %q; want a refusal", in, got)
		}
	}
}
