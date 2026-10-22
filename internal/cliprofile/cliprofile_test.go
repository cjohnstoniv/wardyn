// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package cliprofile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProfileWriteReadRoundTripIsWorldReadableAndHoldsOnlyTheURL(t *testing.T) {
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

func TestProfileReadNamesTheFileItCouldNotParse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("Read of a malformed profile succeeded")
	}
}

func TestProfileNormalize(t *testing.T) {
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

// Writing one field keeps the other, in either order, and a broken profile is replaced rather than blocking
// the write that would fix it.
func TestProfileWriteKeepsTheOtherField(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wardyn")
	if err := Write(dir, "https://wardyn.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := WriteArtifactBase(dir, " https://downloads.example.com/wardyn/ "); err != nil {
		t.Fatal(err)
	}
	if got, _ := Read(dir); got != "https://wardyn.example.com" {
		t.Errorf("url after writing the artifact base = %q, want it kept", got)
	}
	if got, _ := ReadArtifactBase(dir); got != "https://downloads.example.com/wardyn" {
		t.Errorf("artifact base = %q, want the normalised one", got)
	}
	if err := Write(dir, "https://org.example.com"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadArtifactBase(dir); got != "https://downloads.example.com/wardyn" {
		t.Errorf("artifact base after writing the url = %q, want it kept", got)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Write(dir, "https://org.example.com"); err != nil {
		t.Fatalf("Write over a broken profile: %v", err)
	}
	if got, err := Read(dir); err != nil || got != "https://org.example.com" {
		t.Errorf("url after replacing a broken profile = %q, %v", got, err)
	}
}

func TestProfileArtifactBaseIsHTTPSOnly(t *testing.T) {
	dir := t.TempDir()
	for _, in := range []string{"http://downloads.example.com", "http://localhost:9000", "downloads.example.com", "https://u:p@downloads.example.com"} {
		if err := WriteArtifactBase(dir, in); err == nil {
			t.Errorf("WriteArtifactBase(%q) succeeded; want a refusal", in)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err == nil {
		t.Error("a refused artifact base still wrote the profile")
	}
	if got, err := NormalizeArtifactBase("https://downloads.example.com/w/?x=1#f"); err != nil || got != "https://downloads.example.com/w" {
		t.Errorf("NormalizeArtifactBase = %q, %v", got, err)
	}
}

// TestNormalizeIsTheProofOrigin pins Normalize to the form a device proof signs over (types.CLIOrigin):
// lower-case scheme and host, the default port dropped, no trailing slash.
func TestNormalizeIsTheProofOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"HTTPS://Wardyn.Example.COM:443/w/": "https://wardyn.example.com/w",
		"http://LocalHost:80":               "http://localhost",
		"https://wardyn.example.com:8443/":  "https://wardyn.example.com:8443",
		"http://127.0.0.1:8080":             "http://127.0.0.1:8080",
		"http://[::1]:80/":                  "http://[::1]",
		"https://[FE80::1]:9443":            "https://[fe80::1]:9443",
	} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
