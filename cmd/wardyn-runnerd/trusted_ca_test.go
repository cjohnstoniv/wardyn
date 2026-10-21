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
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner"
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

// An unreadable, empty or non-certificate file fails, naming it; the bundle is rebuilt from the
// parsed certificates only; a bundle over 64 KiB is refused, since a sandbox with nothing staged
// could trust it only through one environment string.
func TestLoadTrustedCARefuses(t *testing.T) {
	dir := t.TempDir()
	ca := testCAPEM(t, "corp")
	keyPEM := "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"
	big := strings.Repeat(ca, runner.MaxEnvTrustedCAPEM/len(ca)+1)
	for _, tc := range []struct{ name, file, want string }{
		{"missing", filepath.Join(dir, "missing.pem"), "missing.pem"},
		{"empty", writeFile(t, filepath.Join(dir, "empty.pem"), ""), "no PEM-encoded certificates"},
		{"a key beside the cert", writeFile(t, filepath.Join(dir, "combined.pem"), ca+keyPEM), `"PRIVATE KEY"`},
		{"over 64 KiB", writeFile(t, filepath.Join(dir, "big.pem"), big), "KiB; runners take at most 64 KiB: name a file holding only your organisation's roots, not a merged system bundle"},
		// A-R4-L2: refused on size alone, before the file is parsed (junk would otherwise read "no PEM-encoded certificates").
		{"over the read bound", writeFile(t, filepath.Join(dir, "huge.pem"), strings.Repeat("x", maxCAFileRead+1)), "the file is over 1024 KiB; runners take at most 64 KiB of certificates"},
	} {
		if _, _, err := loadTrustedCA(tc.file); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.want)
		}
	}
	got, n, err := loadTrustedCA(writeFile(t, filepath.Join(dir, "noisy.pem"), "# corp root\n"+ca+"trailing text\n"))
	if err != nil || got != ca || n != 1 {
		t.Errorf("bundle = %q (%d), %v; want only the rebuilt certificate", got, n, err)
	}
	if got, n, err := loadTrustedCA(" "); got != "" || n != 0 || err != nil {
		t.Errorf("no file: %q, %d, %v; want nothing", got, n, err)
	}
}

// A bundle of exactly 64 KiB is the largest accepted: the refusal is "over", not "at", and one byte
// over is refused.
func TestLoadTrustedCAAcceptsExactly64KiB(t *testing.T) {
	// Certificates of many lengths, then x copies of one and y of another that add up to size.
	byLen := map[int]string{}
	for n := 1; n <= 300; n++ {
		c := testCAPEM(t, strings.Repeat("c", n))
		byLen[len(c)] = c
	}
	bundleOf := func(size int) string {
		for la, a := range byLen {
			for lb, b := range byLen {
				for y := 1; y <= 3; y++ {
					if rem := size - y*lb; rem > 0 && rem%la == 0 {
						return strings.Repeat(a, rem/la) + strings.Repeat(b, y)
					}
				}
			}
		}
		t.Fatalf("could not build a bundle of exactly %d bytes", size)
		return ""
	}
	bundle := bundleOf(runner.MaxEnvTrustedCAPEM)
	if got, _, err := loadTrustedCA(writeFile(t, filepath.Join(t.TempDir(), "exact.pem"), bundle)); err != nil || got != bundle {
		t.Errorf("a %d-byte bundle: err=%v, kept=%v; want it accepted whole", len(bundle), err, got == bundle)
	}
	over := bundleOf(runner.MaxEnvTrustedCAPEM + 1)
	if _, _, err := loadTrustedCA(writeFile(t, filepath.Join(t.TempDir(), "over.pem"), over)); err == nil ||
		!strings.Contains(err.Error(), "runners take at most 64 KiB") {
		t.Errorf("a %d-byte bundle: err=%v; want it refused as over 64 KiB", len(over), err)
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
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("SSL_CERT_DIR", "")
	serve := func(env string, args ...string) (serveOptions, bool, error) {
		t.Setenv("WARDYN_TRUSTED_CA_FILE", env)
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
	if got, built, err := serve(""); !errors.Is(err, stop) || !built || got.trustedCA != "" || got.trustedCAPath != "" {
		t.Fatalf("serve with no CA configured: err=%v built=%v bundle=%d bytes path=%q; want nothing trusted and no refusal", err, built, len(got.trustedCA), got.trustedCAPath)
	}
	// A stopped sandbox created with the bundle still mounts the file when revived: it stays, emptied.
	if b, err := os.ReadFile(file); err != nil || len(b) != 0 {
		t.Fatalf("the bundle file after a start with no CA: %d bytes, %v; want it kept and empty", len(b), err)
	}
	if _, built, err := serve(filepath.Join(t.TempDir(), "gone.pem")); err == nil || errors.Is(err, stop) || built {
		t.Fatalf("serve with a missing WARDYN_TRUSTED_CA_FILE: err=%v built=%v; want a startup refusal", err, built)
	}
	big := writeFile(t, filepath.Join(t.TempDir(), "merged.pem"), strings.Repeat(ca, runner.MaxEnvTrustedCAPEM/len(ca)+1))
	if _, built, err := serve(big); err == nil || built || !strings.Contains(err.Error(), "runners take at most 64 KiB") {
		t.Fatalf("serve with a bundle over 64 KiB: err=%v built=%v; want a startup refusal", err, built)
	}
	if got, _, err := serve(filepath.Join(t.TempDir(), "gone.pem"), "--trusted-ca-file", good); !errors.Is(err, stop) || got.trustedCA != ca {
		t.Fatalf("--trusted-ca-file over a bad WARDYN_TRUSTED_CA_FILE: err=%v", err)
	}
}

// setup --trusted-ca-file checks the file before the token is spent, writes its absolute path into
// runnerd.json, and a rerun without the flag keeps it.
func TestSetupWritesAndKeepsTheTrustedCA(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		f := newSetupFixture(t, goos)
		ca := writeFile(t, filepath.Join(t.TempDir(), "corp-ca.pem"), testCAPEM(t, "corp"))
		p := mustPaths(t, f)
		want := "WARDYN_TRUSTED_CA_FILE=" + ca + "\n"
		if _, err := f.run(setupOptions{tokenFile: f.tokenFile, trustedCAFile: ca}); err != nil {
			t.Fatal(err)
		}
		if got := f.settings(p); !strings.Contains(got, want) {
			t.Errorf("%s: runnerd.json lacks %q:\n%s", goos, want, got)
		}
		if _, err := f.run(setupOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := f.settings(p); !strings.Contains(got, want) {
			t.Errorf("%s: a rerun without --trusted-ca-file dropped it:\n%s", goos, got)
		}

		g := newSetupFixture(t, goos)
		g.flagless = true
		if _, err := g.run(setupOptions{tokenFile: g.tokenFile}); err != nil {
			t.Fatal(err)
		}
		if got := g.settings(mustPaths(t, g)); strings.Contains(got, "WARDYN_TRUSTED_CA_FILE") {
			t.Errorf("%s: no CA asked for, yet runnerd.json names one:\n%s", goos, got)
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
	p := mustPaths(t, f)
	if _, err := f.run(setupOptions{tokenFile: f.tokenFile, proxyImage: release}); err != nil {
		t.Fatal(err)
	}
	if got := f.settings(p); !strings.Contains(got, "WARDYN_PROXY_IMAGE="+release+"\n") {
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
	if got := f.settings(p); !strings.Contains(got, "WARDYN_PROXY_IMAGE="+custom+"\n") {
		t.Errorf("a rerun dropped the named image:\n%s", got)
	}
	// A setup before 0.9.3 wrote its derived release image into the service's environment file, with no marker.
	if err := os.Remove(p.configFile()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, p.envFile, "WARDYN_PROXY_IMAGE=ghcr.io/cjohnstoniv/wardyn-proxy:0.9.2\n")
	if _, err := f.run(setupOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.settings(p); strings.Contains(got, "WARDYN_PROXY_IMAGE") {
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
		wants := []string{"WARDYN_PROXY_IMAGE=" + release + "\n", "WARDYN_PROXY_IMAGE_PINNED=1\n"}
		if _, err := f.run(setupOptions{tokenFile: f.tokenFile, proxyImage: release}); err != nil {
			t.Fatal(err)
		}
		f.flagless = true
		if _, err := f.run(setupOptions{}); err != nil {
			t.Fatal(err)
		}
		got := f.settings(p)
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
		if got := g.settings(mustPaths(t, g)); strings.Contains(got, "PINNED") {
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
	if err == nil || !strings.Contains(err.Error(), "kept from the earlier setup ("+mustPaths(t, f).configFile()+")") || !strings.Contains(err.Error(), "remove trusted_ca_file") {
		t.Fatalf("a rerun with a moved kept CA: %v", err)
	}
}

// DR-0233: setup never adopts the ambient SSL_CERT_FILE; it names it once, with the flag that would.
func TestSetupOnlyHintsAtSSLCertFile(t *testing.T) {
	ssl := writeFile(t, filepath.Join(t.TempDir(), "ambient.pem"), testCAPEM(t, "ambient"))
	t.Setenv("SSL_CERT_FILE", ssl)
	t.Setenv("SSL_CERT_DIR", t.TempDir()+":"+t.TempDir())
	f := newSetupFixture(t, "linux")
	out, err := f.run(setupOptions{tokenFile: f.tokenFile})
	if err != nil {
		t.Fatal(err)
	}
	hint := "SSL_CERT_FILE is set (" + ssl + "); pass --trusted-ca-file " + ssl + " if runs must trust it (64 KiB at most), or --org-ca-file " + ssl + " to trust it for this runner's connection to the organisation only (the CLI's own anchors: wardyn ca add)\n"
	if strings.Count(out, hint) != 1 {
		t.Errorf("setup output lacks the one hint %q:\n%s", hint, out)
	}
	if got := f.settings(mustPaths(t, f)); strings.Contains(got, "WARDYN_TRUSTED_CA_FILE") {
		t.Errorf("setup adopted the ambient SSL_CERT_FILE:\n%s", got)
	}
	g := newSetupFixture(t, "linux")
	ca := writeFile(t, filepath.Join(t.TempDir(), "corp-ca.pem"), testCAPEM(t, "corp"))
	if out, err := g.run(setupOptions{tokenFile: g.tokenFile, trustedCAFile: ca}); err != nil || strings.Contains(out, "SSL_CERT_FILE is set") {
		t.Errorf("with --trusted-ca-file: err=%v; the hint should not print:\n%s", err, out)
	}
	// A rerun that keeps the earlier CA has nothing to be told.
	if out, err := g.run(setupOptions{}); err != nil || strings.Contains(out, "SSL_CERT_FILE is set") {
		t.Errorf("a rerun keeping a CA: err=%v; the hint should not print:\n%s", err, out)
	}
}

// DR-0233: serve never reads the ambient SSL_CERT_FILE or SSL_CERT_DIR. Unset, a valid file, a
// colon list or a directory mixing certificates with other files changes nothing and never stops
// startup; the named file stays strict.
func TestServeIgnoresAmbientSSLCert(t *testing.T) {
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
	bad := writeFile(t, filepath.Join(t.TempDir(), "key.pem"), "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n")
	mixed := t.TempDir()
	writeFile(t, filepath.Join(mixed, "5ed36f99.0"), testCAPEM(t, "ambient"))
	writeFile(t, filepath.Join(mixed, "README"), "not a certificate")
	if err := os.Mkdir(filepath.Join(mixed, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	ambientFile := writeFile(t, filepath.Join(t.TempDir(), "ambient.pem"), testCAPEM(t, "ambient"))
	stop := errors.New("built")
	serve := func(named string) (serveOptions, bool, error) {
		t.Setenv("WARDYN_TRUSTED_CA_FILE", named)
		var got serveOptions
		built := false
		cmd := newServeCommand(func(_ context.Context, _ runneridentity.Identity, _ string, opts serveOptions) (runnerRuntime, error) {
			got, built = opts, true
			return runnerRuntime{}, stop
		})
		cmd.SetArgs([]string{"--org", org, "--state-dir", stateDir})
		return got, built, cmd.Execute()
	}
	for _, tc := range []struct{ name, file, dir string }{
		{"unset", "", ""},
		{"a valid file", ambientFile, ""},
		{"a missing file", filepath.Join(t.TempDir(), "gone.pem"), ""},
		{"a colon list", "", mixed + ":" + filepath.Join(t.TempDir(), "gone")},
		{"a mixed directory", "", mixed},
	} {
		t.Setenv("SSL_CERT_FILE", tc.file)
		t.Setenv("SSL_CERT_DIR", tc.dir)
		if got, built, err := serve(""); !errors.Is(err, stop) || !built || got.trustedCA != "" {
			t.Errorf("%s, nothing named: err=%v built=%v bundle=%d bytes; want nothing trusted and no refusal", tc.name, err, built, len(got.trustedCA))
		}
		if got, _, err := serve(good); !errors.Is(err, stop) || got.trustedCA != ca {
			t.Errorf("%s, a named file: err=%v; want exactly the named bundle", tc.name, err)
		}
		if _, built, err := serve(bad); err == nil || built || !strings.Contains(err.Error(), bad) {
			t.Errorf("%s, an invalid named file: err=%v built=%v; want a startup refusal naming it", tc.name, err, built)
		}
	}
}

// testCertPEM is a self-signed certificate named cn, valid from notBefore to notAfter, as PEM.
func testCertPEM(t *testing.T, cn string, isCA bool, notBefore, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: cn}, NotBefore: notBefore, NotAfter: notAfter,
		IsCA: isCA, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// #3309: a bundle with an expired CA, a certificate that is not a CA and a valid CA warns exactly
// twice, naming each, and still loads as before.
func TestTrustedCAWarnsExpiredAndNotCA(t *testing.T) {
	now := time.Now()
	bundle := testCertPEM(t, "old-root", true, now.Add(-48*time.Hour), now.Add(-24*time.Hour)) +
		testCertPEM(t, "laptop-17", false, now.Add(-time.Hour), now.Add(time.Hour)) + testCAPEM(t, "corp")
	file := writeFile(t, filepath.Join(t.TempDir(), "ca.pem"), bundle)
	if _, n, err := loadTrustedCA(file); err != nil || n != 3 {
		t.Fatalf("loadTrustedCA: n=%d err=%v; want all three certificates loaded as today", n, err)
	}
	certs, warnings := trustedCAFileCheck(file, now)
	if len(certs) != 3 || len(warnings) != 2 {
		t.Fatalf("certs=%d warnings=%q; want 3 certificates and exactly two warnings", len(certs), warnings)
	}
	want := []string{
		"the trusted CA file " + file + ` holds a certificate for "old-root" that expired on ` + now.Add(-24*time.Hour).UTC().Format(time.DateOnly) + ". The runner still uses the file as it is.",
		"the trusted CA file " + file + ` holds a certificate for "laptop-17" that is not marked as a certificate authority, so it cannot vouch for a server. The runner still uses the file as it is.`,
	}
	for i := range want {
		if warnings[i] != want[i] {
			t.Errorf("warning %d = %q\nwant %q", i, warnings[i], want[i])
		}
	}
	notYet := writeFile(t, filepath.Join(t.TempDir(), "ca.pem"), testCertPEM(t, "new-root", true, now.Add(24*time.Hour), now.Add(48*time.Hour)))
	if _, w := trustedCAFileCheck(notYet, now); len(w) != 1 || !strings.Contains(w[0], `"new-root" that is not valid until`) {
		t.Errorf("a certificate not valid yet: %q", w)
	}
	if _, w := trustedCAFileCheck(writeFile(t, filepath.Join(t.TempDir(), "ca.pem"), testCAPEM(t, "corp")), now); len(w) != 0 {
		t.Errorf("a current CA in a private file warned: %q", w)
	}
}

// #3309: a file or a directory other accounts can write warns, by mode; a sticky directory does not.
func TestTrustedCAWarnsLooseFileAndDirectory(t *testing.T) {
	dir := t.TempDir()
	file := writeFile(t, filepath.Join(dir, "ca.pem"), testCAPEM(t, "corp"))
	if err := os.Chmod(file, 0o666); err != nil {
		t.Fatal(err)
	}
	_, w := trustedCAFileCheck(file, time.Now())
	if len(w) != 1 || w[0] != "the trusted CA file "+file+" can be changed by other accounts (mode 0666): run chmod go-w on it. The runner still uses the file as it is." {
		t.Fatalf("a 0666 file: %q", w)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, w = trustedCAFileCheck(file, time.Now())
	if len(w) != 1 || w[0] != "the directory "+dir+" holding the trusted CA file can be changed by other accounts (mode 0777): run chmod go-w on it. The runner still uses the file as it is." {
		t.Fatalf("a 0777 directory: %q", w)
	}
	if err := os.Chmod(dir, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if _, w = trustedCAFileCheck(file, time.Now()); len(w) != 0 {
		t.Fatalf("a sticky directory warned: %q", w)
	}
}

// #3309: a file owned by neither root nor the runner's own account warns, naming the owner.
func TestTrustedCAOwnerWarning(t *testing.T) {
	if w := caOwnerWarning("/etc/corp.pem", 1234, 1000); w != "the trusted CA file /etc/corp.pem belongs to another account (uid 1234), not to root or to the account this runner runs as." {
		t.Errorf("another account's file: %q", w)
	}
	for _, uid := range []int{0, 1000} {
		if w := caOwnerWarning("/etc/corp.pem", uid, 1000); w != "" {
			t.Errorf("uid %d warned: %q", uid, w)
		}
	}
}

// #3309: serve logs each certificate and each warning at startup, and still trusts the file.
func TestServeLogsTrustedCAWarningsAndStillLoads(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	now := time.Now()
	bundle := testCertPEM(t, "old-root", true, now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	opts := serveOptions{trustedCAFile: writeFile(t, filepath.Join(t.TempDir(), "ca.pem"), bundle)}
	if err := trustedCAAtStartup(context.Background(), &opts, t.TempDir()); err != nil || opts.trustedCA != bundle {
		t.Fatalf("trustedCAAtStartup: %v; want the expired bundle still trusted", err)
	}
	logs := buf.String()
	if !strings.Contains(logs, `level=INFO msg="corporate CA certificate" subject="CN=old-root" issuer="CN=old-root" not_after=`) {
		t.Errorf("no per-certificate line:\n%s", logs)
	}
	if !strings.Contains(logs, `level=WARN msg="the trusted CA file `) || !strings.Contains(logs, `that expired on`) {
		t.Errorf("no expiry warning:\n%s", logs)
	}
}

// #3309: setup prints the same warnings before it uses the registration token, and still writes the file.
func TestSetupPrintsTrustedCAWarnings(t *testing.T) {
	f := newSetupFixture(t, "linux")
	ca := writeFile(t, filepath.Join(t.TempDir(), "ca.pem"), testCertPEM(t, "laptop-17", false, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)))
	out, err := f.run(setupOptions{tokenFile: f.tokenFile, trustedCAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "warning: the trusted CA file "+ca+` holds a certificate for "laptop-17" that is not marked as a certificate authority`) {
		t.Errorf("setup printed no CA warning:\n%s", out)
	}
	if got := f.settings(mustPaths(t, f)); !strings.Contains(got, "WARDYN_TRUSTED_CA_FILE="+ca+"\n") {
		t.Errorf("the warned file is not written into the service:\n%s", got)
	}
}
