// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/certbundle"
)

// testCA is a CA and a leaf it signed for 127.0.0.1 and localhost.
type testCA struct {
	ca, leaf *x509.Certificate
	caKey    *ecdsa.PrivateKey
	leafKey  *ecdsa.PrivateKey
	caFile   string // the CA as a PEM file
}

func newTestCA(t *testing.T, cn string) testCA {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	f := filepath.Join(t.TempDir(), "bundle.pem")
	if err := os.WriteFile(f, certbundle.Encode([]*x509.Certificate{ca}), 0o600); err != nil {
		t.Fatal(err)
	}
	tc := testCA{ca: ca, caKey: caKey, caFile: f}
	tc.leaf, tc.leafKey = tc.mintLeaf(t, []net.IP{net.ParseIP("127.0.0.1")}, "localhost")
	return tc
}

// mintLeaf is a server certificate tc signed for ips and dns.
func (tc testCA) mintLeaf(t *testing.T, ips []net.IP, dns ...string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "org leaf"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: ips, DNSNames: dns,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, tc.ca, &leafKey.PublicKey, tc.caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(leafDER)
	return leaf, leafKey
}

// serve is an https server presenting tc's leaf; its /healthz publishes fps.
func (tc testCA) serve(t *testing.T, fps []string) *httptest.Server {
	t.Helper()
	return tc.serveLeaf(t, tc.leaf, tc.leafKey, fps)
}

func (tc testCA) serveLeaf(t *testing.T, leaf *x509.Certificate, leafKey *ecdsa.PrivateKey, fps []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			body := map[string]any{"status": "ok"}
			if fps != nil {
				body["org_ca_fingerprints"] = fps
			}
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: leafKey}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// isolateStore points the anchor store at a fresh directory.
func isolateStore(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("AppData", filepath.Join(home, "appdata"))
}

func addNonInteractive(t *testing.T, src, org string, fps ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := caAdd(&out, strings.NewReader(""), src, org, fps, time.Now())
	return out.String(), err
}

// TestAnchor_UsedOnlyForItsOrg: an anchor stored for org A verifies A, and is
// absent from the client for org B (the same server under another host name),
// from a host org A's transport dials that is not A, and from the runnerd
// download client (an origin with no anchors gets the default transport).
func TestAnchor_UsedOnlyForItsOrg(t *testing.T) {
	isolateStore(t)
	tc := newTestCA(t, "Corp Inspection Root")
	srv := tc.serve(t, nil)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	orgA := srv.URL // https://127.0.0.1:<port>
	orgB := "https://localhost:" + strconv.Itoa(port)

	if _, err := orgClient(orgA, 5*time.Second).Get(orgA + "/healthz"); err == nil {
		t.Fatal("org A verified before any anchor was added")
	}
	if out, err := addNonInteractive(t, tc.caFile, orgA, certbundle.Fingerprint(tc.ca)); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if resp, err := orgClient(orgA, 5*time.Second).Get(orgA + "/healthz"); err != nil {
		t.Fatalf("org A through its anchor: %v", err)
	} else {
		resp.Body.Close()
	}
	if _, err := orgClient(orgB, 5*time.Second).Get(orgB + "/healthz"); !isUnknownAuthority(err) {
		t.Fatalf("org B trusted org A's anchor: %v", err)
	}
	// Org A's transport reaching a host whose certificate (signed by the same
	// CA) does not name org A keeps to the system roots.
	otherLeaf, otherKey := tc.mintLeaf(t, nil, "localhost")
	other := tc.serveLeaf(t, otherLeaf, otherKey, nil)
	otherURL := "https://localhost:" + strconv.Itoa(other.Listener.Addr().(*net.TCPAddr).Port)
	if _, err := orgClient(orgA, 5*time.Second).Get(otherURL + "/healthz"); !isUnknownAuthority(err) {
		t.Fatalf("org A's anchor vouched for another host on its transport: %v", err)
	}
	if c := orgClient("https://github.com", 0); c.Transport != nil {
		t.Fatalf("an origin with no anchors got its own transport %T, want the default", c.Transport)
	}
}

func isUnknownAuthority(err error) bool {
	var ua x509.UnknownAuthorityError
	return errors.As(err, &ua)
}

// TestAnchor_StoreModeEnforced: the store is 0400 in 0700, and at use a file
// with a write bit or a group-writable directory fails every request closed.
func TestAnchor_StoreModeEnforced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes")
	}
	isolateStore(t)
	tc := newTestCA(t, "Corp Root")
	srv := tc.serve(t, nil)
	if out, err := addNonInteractive(t, tc.caFile, srv.URL, certbundle.Fingerprint(tc.ca)); err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	path, _, _ := anchorFile(srv.URL)
	fi, _ := os.Stat(path)
	di, _ := os.Stat(filepath.Dir(path))
	if fi.Mode().Perm() != 0o400 || di.Mode().Perm() != 0o700 {
		t.Fatalf("store modes file %04o dir %04o, want 0400 in 0700", fi.Mode().Perm(), di.Mode().Perm())
	}
	for _, tweak := range []struct {
		name string
		do   func()
		undo func()
	}{
		{"file 0600", func() { _ = os.Chmod(path, 0o600) }, func() { _ = os.Chmod(path, 0o400) }},
		{"dir 0770", func() { _ = os.Chmod(filepath.Dir(path), 0o770) }, func() { _ = os.Chmod(filepath.Dir(path), 0o700) }},
	} {
		tweak.do()
		_, err := orgClient(srv.URL, 5*time.Second).Get(srv.URL + "/healthz")
		var refused *anchorRefusedError
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: %v, want a refusal naming %s", tweak.name, err, path)
		}
		if h := caHint(err); !strings.Contains(h, "wardyn ca remove") {
			t.Errorf("%s: hint %q", tweak.name, h)
		}
		tweak.undo()
	}
}

func TestCAAdd_FingerprintRules(t *testing.T) {
	isolateStore(t)
	tc := newTestCA(t, "Corp Root")
	srv := tc.serve(t, nil)
	stored := func() bool { p, _, _ := anchorFile(srv.URL); _, err := os.Stat(p); return err == nil }

	orig := stdinIsTerminal
	t.Cleanup(func() { stdinIsTerminal = orig })
	stdinIsTerminal = func() bool { return false }
	if _, err := addNonInteractive(t, tc.caFile, srv.URL); err == nil || !strings.Contains(err.Error(), "--fingerprint") || stored() {
		t.Fatalf("non-interactive without --fingerprint: %v (stored %v)", err, stored())
	}
	wrong := "sha256:" + strings.Repeat("00", 32)
	if _, err := addNonInteractive(t, tc.caFile, srv.URL, wrong); err == nil || stored() {
		t.Fatalf("wrong fingerprint: %v (stored %v)", err, stored())
	}
	if _, err := addNonInteractive(t, tc.caFile, srv.URL, certbundle.Fingerprint(tc.ca), wrong); err == nil || stored() {
		t.Fatalf("an extra unmatched fingerprint: %v (stored %v)", err, stored())
	}
	two := filepath.Join(t.TempDir(), "two.pem")
	second := newTestCA(t, "Second Root")
	if err := os.WriteFile(two, certbundle.Encode([]*x509.Certificate{tc.ca, second.ca}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := addNonInteractive(t, two, srv.URL, certbundle.Fingerprint(tc.ca)); err == nil || stored() {
		t.Fatalf("a CA without its --fingerprint: %v (stored %v)", err, stored())
	}

	// At a terminal: none published states trust on first use; a "no" adds nothing.
	stdinIsTerminal = func() bool { return true }
	var out bytes.Buffer
	if err := caAdd(&out, strings.NewReader("n\n"), tc.caFile, srv.URL, nil, time.Now()); err == nil || stored() {
		t.Fatalf("declined: %v (stored %v)", err, stored())
	}
	if !strings.Contains(out.String(), msgTOFU) || !strings.Contains(out.String(), certbundle.Fingerprint(tc.ca)) {
		t.Fatalf("prompt without a published fingerprint:\n%s", out.String())
	}
	out.Reset()
	if err := caAdd(&out, strings.NewReader("y\n"), tc.caFile, srv.URL, nil, time.Now()); err != nil || !stored() {
		t.Fatalf("accepted: %v (stored %v)\n%s", err, stored(), out.String())
	}

	published := tc.serve(t, []string{certbundle.Fingerprint(tc.ca)})
	out.Reset()
	if err := caAdd(&out, strings.NewReader("y\n"), tc.caFile, published.URL, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), msgTOFU) || !strings.Contains(out.String(), msgCompare) {
		t.Fatalf("prompt with a published fingerprint:\n%s", out.String())
	}
}

func TestCAAdd_LeafIsNamedAndNotAdded(t *testing.T) {
	isolateStore(t)
	tc := newTestCA(t, "Corp Root")
	f := filepath.Join(t.TempDir(), "leaf.pem")
	if err := os.WriteFile(f, certbundle.Encode([]*x509.Certificate{tc.leaf}), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := addNonInteractive(t, f, "https://org.example", certbundle.Fingerprint(tc.leaf))
	if err == nil || !strings.Contains(out, "not added: not a CA certificate") {
		t.Fatalf("leaf: %v\n%s", err, out)
	}
}

// TestCAVerify_NamesTheSigner: through the anchor the chain is named as
// intercepted by the root; without it, an unknown authority with the add line.
func TestCAVerify_NamesTheSigner(t *testing.T) {
	isolateStore(t)
	tc := newTestCA(t, "Corp Inspection Root")
	srv := tc.serve(t, nil)
	var out bytes.Buffer
	if err := caVerify(&out, srv.URL); err == nil || !strings.Contains(out.String(), "unknown authority CN=Corp Inspection Root: add it with `wardyn ca add`") {
		t.Fatalf("without the anchor: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "presented: CN=org leaf") {
		t.Fatalf("chain not printed:\n%s", out.String())
	}
	if o, err := addNonInteractive(t, tc.caFile, srv.URL, certbundle.Fingerprint(tc.ca)); err != nil {
		t.Fatalf("add: %v\n%s", err, o)
	}
	out.Reset()
	if err := caVerify(&out, srv.URL); err != nil || !strings.Contains(out.String(), "this connection is intercepted by CN=Corp Inspection Root") ||
		!strings.Contains(out.String(), "heuristic") {
		t.Fatalf("through the anchor: %v\n%s", err, out.String())
	}
}

func TestCAHint_NamesThePresentedRoot(t *testing.T) {
	isolateStore(t)
	tc := newTestCA(t, "Corp Root")
	srv := tc.serve(t, nil)
	_, err := orgClient(srv.URL, 5*time.Second).Get(srv.URL)
	h := caHint(err)
	// The server sends only its leaf, so the chain presented ends there.
	if !strings.Contains(h, "CN=org leaf") || !strings.Contains(h, certbundle.Fingerprint(tc.leaf)) || !strings.Contains(h, "wardyn ca add") {
		t.Fatalf("hint %q", h)
	}
	if caHint(errors.New("other")) != "" {
		t.Fatal("hint for an unrelated error")
	}
}

// clientBuilderRE is a construction that would miss the anchors: a client or
// transport literal, the default client, or a helper that uses it.
var clientBuilderRE = regexp.MustCompile(`http\.(Client|Transport)\{|new\(http\.(Client|Transport)\)|http\.DefaultClient|http\.(Get|Head|Post|PostForm)\(`)

func clientBuilderViolations(files map[string]string) []string {
	var out []string
	for name, src := range files {
		if name == "orgclient.go" {
			continue
		}
		for i, line := range strings.Split(src, "\n") {
			if t := strings.TrimSpace(line); strings.HasPrefix(t, "//") {
				continue
			}
			if clientBuilderRE.MatchString(line) {
				out = append(out, name+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	return out
}

// TestOrgClient_IsTheOnlyClientBuilder: no non-test file of cmd/wardyn but
// orgclient.go builds an HTTP client or transport, so every dial carries the
// organisation's anchors and the no-redirect rule.
func TestOrgClient_IsTheOnlyClientBuilder(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, n := range names {
		if strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		files[n] = string(b)
	}
	if len(files) < 20 || files["orgclient.go"] == "" {
		t.Fatalf("read %d files; the census is vacuous", len(files))
	}
	if v := clientBuilderViolations(files); len(v) > 0 {
		t.Fatalf("build clients through orgClient (orgclient.go) instead:\n%s", strings.Join(v, "\n"))
	}
	// Planted twin: one violation of each shape must trip the guard.
	for _, plant := range []string{
		"var c = &http.Client{Timeout: 1}", "t := http.Transport{}", "x := new(http.Client)",
		"http.DefaultClient.Do(req)", "resp, err := http.Get(u)",
	} {
		if v := clientBuilderViolations(map[string]string{"planted.go": "package main\n" + plant + "\n"}); len(v) != 1 {
			t.Errorf("planted %q: %v, want one violation", plant, v)
		}
	}
}

// TestCA_NoInsecureSkipVerify: the trust code never turns verification off.
func TestCA_NoInsecureSkipVerify(t *testing.T) {
	paths, _ := filepath.Glob(filepath.Join("..", "..", "internal", "certbundle", "*.go"))
	paths = append(paths, "ca.go", "orgclient.go")
	if len(paths) < 4 {
		t.Fatalf("found %v; the guard is vacuous", paths)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "InsecureSkipVerify") && !strings.HasSuffix(p, "_test.go") {
			t.Errorf("%s mentions InsecureSkipVerify", p)
		}
	}
}
