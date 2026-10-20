// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// certPEM is a self-signed certificate named cn: a CA or not, valid until notAfter.
func certPEM(t *testing.T, cn string, isCA bool, notAfter time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-48 * time.Hour),
		NotAfter: notAfter, IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// The org-only CA file is refused, naming the file, when it is writable by group or others, owned by
// another account, unreadable, not certificates, expired or not a CA.
func TestLoadOrgCARefuses(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, filepath.Join(dir, "good.pem"), testCAPEM(t, "corp"))
	if b, n, err := loadOrgCA(good); err != nil || n != 1 || b == "" {
		t.Fatalf("a good file: %d certificates, %v", n, err)
	}
	if b, n, err := loadOrgCA(""); err != nil || b != "" || n != 0 {
		t.Fatalf("no file: %q %d %v; want nothing and no error", b, n, err)
	}
	groupWritable := writeFile(t, filepath.Join(dir, "gw.pem"), testCAPEM(t, "corp"))
	otherWritable := writeFile(t, filepath.Join(dir, "ow.pem"), testCAPEM(t, "corp"))
	for p, mode := range map[string]os.FileMode{groupWritable: 0o660, otherWritable: 0o602} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	for name, c := range map[string]struct{ path, want string }{
		"group writable": {groupWritable, "lets group or others write it"},
		"other writable": {otherWritable, "lets group or others write it"},
		"missing":        {filepath.Join(dir, "gone.pem"), "no such file"},
		"not a cert":     {writeFile(t, filepath.Join(dir, "junk.pem"), "not a certificate"), "no PEM-encoded certificates"},
		"expired":        {writeFile(t, filepath.Join(dir, "expired.pem"), certPEM(t, "old-root", true, time.Now().Add(-time.Hour))), "expired on"},
		"not a CA":       {writeFile(t, filepath.Join(dir, "leaf.pem"), certPEM(t, "a-leaf", false, time.Now().Add(time.Hour))), "is not a CA"},
		"a directory":    {dir, "not a regular file"},
	} {
		_, _, err := loadOrgCA(c.path)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), c.path) || !strings.Contains(err.Error(), "--org-ca-file / WARDYN_ORG_CA_FILE") {
			t.Errorf("%s: err=%v; want it to name the setting and %s and say %q", name, err, c.path, c.want)
		}
	}
}

// A file owned by another account is refused; one owned by root is how a fleet delivers it.
func TestReadOrgCARefusesAnotherAccountsFile(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "ca.pem"), testCAPEM(t, "corp"))
	owner, ok := fileOwner(mustStat(t, path))
	if !ok || owner == 0 {
		t.Skip("needs a platform that names the owner, and a test that does not run as root")
	}
	if _, _, err := readOrgCA(path, owner+1, time.Now(), func(string) {}); err == nil || !strings.Contains(err.Error(), "owned by another account") {
		t.Errorf("a file owned by uid %d read as uid %d: %v; want a refusal", owner, owner+1, err)
	}
	if _, _, err := readOrgCA(path, owner, time.Now(), func(string) {}); err != nil {
		t.Errorf("a file owned by its reader: %v", err)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// serveWithOrg runs serve against a TLS organisation signed by a CA no system trusts, and returns the
// options the builder saw and the status file serve kept while it failed to connect.
func serveWithOrg(t *testing.T, orgCAFile, trustedCAFile string) (serveOptions, runnerStatus, string) {
	t.Helper()
	org := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	t.Cleanup(org.Close)
	orgCA := writeFile(t, filepath.Join(t.TempDir(), "org-ca.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: org.Certificate().Raw})))
	switch orgCAFile {
	case "use":
		orgCAFile = orgCA
	}
	stateDir := filepath.Join(t.TempDir(), "runner")
	key, err := runneridentity.Generate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	identity := runneridentity.Identity{RunnerID: uuid.New(), PrivateKey: key, Fingerprint: runnerwire.Fingerprint(key.Public().(ed25519.PublicKey)), OrgURLSHA256: federation.OrgURLSHA256(org.URL)}
	if err := runneridentity.Save(stateDir, identity); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARDYN_ORG_CA_FILE", "")
	t.Setenv("WARDYN_TRUSTED_CA_FILE", "")
	var got serveOptions
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	noop := func(context.Context) {}
	cmd := newServeCommand(func(_ context.Context, _ runneridentity.Identity, _ string, opts serveOptions) (runnerRuntime, error) {
		got = opts
		return runnerRuntime{
			run:   func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
			close: func() error { return nil },
			hooks: runnerwire.RunnerHooks{
				Call:  func(context.Context, *runnerwire.Peer, uint32, string, json.RawMessage) (any, error) { return nil, nil },
				Open:  func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error { return nil },
				Clock: func(runnerwire.Hello, time.Time) error { return nil }, Pending: func(context.Context, *runnerwire.Peer, []runnerwire.PendingAction) error { return nil },
				StatePages: func(context.Context, func(runnerwire.StatePage) error) error { return nil },
				Replay:     func(context.Context, *runnerwire.Peer) error { return nil }, BeforeReady: func(context.Context, *runnerwire.Peer, bool) error { return nil },
				Lease: func(context.Context, runnerwire.Lease) error { return nil }, Revoked: func(context.Context) error { return nil }, Offline: noop,
			},
		}, nil
	})
	args := []string{"--org", org.URL, "--state-dir", stateDir}
	if orgCAFile != "" {
		args = append(args, "--org-ca-file", orgCAFile)
	}
	if trustedCAFile != "" {
		args = append(args, "--trusted-ca-file", trustedCAFile)
	}
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	var st runnerStatus
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			return got, runnerStatus{}, err.Error()
		default:
		}
		if b, err := os.ReadFile(filepath.Join(stateDir, statusFile)); err == nil && json.Unmarshal(b, &st) == nil && st.LastError != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	b, _ := os.ReadFile(filepath.Join(stateDir, statusFile))
	if _, err := os.Stat(filepath.Join(stateDir, "trusted-ca.pem")); err == nil && trustedCAFile == "" {
		t.Error("serve wrote the sandboxes' trust file although only an org-only CA was named")
	}
	return got, st, string(b)
}

// The org CA reaches the organisation connection (the failure moves from an unknown authority to the
// organisation's own answer) and nothing a run sees: not the bundle handed to the builder, not the file
// every sandbox mounts.
func TestServeOrgCATrustsTheOrgConnectionOnly(t *testing.T) {
	got, st, raw := serveWithOrg(t, "use", "")
	if got.trustedCA != "" || got.trustedCAPath != "" {
		t.Errorf("the org CA reached the builder's run trust: bundle=%d bytes path=%q", len(got.trustedCA), got.trustedCAPath)
	}
	if st.LastError == "" || strings.Contains(st.LastError, "certificate") || strings.Contains(st.LastError, "x509") {
		t.Errorf("with --org-ca-file the connection still fails on the certificate: %q\n%s", st.LastError, raw)
	}
	_, st, _ = serveWithOrg(t, "", "")
	if !strings.Contains(st.LastError, "certificate") && !strings.Contains(st.LastError, "x509") {
		t.Errorf("without it the connection should fail on the unknown authority: %q", st.LastError)
	}
}

// Naming a file as run trust does not trust it for the organisation by accident of the old default: the
// two union for the org connection, and only --trusted-ca-file reaches the sandboxes.
func TestServeUnionsTheTwoCAFilesForTheOrgConnection(t *testing.T) {
	other := writeFile(t, filepath.Join(t.TempDir(), "other.pem"), testCAPEM(t, "other"))
	got, st, raw := serveWithOrg(t, "use", other)
	if !strings.Contains(got.trustedCA, "CERTIFICATE") || got.trustedCAPath == "" {
		t.Errorf("--trusted-ca-file no longer reaches the builder: %+v", got)
	}
	if strings.Contains(st.LastError, "x509") || strings.Contains(st.LastError, "certificate") {
		t.Errorf("the union lost the org-only file: %q\n%s", st.LastError, raw)
	}
}

func TestServeRefusesABadOrgCAFileNamingIt(t *testing.T) {
	bad := writeFile(t, filepath.Join(t.TempDir(), "bad.pem"), "nothing")
	_, _, msg := serveWithOrg(t, bad, "")
	if !strings.Contains(msg, "--org-ca-file / WARDYN_ORG_CA_FILE "+bad) {
		t.Errorf("the refusal %q does not name the setting and the file", msg)
	}
}

// Registration against a TLS server signed by a CA no system trusts succeeds with --org-ca-file, and
// without it fails with the hint that names the flag. The hint spends nothing.
func TestRegisterOrgCAFile(t *testing.T) {
	var org string
	called := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		var req types.RunnerRegisterRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.RunnerRegistration{RunnerID: uuid.New(), Fingerprint: runnerwire.Fingerprint(req.PublicKey), State: types.RunnerUnclaimed, OrgURLSHA256: federation.OrgURLSHA256(org), Limits: req.Limits})
	}))
	defer srv.Close()
	org = srv.URL
	root := t.TempDir()
	tokenFile := writeFile(t, filepath.Join(root, "token"), "wdr_"+strings.Repeat("c", 64))
	ca := writeFile(t, filepath.Join(root, "ca.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})))
	k8s := []string{"--substrate=k8s", "--arch=amd64", "--max-runs=2", "--total-cpu=4", "--total-memory=8", "--total-disk=50", "--per-run-cpu=2", "--per-run-memory=4", "--per-run-disk=25"}
	run := func(state string, extra ...string) error {
		t.Setenv("WARDYN_ORG_CA_FILE", "")
		cmd := newRegisterCommand()
		cmd.SetOut(&strings.Builder{})
		cmd.SetIn(strings.NewReader(""))
		cmd.SetArgs(append(append([]string{"--org", org, "--token-file", tokenFile, "--state-dir", filepath.Join(root, state)}, k8s...), extra...))
		return cmd.Execute()
	}
	err := run("same")
	var unknown x509.UnknownAuthorityError
	if err == nil || called || !errors.As(err, &unknown) || !strings.Contains(err.Error(), "--org-ca-file <file> (or WARDYN_ORG_CA_FILE)") {
		t.Fatalf("without the CA: called=%v err=%v; want the unknown-authority refusal naming --org-ca-file", called, err)
	}
	if err := run("bad", "--org-ca-file", filepath.Join(root, "missing.pem")); err == nil || called {
		t.Fatalf("an unreadable org CA file was accepted: called=%v err=%v", called, err)
	}
	if err := run("same", "--org-ca-file", ca); err != nil || !called { // the state directory of the refused try
		t.Fatalf("with the CA: called=%v err=%v", called, err)
	}
}

// Setup checks the org CA file before the token is spent, writes its absolute path into the service, keeps
// it on a rerun, registers with it, and never makes it a run's trust.
func TestSetupWritesKeepsAndRegistersWithTheOrgCA(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			clearProxyEnv(t)
			t.Setenv("WARDYN_ORG_CA_FILE", "")
			f := newSetupFixture(t, goos)
			tls := httptest.NewTLSServer(http.HandlerFunc(f.serve))
			t.Cleanup(tls.Close)
			f.org = tls.URL
			ca := writeFile(t, filepath.Join(t.TempDir(), "org-ca.pem"), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tls.Certificate().Raw})))
			p := mustPaths(t, f)
			service := func() string {
				if goos == "linux" {
					return f.read(p.envFile)
				}
				return f.read(p.plistFile)
			}
			bad := writeFile(t, filepath.Join(t.TempDir(), "bad.pem"), "nothing")
			if out, err := f.run(setupOptions{tokenFile: f.tokenFile, orgCAFile: bad}); err == nil || !strings.Contains(err.Error(), bad) || f.count("POST /api/v1/runners/register") != 0 {
				t.Fatalf("a bad org CA file: err=%v out=%s registered=%d; want a refusal naming it before the token", err, out, f.count("POST /api/v1/runners/register"))
			}
			if out, err := f.run(setupOptions{tokenFile: f.tokenFile}); err == nil || f.count("POST /api/v1/runners/register") != 0 || !strings.Contains(err.Error(), "--org-ca-file") {
				t.Fatalf("an organisation signed by an unknown CA: err=%v out=%s; want the refusal naming --org-ca-file", err, out)
			}
			if out, err := f.run(setupOptions{tokenFile: f.tokenFile, orgCAFile: ca}); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if f.count("POST /api/v1/runners/register") != 1 {
				t.Fatal("setup did not register with the org CA")
			}
			if got := service(); !strings.Contains(got, ca) || strings.Contains(got, "WARDYN_TRUSTED_CA_FILE="+ca) || strings.Contains(got, "<key>WARDYN_TRUSTED_CA_FILE</key>") {
				t.Errorf("the service should name the org CA and not make it run trust:\n%s", got)
			}
			f.flagless = true
			if _, err := f.run(setupOptions{}); err != nil {
				t.Fatal(err)
			}
			if got := service(); !strings.Contains(got, ca) {
				t.Errorf("a rerun lost the org CA:\n%s", got)
			}
		})
	}
}

// A file in a directory group or others can write is refused, since another account could replace it
// after the check; a directory only the account writes is fine, and so is a sticky root-owned one (/tmp).
func TestLoadOrgCARefusesAWritableDirectory(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, "ca.pem"), testCAPEM(t, "corp"))
	if _, _, err := loadOrgCA(path); err != nil {
		t.Fatalf("a private directory: %v", err)
	}
	for _, mode := range []os.FileMode{0o770, 0o707, 0o777, 0o777 | os.ModeSticky} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadOrgCA(path); err == nil || !strings.Contains(err.Error(), "the directory "+dir) || !strings.Contains(err.Error(), path) {
			t.Errorf("directory mode %v: err=%v; want a refusal naming the directory and the file", mode, err)
		}
	}
	if st, err := os.Stat(os.TempDir()); err == nil && st.Mode()&os.ModeSticky != 0 && st.Mode().Perm() == 0o777 {
		if owner, ok := fileOwner(st); ok && owner == 0 {
			file := filepath.Join(os.TempDir(), "wardyn-org-ca-test-"+filepath.Base(dir)+".pem")
			writeFile(t, file, testCAPEM(t, "corp"))
			t.Cleanup(func() { os.Remove(file) })
			if _, _, err := loadOrgCA(file); err != nil {
				t.Errorf("a file directly in the sticky, root-owned %s: %v", os.TempDir(), err)
			}
		}
	}
}

// The directory that is checked is the file's real one: a symlink from a private directory to a file in a
// directory others can write is refused.
func TestLoadOrgCARefusesAFileReachedThroughASymlinkToAWritableDirectory(t *testing.T) {
	private, shared := t.TempDir(), t.TempDir()
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	real := writeFile(t, filepath.Join(shared, "ca.pem"), testCAPEM(t, "corp"))
	link := filepath.Join(private, "ca.pem")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if _, _, err := loadOrgCA(link); err == nil || !strings.Contains(err.Error(), "the directory "+shared) {
		t.Errorf("err=%v; want a refusal naming the directory %s the file is really in", err, shared)
	}
}

// anchorFileCase is one row of testdata/anchor-file-cases.json, the file rule shared with the other loader
// of CA anchors: owners are named "self", "root" or "other" and set through the owner seam, so none needs
// the test to run as root.
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

func anchorCert(t *testing.T, kind string) (pemText string, fingerprint string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: kind}, NotBefore: time.Now().Add(-48 * time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: kind != "not_ca", BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	switch kind {
	case "expired":
		tpl.NotAfter = time.Now().Add(-time.Hour)
	case "not_yet_valid":
		tpl.NotBefore = time.Now().Add(24 * time.Hour)
		tpl.NotAfter = time.Now().Add(48 * time.Hour)
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), fmt.Sprintf("%x", sha256.Sum256(der))
}

func TestLoadOrgCAFileRule(t *testing.T) {
	raw, err := os.ReadFile("testdata/anchor-file-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []anchorFileCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	mode := func(s string, sticky bool) os.FileMode {
		m, err := strconv.ParseUint(s, 8, 32)
		if err != nil {
			t.Fatal(err)
		}
		if sticky {
			return os.FileMode(m) | os.ModeSticky
		}
		return os.FileMode(m)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "data")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			var pems, skippedFPs, usable []string
			for _, kind := range c.Certs {
				text, fp := anchorCert(t, kind)
				pems = append(pems, text)
				if kind == "valid" {
					usable = append(usable, fp)
				} else {
					skippedFPs = append(skippedFPs, fp)
				}
			}
			file := writeFile(t, filepath.Join(dir, "ca.pem"), strings.Join(pems, ""))
			if err := os.Chmod(file, mode(c.FileMode, false)); err != nil {
				t.Fatal(err)
			}
			path := file
			if c.ViaLinkDirMode != "" {
				linkDir := filepath.Join(root, "link")
				if err := os.Mkdir(linkDir, 0o700); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(linkDir, "ca.pem")
				if err := os.Symlink(file, path); err != nil {
					t.Skip(err)
				}
				if err := os.Chmod(linkDir, mode(c.ViaLinkDirMode, false)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(dir, mode(c.DirMode, c.DirSticky)); err != nil {
				t.Fatal(err)
			}
			uid := func(who string) int {
				switch who {
				case "root":
					return 0
				case "other":
					return os.Geteuid() + 1
				}
				return os.Geteuid()
			}
			fileInfo, dirInfo := mustStat(t, file), mustStat(t, dir)
			prev := orgCAOwner
			t.Cleanup(func() { orgCAOwner = prev })
			orgCAOwner = func(st os.FileInfo) (int, bool) {
				switch {
				case os.SameFile(st, fileInfo):
					return uid(c.FileOwner), true
				case os.SameFile(st, dirInfo):
					return uid(c.DirOwner), true
				}
				return uid("self"), true
			}
			var warnings []string
			bundle, n, err := readOrgCA(path, os.Geteuid(), time.Now(), func(m string) { warnings = append(warnings, m) })
			if c.Expect == "refuse" {
				if err == nil {
					t.Fatalf("loaded %d certificates; want a refusal", n)
				}
			} else if err != nil || n != len(usable) || bundle == "" {
				t.Fatalf("n=%d err=%v; want %d certificates and no error", n, err, len(usable))
			}
			if len(warnings) != c.Warnings {
				t.Fatalf("%d warnings %q; want %d", len(warnings), warnings, c.Warnings)
			}
			for i, w := range warnings {
				if !strings.Contains(w, skippedFPs[i]) {
					t.Errorf("warning %q does not name the skipped certificate's sha256 %s", w, skippedFPs[i])
				}
			}
		})
	}
}
