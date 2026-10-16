// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// testCAPEM is a self-signed CA certificate named cn, as PEM.
func testCAPEM(t *testing.T, cn string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func writeFile(t *testing.T, path, text string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// The first source set wins and only it is read: the flag (defaulting to WARDYN_TRUSTED_CA_FILE),
// else SSL_CERT_FILE, else SSL_CERT_DIR, whose files are read in lexical order.
func TestLoadTrustedCAPrecedence(t *testing.T) {
	dir := t.TempDir()
	a, b, c := testCAPEM(t, "flag"), testCAPEM(t, "ssl-file"), testCAPEM(t, "dir")
	flagFile := writeFile(t, filepath.Join(dir, "flag.pem"), a)
	sslFile := writeFile(t, filepath.Join(dir, "ssl.pem"), b)
	certDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(filepath.Join(certDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(certDir, "2.pem"), b)
	writeFile(t, filepath.Join(certDir, "1.pem"), c)
	both := map[string]string{"SSL_CERT_FILE": sslFile, "SSL_CERT_DIR": certDir}
	for _, tc := range []struct {
		name, file string
		env        map[string]string
		want       string
		n          int
	}{
		{"flag over the rest", flagFile, both, a, 1},
		{"SSL_CERT_FILE over SSL_CERT_DIR", "", both, b, 1},
		{"SSL_CERT_DIR in lexical order", "", map[string]string{"SSL_CERT_DIR": certDir}, c + b, 2},
		{"none", "", nil, "", 0},
	} {
		got, _, n, err := loadTrustedCA(tc.file, envOf(tc.env))
		if err != nil || got != tc.want || n != tc.n {
			t.Errorf("%s: got %d certs, err %v, bundle match %v", tc.name, n, err, got == tc.want)
		}
	}
}

// An unreadable, empty or non-certificate source fails, naming it; the bundle is rebuilt from the
// parsed certificates only; a whole system bundle is refused, since no sandbox could start with it.
func TestLoadTrustedCARefuses(t *testing.T) {
	dir := t.TempDir()
	ca := testCAPEM(t, "corp")
	keyPEM := "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"
	badDir := filepath.Join(dir, "bad")
	if err := os.Mkdir(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(badDir, "a.pem"), ca)
	bad := writeFile(t, filepath.Join(badDir, "b.pem"), "not a certificate")
	big := strings.Repeat(ca, maxTrustedCABundle/len(ca)+1) // over the 4 MiB sanity bound
	for _, tc := range []struct {
		name, file string
		env        map[string]string
		want       string
	}{
		{"missing", filepath.Join(dir, "missing.pem"), nil, "missing.pem"},
		{"empty", writeFile(t, filepath.Join(dir, "empty.pem"), ""), nil, "no PEM-encoded certificates"},
		{"a key beside the cert", writeFile(t, filepath.Join(dir, "combined.pem"), ca+keyPEM), nil, `"PRIVATE KEY"`},
		{"SSL_CERT_FILE unreadable", "", map[string]string{"SSL_CERT_FILE": filepath.Join(dir, "nope.pem")}, "SSL_CERT_FILE"},
		{"an invalid file in SSL_CERT_DIR", "", map[string]string{"SSL_CERT_DIR": badDir}, bad},
		{"an empty SSL_CERT_DIR", "", map[string]string{"SSL_CERT_DIR": t.TempDir()}, "no certificate files"},
		{"a whole system bundle", writeFile(t, filepath.Join(dir, "big.pem"), big), nil, "limit"},
	} {
		if _, _, _, err := loadTrustedCA(tc.file, envOf(tc.env)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.want)
		}
	}
	got, _, _, err := loadTrustedCA(writeFile(t, filepath.Join(dir, "noisy.pem"), "# corp root\n"+ca+"trailing text\n"), envOf(nil))
	if err != nil || got != ca {
		t.Errorf("bundle = %q, %v; want only the rebuilt certificate", got, err)
	}
}

// serve resolves the bundle at startup, from WARDYN_TRUSTED_CA_FILE when the flag is not given, and
// a bad one ends serve before the runtime is built; a good one reaches the builder.
func TestServeTrustedCAAtStartup(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "runner")
	key, err := runneridentity.Generate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	org := "https://org.example"
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)), OrgURLSHA256: federation.OrgURLSHA256(org)}
	if err := runneridentity.Save(stateDir, identity); err != nil {
		t.Fatal(err)
	}
	ca := testCAPEM(t, "corp")
	good := writeFile(t, filepath.Join(t.TempDir(), "ca.pem"), ca)
	stop := errors.New("built")
	serve := func(env string, args ...string) (serveOptions, bool, error) {
		t.Setenv("WARDYN_TRUSTED_CA_FILE", env)
		t.Setenv("SSL_CERT_FILE", "")
		t.Setenv("SSL_CERT_DIR", "")
		var got serveOptions
		built := false
		cmd := newServeCommand(func(_ context.Context, _ runneridentity.Identity, _ string, opts serveOptions) (runnerRuntime, error) {
			got, built = opts, true
			return runnerRuntime{}, stop
		})
		cmd.SetArgs(append([]string{"--org", org, "--state-dir", stateDir}, args...))
		return got, built, cmd.Execute()
	}
	if got, built, err := serve(good); !errors.Is(err, stop) || !built || got.trustedCA != ca || got.trustedCAPath != filepath.Join(stateDir, "trusted-ca.pem") {
		t.Fatalf("serve with WARDYN_TRUSTED_CA_FILE: err=%v built=%v; want the bundle at the builder", err, built)
	}
	file := filepath.Join(stateDir, "trusted-ca.pem")
	if st, err := os.Stat(file); err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("the bundle file for sandboxes: %v, %v; want it written 0644", st, err)
	}
	if b, _ := os.ReadFile(file); string(b) != ca {
		t.Fatalf("the bundle file holds %q, want the validated bundle", b)
	}
	if got, _, err := serve(""); !errors.Is(err, stop) || got.trustedCAPath != "" {
		t.Fatalf("serve with no CA: err=%v path=%q", err, got.trustedCAPath)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("a stale bundle file survived a start with no CA: %v", err)
	}
	if _, built, err := serve(filepath.Join(t.TempDir(), "gone.pem")); err == nil || errors.Is(err, stop) || built {
		t.Fatalf("serve with a missing WARDYN_TRUSTED_CA_FILE: err=%v built=%v; want a startup refusal", err, built)
	}
	if got, _, err := serve(filepath.Join(t.TempDir(), "gone.pem"), "--trusted-ca-file", good); !errors.Is(err, stop) || got.trustedCA != ca {
		t.Fatalf("--trusted-ca-file over a bad WARDYN_TRUSTED_CA_FILE: err=%v", err)
	}
}

// setup --trusted-ca-file checks the file before the token is spent, writes its absolute path into
// the env file or LaunchAgent, and a rerun without the flag keeps it.
func TestSetupWritesAndKeepsTheTrustedCA(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		f := newSetupFixture(t, goos)
		ca := writeFile(t, filepath.Join(t.TempDir(), "corp-ca.pem"), testCAPEM(t, "corp"))
		p := mustPaths(t, f)
		file, want := p.envFile, "WARDYN_TRUSTED_CA_FILE="+ca+"\n"
		if goos == "darwin" {
			file, want = p.plistFile, "<key>WARDYN_TRUSTED_CA_FILE</key>\n    <string>"+ca+"</string>"
		}
		if _, err := f.run(setupOptions{tokenFile: f.tokenFile, trustedCAFile: ca}); err != nil {
			t.Fatal(err)
		}
		if got := f.read(file); !strings.Contains(got, want) {
			t.Errorf("%s: service definition lacks %q:\n%s", goos, want, got)
		}
		if _, err := f.run(setupOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := f.read(file); !strings.Contains(got, want) {
			t.Errorf("%s: a rerun without --trusted-ca-file dropped it:\n%s", goos, got)
		}

		g := newSetupFixture(t, goos)
		g.flagless = true
		if _, err := g.run(setupOptions{tokenFile: g.tokenFile}); err != nil {
			t.Fatal(err)
		}
		if got := g.read(map[string]string{"linux": mustPaths(t, g).envFile, "darwin": mustPaths(t, g).plistFile}[goos]); strings.Contains(got, "WARDYN_TRUSTED_CA_FILE") || strings.Contains(got, "__WARDYN") {
			t.Errorf("%s: no CA asked for, yet the service definition names one:\n%s", goos, got)
		}
	}
	f := newSetupFixture(t, "linux")
	bad := writeFile(t, filepath.Join(t.TempDir(), "combined.pem"), "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile, trustedCAFile: bad}); err == nil || len(f.calls) != 0 {
		t.Fatalf("a key file as the CA: err=%v calls=%v; want a refusal before the token", err, f.calls)
	}
}

// DR-0202: setup pins a proxy image only when someone named it. The derived release image is left
// out so serve follows the binary; a named image is written even when it equals the release image;
// a rerun keeps a named custom image but drops an older setup's derived release pin.
func TestSetupPinsOnlyANamedProxyImage(t *testing.T) {
	const release = "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.3"
	setupBuild(t, "0.9.3")
	f := newSetupFixture(t, "linux")
	env := mustPaths(t, f).envFile
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile, proxyImage: release}); err != nil {
		t.Fatal(err)
	}
	if got := f.read(env); !strings.Contains(got, "WARDYN_PROXY_IMAGE="+release+"\n") {
		t.Errorf("a named image equal to the release image was not pinned:\n%s", got)
	}
	const custom = "mirror.example.com/wardyn-proxy:0.9.1"
	if _, err := f.run(setupOptions{proxyImage: custom}); err != nil {
		t.Fatal(err)
	}
	f.flagless = true
	if _, err := f.run(setupOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.read(env); !strings.Contains(got, "WARDYN_PROXY_IMAGE="+custom+"\n") {
		t.Errorf("a rerun dropped the named image:\n%s", got)
	}
	// A setup before 0.9.3 wrote its derived release image with no marker.
	legacy := strings.Replace(strings.Replace(f.read(env), custom, "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.2", 1), "WARDYN_PROXY_IMAGE_PINNED=1\n", "", 1)
	writeFile(t, env, legacy)
	if _, err := f.run(setupOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.read(env); strings.Contains(got, "WARDYN_PROXY_IMAGE") {
		t.Errorf("a rerun kept an older setup's derived release pin:\n%s", got)
	}
}

// DR-0202: a release image named with --proxy-image carries the pinned marker, so a flagless rerun
// keeps it (on both systems) instead of mistaking it for an older setup's derived default.
func TestSetupKeepsANamedReleaseImage(t *testing.T) {
	const release = "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.3"
	setupBuild(t, "0.9.3")
	for _, goos := range []string{"linux", "darwin"} {
		f := newSetupFixture(t, goos)
		p := mustPaths(t, f)
		file, wants := p.envFile, []string{"WARDYN_PROXY_IMAGE=" + release + "\n", "WARDYN_PROXY_IMAGE_PINNED=1\n"}
		if goos == "darwin" {
			file, wants = p.plistFile, []string{"<key>WARDYN_PROXY_IMAGE</key>\n    <string>" + release + "</string>", "<key>WARDYN_PROXY_IMAGE_PINNED</key>\n    <string>1</string>"}
		}
		if _, err := f.run(setupOptions{tokenFile: f.tokenFile, proxyImage: release}); err != nil {
			t.Fatal(err)
		}
		f.flagless = true
		if _, err := f.run(setupOptions{}); err != nil {
			t.Fatal(err)
		}
		got := f.read(file)
		for _, want := range wants {
			if !strings.Contains(got, want) {
				t.Errorf("%s: a flagless rerun dropped the named release image (%q):\n%s", goos, want, got)
			}
		}

		g := newSetupFixture(t, goos)
		g.flagless = true
		if _, err := g.run(setupOptions{tokenFile: g.tokenFile}); err != nil {
			t.Fatal(err)
		}
		if got := g.read(map[string]string{"linux": mustPaths(t, g).envFile, "darwin": mustPaths(t, g).plistFile}[goos]); strings.Contains(got, "PINNED") {
			t.Errorf("%s: a derived image was marked pinned:\n%s", goos, got)
		}
	}
}

// A kept CA file that no longer checks out names where it was kept and how to replace or drop it.
func TestSetupNamesAKeptTrustedCA(t *testing.T) {
	f := newSetupFixture(t, "linux")
	ca := writeFile(t, filepath.Join(t.TempDir(), "corp-ca.pem"), testCAPEM(t, "corp"))
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile, trustedCAFile: ca}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ca); err != nil {
		t.Fatal(err)
	}
	_, err := f.run(setupOptions{})
	if err == nil || !strings.Contains(err.Error(), "kept from the earlier setup ("+mustPaths(t, f).envFile+")") || !strings.Contains(err.Error(), "remove WARDYN_TRUSTED_CA_FILE") {
		t.Fatalf("a rerun with a moved kept CA: %v", err)
	}
}
