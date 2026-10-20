// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package certbundle

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// anchorFileCase is one entry of testdata/anchor-file-cases.json, the table
// the runner's org CA check carries a byte-identical copy of.
type anchorFileCase struct {
	Name           string   `json:"name"`
	FileMode       string   `json:"file_mode"`
	FileOwner      string   `json:"file_owner"`
	DirMode        string   `json:"dir_mode"`
	DirSticky      bool     `json:"dir_sticky"`
	DirOwner       string   `json:"dir_owner"`
	ViaLinkDirMode string   `json:"via_link_dir_mode"`
	Certs          []string `json:"certs"`
	Expect         string   `json:"expect"`
	Warnings       int      `json:"warnings"`
}

func octal(t *testing.T, s string) os.FileMode {
	t.Helper()
	n, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		t.Fatalf("mode %q: %v", s, err)
	}
	return os.FileMode(n & 0o777)
}

// anchorFixture is a store directory "store" holding "anchors.pem", with the
// owners the case names injected through anchorOwner: a test cannot create a
// root-owned file without root, so "root" and "other" are stood in; "self"
// is the real owner.
type anchorFixture struct {
	base, dir, path string
}

func newAnchorFixture(t *testing.T, pemBytes []byte, fileMode, dirMode os.FileMode, fileOwner, dirOwner string) anchorFixture {
	t.Helper()
	base := t.TempDir()
	f := anchorFixture{base: base, dir: filepath.Join(base, "store")}
	f.path = filepath.Join(f.dir, "anchors.pem")
	if err := os.Mkdir(f.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.path, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.dir, dirMode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.dir, 0o700) })
	uid := map[string]int{"self": os.Geteuid(), "root": 0, "other": os.Geteuid() + 4242}
	real := anchorOwner
	anchorOwner = func(fi os.FileInfo) (int, bool) {
		switch fi.Name() {
		case "anchors.pem":
			return uid[fileOwner], true
		case "store":
			return uid[dirOwner], true
		}
		return real(fi)
	}
	t.Cleanup(func() { anchorOwner = real })
	return f
}

// link is a symlink to f.path in a second directory of mode m.
func (f anchorFixture) link(t *testing.T, m os.FileMode) string {
	t.Helper()
	links := filepath.Join(f.base, "links")
	if err := os.Mkdir(links, 0o700); err != nil {
		t.Fatal(err)
	}
	l := filepath.Join(links, "link.pem")
	if err := os.Symlink(f.path, l); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(links, m); err != nil {
		t.Fatal(err)
	}
	return l
}

func testBundles(t *testing.T) map[string]*x509.Certificate {
	t.Helper()
	year := now.Add(365 * 24 * time.Hour)
	root, rootKey := mint(t, "Valid Root", true, year, nil, nil)
	other, _ := mint(t, "Second Root", true, year, nil, nil)
	expired, _ := mint(t, "Expired Root", true, now.Add(-time.Hour), nil, nil)
	future := mintNotYetValid(t)
	leaf, _ := mint(t, "Leaf", false, year, root, rootKey)
	return map[string]*x509.Certificate{"valid": root, "valid2": other, "expired": expired, "not_ca": leaf, "not_yet_valid": future}
}

// mintNotYetValid is a self-signed CA whose validity starts a day after now.
func mintNotYetValid(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "Future Root"},
		NotBefore: now.Add(24 * time.Hour), NotAfter: now.Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestLoadFile_AnchorFileCases runs the shared table unchanged.
func TestLoadFile_AnchorFileCases(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "anchor-file-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []anchorFileCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 10 {
		t.Fatalf("%d cases; the table is vacuous", len(cases))
	}
	certs := testBundles(t)
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			var bundle []*x509.Certificate
			valids := 0
			for _, n := range tc.Certs {
				if n == "valid" {
					// A second "valid" is a second root, so the bundle holds two CAs.
					if valids++; valids == 2 {
						n = "valid2"
					}
				}
				c, ok := certs[n]
				if !ok {
					t.Fatalf("unknown cert kind %q", n)
				}
				bundle = append(bundle, c)
			}
			dirMode := octal(t, tc.DirMode)
			if tc.DirSticky {
				dirMode |= os.ModeSticky
			}
			f := newAnchorFixture(t, Encode(bundle), octal(t, tc.FileMode), dirMode, tc.FileOwner, tc.DirOwner)
			use := f.path
			if tc.ViaLinkDirMode != "" {
				use = f.link(t, octal(t, tc.ViaLinkDirMode))
			}
			got, warnings, err := LoadFile(use, now)
			switch tc.Expect {
			case "load":
				if err != nil {
					t.Fatalf("LoadFile: %v, want load", err)
				}
				if want := len(tc.Certs) - tc.Warnings; len(got) != want {
					t.Errorf("%d certs, want %d", len(got), want)
				}
			case "refuse":
				if err == nil {
					t.Fatalf("LoadFile loaded %d certs, want a refusal", len(got))
				}
			default:
				t.Fatalf("unknown expect %q", tc.Expect)
			}
			if len(warnings) != tc.Warnings {
				t.Errorf("warnings %q, want %d", warnings, tc.Warnings)
			}
			for _, w := range warnings {
				if !strings.Contains(w, "sha256:") {
					t.Errorf("warning %q names no fingerprint", w)
				}
			}
		})
	}
}

// Rules the shared table does not carry: the resolved target's directory is
// checked too, and only a regular file loads.
func TestLoadFile_ResolvedDirAndRegularFile(t *testing.T) {
	pemBytes := Encode([]*x509.Certificate{testBundles(t)["valid"]})
	f := newAnchorFixture(t, pemBytes, 0o600, 0o770, "self", "self")
	if _, _, err := LoadFile(f.link(t, 0o700), now); err == nil || !strings.Contains(err.Error(), "its directory") {
		t.Errorf("link from a private directory to a target in a group-writable one: %v, want a refusal", err)
	}
	ok := newAnchorFixture(t, pemBytes, 0o600, 0o700, "self", "self")
	if _, _, err := LoadFile(ok.link(t, 0o700), now); err != nil {
		t.Errorf("twin, both directories private: %v", err)
	}
	d := newAnchorFixture(t, pemBytes, 0o600, 0o700, "self", "self")
	if err := os.Remove(d.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(d.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadFile(d.path, now); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a directory at the path: %v, want a refusal", err)
	}
}
