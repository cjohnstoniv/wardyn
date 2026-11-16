// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package macvm_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// testRunCA is a run CA as the control plane mints one, returned parsed, with its key and as PEM.
func testRunCA(t *testing.T, notAfter time.Time) (*x509.Certificate, *ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Wardyn run egress-inspection CA", Organization: []string{"Wardyn"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// leafUnder is a serverAuth leaf the proxy would mint, signed by key under parent.
func leafUnder(t *testing.T, parent *x509.Certificate, key *ecdsa.PrivateKey, dns []string, ips []net.IP) *x509.Certificate {
	t.Helper()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &leafKey.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

func openCA(t *testing.T) (*macvm.InspectionCA, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "inspection-ca")
	ca, err := macvm.OpenInspectionCA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return ca, dir
}

func crossed(t *testing.T, ca *macvm.InspectionCA, runPEM string, terminated []string, validFor time.Duration) *x509.Certificate {
	t.Helper()
	out, err := ca.CrossSign(runPEM, terminated, validFor)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode([]byte(out))
	if blk == nil {
		t.Fatal("CrossSign returned no PEM")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func macCert(t *testing.T, ca *macvm.InspectionCA) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode([]byte(ca.PEM()))
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// expiredCADir is a directory holding a CA whose validity ended an hour ago.
func expiredCADir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "expired")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(7),
		Subject:               pkix.Name{CommonName: "expired test CA"},
		NotBefore:             time.Now().Add(-2 * time.Hour),
		NotAfter:              time.Now().Add(-time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            1,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is cut by the umask; make the modes exact.
	if err := os.Chmod(filepath.Join(dir, "key.pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "ca.pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func pkcs8PEM(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestInspectionCAIsMadeOnceAndKeptPrivate(t *testing.T) {
	ca, dir := openCA(t)
	again, err := macvm.OpenInspectionCA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if again.Fingerprint() != ca.Fingerprint() {
		t.Errorf("a second open made another CA")
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "key.pem"): 0o600, filepath.Join(dir, "ca.pem"): 0o644} {
		fi, err := os.Lstat(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s: mode %#o, want %#o", path, fi.Mode().Perm(), want)
		}
	}
	if ca.CertPath() != filepath.Join(dir, "ca.pem") {
		t.Errorf("CertPath = %s", ca.CertPath())
	}
	c := macCert(t, ca)
	if !c.IsCA || c.MaxPathLen != 1 {
		t.Errorf("IsCA %v MaxPathLen %d", c.IsCA, c.MaxPathLen)
	}
	if !c.NotAfter.After(time.Now().AddDate(4, 11, 0)) {
		t.Errorf("NotAfter %s", c.NotAfter)
	}
	sum := sha256.Sum256(c.Raw)
	if ca.Fingerprint() != hex.EncodeToString(sum[:]) {
		t.Errorf("Fingerprint = %s", ca.Fingerprint())
	}
	if ca.Expired(time.Now()) {
		t.Error("a new CA reads expired")
	}
	if !ca.Expired(time.Now().AddDate(6, 0, 0)) {
		t.Error("a CA does not read expired in six years")
	}
}

func TestInspectionCARefusedAndKeptWhenNotPrivate(t *testing.T) {
	spoilers := []struct {
		name  string
		spoil func(t *testing.T, dir string)
	}{
		{"a key others can read", func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(dir, "key.pem"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a certificate others can write", func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(dir, "ca.pem"), 0o666); err != nil {
				t.Fatal(err)
			}
		}},
		{"an open directory", func(t *testing.T, dir string) {
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"a missing key", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "key.pem")); err != nil {
				t.Fatal(err)
			}
		}},
		{"another CA's key", func(t *testing.T, dir string) {
			_, other := openCA(t)
			b, err := os.ReadFile(filepath.Join(other, "key.pem"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "key.pem"), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a symlinked key", func(t *testing.T, dir string) {
			if err := os.Rename(filepath.Join(dir, "key.pem"), filepath.Join(dir, "key.real")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("key.real", filepath.Join(dir, "key.pem")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a symlinked certificate", func(t *testing.T, dir string) {
			if err := os.Rename(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.real")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("ca.real", filepath.Join(dir, "ca.pem")); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, s := range spoilers {
		t.Run(s.name, func(t *testing.T) {
			ca, dir := openCA(t)
			s.spoil(t, dir)
			if _, err := macvm.OpenInspectionCA(context.Background(), dir); err == nil {
				t.Error("OpenInspectionCA accepted it")
			}
			// The certificate is kept, not replaced by a new CA (a symlink reads through to the same bytes).
			b, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != ca.PEM() {
				t.Error("the certificate was changed")
			}
		})
	}
}

func TestRemoveInspectionCA(t *testing.T) {
	_, dir := openCA(t)
	removed, err := macvm.RemoveInspectionCA(dir)
	if err != nil || !removed {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	for _, f := range []string{"key.pem", "ca.pem"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: %v", f, err)
		}
	}
	if removed, err := macvm.RemoveInspectionCA(dir); removed || err != nil {
		t.Errorf("second remove = %v, %v", removed, err)
	}
	if removed, err := macvm.RemoveInspectionCA(filepath.Join(t.TempDir(), "none")); removed || err != nil {
		t.Errorf("remove of a never-made CA = %v, %v", removed, err)
	}
}

func TestReadInspectionCANeverMakesOne(t *testing.T) {
	ctx := context.Background()
	none := filepath.Join(t.TempDir(), "none")
	if _, err := macvm.ReadInspectionCA(ctx, none); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing directory: %v", err)
	}
	if _, err := os.Lstat(none); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directory was made: %v", err)
	}

	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := macvm.ReadInspectionCA(ctx, empty); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an empty directory: %v", err)
	}
	for _, f := range []string{"key.pem", "ca.pem"} {
		if _, err := os.Lstat(filepath.Join(empty, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was made: %v", f, err)
		}
	}

	ca, dir := openCA(t)
	got, err := macvm.ReadInspectionCA(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint() != ca.Fingerprint() {
		t.Error("a read returned another CA")
	}
	if err := os.Chmod(filepath.Join(dir, "key.pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := macvm.ReadInspectionCA(ctx, dir); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Errorf("a key others can read: %v", err)
	}
	for _, f := range []string{"key.pem", "ca.pem"} {
		if _, err := os.Lstat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestInspectedNames(t *testing.T) {
	for _, tc := range []struct {
		name               string
		in, permitted, exc []string
	}{
		{"hosts and names x509 cannot encode",
			[]string{"api.example.com:443", "API.example.com", "[::1]:443", "10.0.0.1:443", "", "*.x.example", "bücher.example", "a..b", ".x.example", "h:1:2"},
			[]string{"api.example.com"}, []string{".api.example.com"}},
		{"a host under another",
			[]string{"example.com", "api.example.com:443"},
			[]string{"example.com", "api.example.com"}, []string{".api.example.com"}},
		{"nil", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, e := macvm.InspectedNames(tc.in)
			if !reflect.DeepEqual(p, tc.permitted) || !reflect.DeepEqual(e, tc.exc) {
				t.Errorf("got %q, %q; want %q, %q", p, e, tc.permitted, tc.exc)
			}
		})
	}
}

func verifyWith(leaf *x509.Certificate, roots, inter []*x509.Certificate, name string) error {
	r, i := x509.NewCertPool(), x509.NewCertPool()
	for _, c := range roots {
		r.AddCert(c)
	}
	for _, c := range inter {
		i.AddCert(c)
	}
	_, err := leaf.Verify(x509.VerifyOptions{Roots: r, Intermediates: i, DNSName: name})
	return err
}

func TestCrossSignedRunCAChainsToBothRoots(t *testing.T) {
	ca, _ := openCA(t)
	run, key, runPEM := testRunCA(t, time.Now().AddDate(1, 0, 0))
	cp := crossed(t, ca, runPEM, []string{"api.example.com:443", "git.example.com"}, 0)
	copyPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cp.Raw})
	if _, err := tls.X509KeyPair(copyPEM, pkcs8PEM(t, key)); err != nil {
		t.Errorf("the proxy cannot load the copy: %v", err)
	}
	if !bytes.Equal(cp.RawSubject, run.RawSubject) || !bytes.Equal(cp.SubjectKeyId, run.SubjectKeyId) {
		t.Error("the copy does not keep the run CA's subject and key id")
	}
	if !cp.IsCA || !cp.MaxPathLenZero || !cp.PermittedDNSDomainsCritical {
		t.Errorf("IsCA %v MaxPathLenZero %v critical %v", cp.IsCA, cp.MaxPathLenZero, cp.PermittedDNSDomainsCritical)
	}
	if !reflect.DeepEqual(cp.PermittedDNSDomains, []string{"api.example.com", "git.example.com"}) {
		t.Errorf("permitted %q", cp.PermittedDNSDomains)
	}
	if len(cp.ExcludedIPRanges) != 2 {
		t.Errorf("excluded IP ranges %d", len(cp.ExcludedIPRanges))
	}
	mac := macCert(t, ca)
	if bytes.Equal(mac.RawSubject, run.RawSubject) {
		t.Error("the Mac CA and the run CA share a subject")
	}
	leaf := leafUnder(t, cp, key, []string{"api.example.com"}, nil)
	if err := verifyWith(leaf, []*x509.Certificate{run}, nil, "api.example.com"); err != nil {
		t.Errorf("via the run CA: %v", err)
	}
	if err := verifyWith(leaf, []*x509.Certificate{mac}, []*x509.Certificate{cp}, "api.example.com"); err != nil {
		t.Errorf("via the Mac CA: %v", err)
	}
	if err := verifyWith(leaf, []*x509.Certificate{mac}, nil, "api.example.com"); err == nil {
		t.Error("the Mac CA verifies the leaf without the copy")
	}
}

func TestCrossSignedRunCAIsLimitedToTheInspectedHosts(t *testing.T) {
	ca, _ := openCA(t)
	run, key, runPEM := testRunCA(t, time.Now().AddDate(1, 0, 0))
	cp := crossed(t, ca, runPEM, []string{"api.example.com:443", "git.example.com"}, 0)
	mac := macCert(t, ca)
	viaMac := func(dns []string, ips []net.IP, name string) error {
		return verifyWith(leafUnder(t, cp, key, dns, ips), []*x509.Certificate{mac}, []*x509.Certificate{cp}, name)
	}
	if err := viaMac([]string{"tunnel.example.org"}, nil, "tunnel.example.org"); err == nil {
		t.Error("a Tunnel-only host verifies")
	}
	if err := viaMac([]string{"x.api.example.com"}, nil, "x.api.example.com"); err == nil {
		t.Error("a subdomain of an Inspected host verifies")
	}
	if err := viaMac(nil, []net.IP{net.ParseIP("203.0.113.7")}, "203.0.113.7"); err == nil {
		t.Error("an IP address verifies")
	}
	if err := viaMac([]string{"git.example.com"}, nil, "git.example.com"); err != nil {
		t.Errorf("an Inspected host: %v", err)
	}
	leaf := leafUnder(t, cp, key, []string{"tunnel.example.org"}, nil)
	if err := verifyWith(leaf, []*x509.Certificate{run}, nil, "tunnel.example.org"); err != nil {
		t.Errorf("the guest's own bundle path: %v", err)
	}
}

func TestCrossSignIsShortLived(t *testing.T) {
	ca, _ := openCA(t)
	_, _, runPEM := testRunCA(t, time.Now().AddDate(10, 0, 0))
	week := 7 * 24 * time.Hour
	for _, validFor := range []time.Duration{0, 30 * 24 * time.Hour} {
		cp := crossed(t, ca, runPEM, []string{"api.example.com"}, validFor)
		if !cp.NotAfter.After(time.Now().Add(week-time.Minute)) || cp.NotAfter.After(time.Now().Add(week+time.Minute)) {
			t.Errorf("validFor %s: NotAfter %s", validFor, cp.NotAfter)
		}
	}
	cp := crossed(t, ca, runPEM, []string{"api.example.com"}, 2*time.Hour)
	if cp.NotAfter.After(time.Now().Add(2*time.Hour + time.Minute)) {
		t.Errorf("validFor 2h: NotAfter %s", cp.NotAfter)
	}
}

func TestCrossSignRefuses(t *testing.T) {
	ca, _ := openCA(t)
	_, _, good := testRunCA(t, time.Now().AddDate(1, 0, 0))
	_, _, expiredRun := testRunCA(t, time.Now().Add(-time.Minute))
	api := []string{"api.example.com:443"}
	for _, tc := range []struct {
		name       string
		pem        string
		terminated []string
	}{
		{"not a certificate", "not a certificate", api},
		{"a CA that signs CAs", ca.PEM(), api},
		{"no terminated host", good, nil},
		{"an IP address only", good, []string{"10.0.0.1:443"}},
		{"a name x509 cannot encode", good, []string{"bücher.example"}},
		{"an expired run CA", expiredRun, api},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ca.CrossSign(tc.pem, tc.terminated, 0); err == nil {
				t.Error("CrossSign accepted it")
			}
		})
	}
	t.Run("an expired Mac CA", func(t *testing.T) {
		old, err := macvm.OpenInspectionCA(context.Background(), expiredCADir(t))
		if err != nil {
			t.Fatal(err)
		}
		if !old.Expired(time.Now()) {
			t.Fatal("the test CA is not expired")
		}
		if _, err := old.CrossSign(good, api, 0); err == nil {
			t.Error("an expired CA re-issued")
		}
	})
}

func TestWithInspectionCAPinsTheFileAndOwnsItsName(t *testing.T) {
	ca, _ := openCA(t)
	l := macvm.BaseList{Version: 1, Items: []macvm.BaseItem{{Kind: macvm.ItemKindPkg, Name: "edr", Path: "/opt/edr.pkg", SHA256: strings.Repeat("a", 64)}}}
	out, err := macvm.WithInspectionCA(l, ca.CertPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 2 || len(l.Items) != 1 {
		t.Fatalf("items: out %d, in %d", len(out.Items), len(l.Items))
	}
	b, err := os.ReadFile(ca.CertPath())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	last := out.Items[1]
	if last.Kind != macvm.ItemKindCA || last.Name != macvm.InspectionCAItem || last.Path != ca.CertPath() || last.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("last item %+v", last)
	}
	if d1, d2 := l.Digest(), out.Digest(); d1 == d2 {
		t.Error("the digest does not change with the item")
	}
	bad := `{"version":1,"items":[{"kind":"ca","name":"` + macvm.InspectionCAItem + `","path":"/x.pem","sha256":"` + strings.Repeat("a", 64) + `"}]}`
	if _, err := macvm.ParseBaseList([]byte(bad)); err == nil {
		t.Error("a list may use the inspection CA's name")
	}
}

func TestInspectionCATrustedInReadsTheBaseRecord(t *testing.T) {
	ca, _ := openCA(t)
	fp := ca.Fingerprint()
	item := func(name, fingerprint, state string) *macvm.ItemRecord {
		return &macvm.ItemRecord{Kind: macvm.ItemKindCA, Name: name, SHA256: strings.Repeat("a", 64), State: state, Fingerprint: fingerprint}
	}
	for _, tc := range []struct {
		name string
		rec  *macvm.ItemRecord
		want bool
	}{
		{"trusted", item(macvm.InspectionCAItem, fp, macvm.ItemTrusted), true},
		{"not trusted", item(macvm.InspectionCAItem, fp, macvm.ItemNotTrusted), false},
		{"an older CA", item(macvm.InspectionCAItem, strings.Repeat("0", 64), macvm.ItemTrusted), false},
		{"no such item", item("corp-root", fp, macvm.ItemTrusted), false},
		{"no tools.json", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.rec != nil {
				writeToolsJSON(t, dir, macvm.ToolsRecord{Items: []macvm.ItemRecord{*tc.rec}})
			}
			if got := ca.TrustedIn(dir); got != tc.want {
				t.Errorf("TrustedIn = %v, want %v", got, tc.want)
			}
		})
	}
}
