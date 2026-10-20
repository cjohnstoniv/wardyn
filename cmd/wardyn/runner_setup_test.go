// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// setupRig is a fake organisation and a fake wardyn-runnerd for `wardyn runner setup`. HOME is a temp
// directory; the fake runnerd records what it was given in rec, outside HOME.
type setupRig struct {
	t                 *testing.T
	srv               *httptest.Server
	home, rec, binDir string
	tmp               string // TMPDIR while the rig runs
	runnerID          uuid.UUID
	mu                sync.Mutex
	paths             []string
	mints             int
	claimed, online   bool
	version           string
	mintStatus        int
	mintBody          string
	claimStatus       int
	advertised        string
	signIn            bool         // /healthz.cli_sign_in
	fallback          http.Handler // the sign-in fake, for routes this rig does not answer
	mintBearer        string
	assets            map[string][]byte // download path -> bytes
}

const setupTestToken = "wdr_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newSetupRig(t *testing.T) *setupRig {
	t.Helper()
	r := &setupRig{t: t, home: t.TempDir(), rec: t.TempDir(), binDir: t.TempDir(), runnerID: uuid.New(), online: true, assets: map[string][]byte{}}
	// TLS, because a download base must be https; the default transport trusts this server for the test.
	r.srv = httptest.NewTLSServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = r.srv.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	t.Setenv("HOME", r.home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(r.home, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(r.home, ".local", "share"))
	t.Setenv("PATH", t.TempDir()+":/usr/bin:/bin")
	t.Setenv("WARDYN_URL", "")
	t.Setenv("WARDYN_TOKEN", "")
	t.Setenv("WARDYN_ADMIN_TOKEN", "")
	r.tmp = t.TempDir()
	t.Setenv("TMPDIR", r.tmp)
	restore := []func(){}
	swap := func(f func()) { restore = append(restore, f) }
	oldGOOS, oldPin, oldVer, oldExe, oldWait, oldPoll, oldRel, oldPkg, oldOwner := setupGOOS, pinnedRunnerdSHA256, setupVersion, setupExecutable, setupConnectWait, setupConnectPoll, releaseDownloadBase, packageRunnerd, packageOwner
	swap(func() {
		setupGOOS, pinnedRunnerdSHA256, setupVersion, setupExecutable, setupConnectWait, setupConnectPoll, releaseDownloadBase, packageRunnerd, packageOwner = oldGOOS, oldPin, oldVer, oldExe, oldWait, oldPoll, oldRel, oldPkg, oldOwner
	})
	t.Cleanup(func() {
		for _, f := range restore {
			f()
		}
	})
	setupGOOS = "linux"
	setupVersion = func() string { return "0.10.0" }
	emptyDir := t.TempDir()
	setupExecutable = func() (string, error) { return filepath.Join(emptyDir, "wardyn"), nil }
	setupConnectWait, setupConnectPoll = 200*time.Millisecond, 10*time.Millisecond
	releaseDownloadBase = r.srv.URL + "/release"
	packageRunnerd = "/nonexistent/wardyn-runnerd"
	return r
}

func (r *setupRig) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, req.Method+" "+req.URL.Path)
	me := "/api/v1/me/runners/" + r.runnerID.String()
	switch p := req.URL.Path; {
	case p == "/healthz":
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "cli_artifact_base": r.advertised, "cli_sign_in": r.signIn})
	case req.Method == http.MethodPost && p == "/api/v1/me/runners/tokens":
		r.mints++
		r.mintBearer = strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		if r.mintStatus != 0 {
			w.WriteHeader(r.mintStatus)
			_, _ = w.Write([]byte(r.mintBody))
			return
		}
		_ = json.NewEncoder(w).Encode(types.RunnerRegistrationToken{ID: uuid.New(), Token: setupTestToken, ExpiresAt: time.Now().Add(time.Hour)})
	case strings.Contains(p, "tokens"):
		r.t.Errorf("setup called another mint route: %s %s", req.Method, p)
		w.WriteHeader(http.StatusTeapot)
	case req.Method == http.MethodPost && p == me+"/claim":
		if r.claimStatus != 0 {
			w.WriteHeader(r.claimStatus)
			_, _ = w.Write([]byte(`{"error":"this runner has already been claimed"}`))
			return
		}
		r.claimed = true
		_ = json.NewEncoder(w).Encode(types.RunnerRegistration{RunnerID: r.runnerID, State: types.RunnerClaimed})
	case req.Method == http.MethodGet && p == me:
		state := types.RunnerUnclaimed
		if r.claimed {
			state = types.RunnerClaimed
		}
		_ = json.NewEncoder(w).Encode(types.RunnerView{ID: r.runnerID, State: state, Online: r.claimed && r.online, Version: r.version})
	case r.assets[p] != nil:
		_, _ = w.Write(r.assets[p])
	case p == "/release/v0.10.0/wardyn-runnerd-linux-amd64":
		http.Redirect(w, req, "/objects/runnerd", http.StatusFound)
	case r.fallback != nil:
		r.mu.Unlock()
		r.fallback.ServeHTTP(w, req)
		r.mu.Lock()
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (r *setupRig) called(prefix string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, p := range r.paths {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out
}

func (r *setupRig) stateDir() string { return filepath.Join(r.home, ".config", "wardyn", "runner") }

// fakeRunnerd writes a wardyn-runnerd script that records its argv, environment and the fd 3 token in
// rec, and then (exit 0) writes runner.json for org as a registration would. It returns its path and hash.
func (r *setupRig) fakeRunnerd(dir string, exit int) (string, string) {
	r.t.Helper()
	id := fmt.Sprintf(`{"runner_id":%q,"key_fingerprint":"fp-1","org_url_sha256":%q}`, r.runnerID, federation.OrgURLSHA256(r.srv.URL))
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %[1]s/argv
/usr/bin/env > %[1]s/env
case " $* " in *" --token-file /dev/fd/3 "*) /bin/cat /dev/fd/3 > %[1]s/token ;; esac
[ %[2]d -eq 0 ] || exit %[2]d
/bin/mkdir -p %[3]s
printf '%%s' '%[4]s' > %[3]s/runner.json
`, r.rec, exit, r.stateDir(), id)
	path := filepath.Join(dir, "wardyn-runnerd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		r.t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(script))
	return path, hex.EncodeToString(sum[:])
}

func (r *setupRig) run(args ...string) (string, error) {
	r.t.Helper()
	root := rootCmd()
	var out strings.Builder
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(""))
	root.SetArgs(append([]string{"runner", "setup", "--url", r.srv.URL}, args...))
	err := root.Execute()
	return out.String(), err
}

func (r *setupRig) record(name string) string {
	b, _ := os.ReadFile(filepath.Join(r.rec, name))
	return string(b)
}

// filesContaining lists the files under dir whose bytes contain needle.
func filesContaining(t *testing.T, dir, needle string) []string {
	t.Helper()
	var hits []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(b), needle) {
				hits = append(hits, p)
			}
		}
		return nil
	})
	return hits
}

func TestRunnerSetupTokenGoesOnlyOverThePipe(t *testing.T) {
	r := newSetupRig(t)
	bin, sum := r.fakeRunnerd(r.binDir, 0)
	pinnedRunnerdSHA256 = sum
	t.Setenv("WARDYN_TOKEN", "wdn_env_personal")
	t.Setenv("WARDYN_ADMIN_TOKEN", "wda_env_admin")
	out, err := r.run("--token", "wdn_flag_personal", "--runnerd", bin, "--name", "laptop")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	argv := r.record("argv")
	if !strings.Contains(argv, "--token-file\n/dev/fd/3\n") || !strings.Contains(argv, "--org\n"+r.srv.URL+"\n") || !strings.Contains(argv, "--name\nlaptop\n") {
		t.Fatalf("runnerd argv:\n%s", argv)
	}
	if got := strings.TrimSpace(r.record("token")); got != setupTestToken {
		t.Fatalf("runnerd read %q from fd 3, want the minted token", got)
	}
	for _, where := range []string{argv, r.record("env"), out} {
		if strings.Contains(where, "wdr_") {
			t.Fatalf("the registration token is in argv, the environment or the output:\n%s", where)
		}
	}
	for _, name := range []string{"WARDYN_TOKEN=", "WARDYN_ADMIN_TOKEN="} {
		if strings.Contains(r.record("env"), name) {
			t.Fatalf("runnerd's environment carries %s", name)
		}
	}
	if hits := filesContaining(t, r.home, "wdr_"); len(hits) != 0 {
		t.Fatalf("the registration token was written under HOME: %v", hits)
	}
	if hits := filesContaining(t, r.tmp, setupTestToken); len(hits) != 0 {
		t.Fatalf("the registration token was written under the temp dir: %v", hits)
	}
	if r.mints != 1 || len(r.called("POST "+"/api/v1/me/runners/"+r.runnerID.String()+"/claim")) != 1 {
		t.Fatalf("mints=%d calls=%v", r.mints, r.called(""))
	}
	for _, want := range []string{fmt.Sprintf(copySetupClaimed, r.runnerID, "fp-1"), fmt.Sprintf(copySetupConnected, r.runnerID)} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestRunnerSetupRerunMintsNothing(t *testing.T) {
	r := newSetupRig(t)
	bin, sum := r.fakeRunnerd(r.binDir, 0)
	pinnedRunnerdSHA256 = sum
	if err := os.MkdirAll(r.stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf(`{"runner_id":%q,"key_fingerprint":"fp-1","org_url_sha256":%q,"proxy_image":"x"}`, r.runnerID, federation.OrgURLSHA256(r.srv.URL))
	if err := os.WriteFile(filepath.Join(r.stateDir(), "runner.json"), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
	r.claimed = true
	out, err := r.run("--token", "wdn_personal", "--runnerd", bin)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if r.mints != 0 || strings.Contains(r.record("argv"), "--token-file") || len(r.called("POST /api/v1/me/runners/"+r.runnerID.String()+"/claim")) != 0 {
		t.Fatalf("a registered host minted, passed a token or claimed again: mints=%d argv=%q calls=%v", r.mints, r.record("argv"), r.called(""))
	}
	if !strings.Contains(out, fmt.Sprintf(copySetupRegistered, r.srv.URL)) || !strings.Contains(r.record("argv"), "setup\n--org\n") {
		t.Fatalf("output:\n%s\nargv:\n%s", out, r.record("argv"))
	}
}

func TestRunnerSetupRefusesARunnerdWithAnotherHash(t *testing.T) {
	r := newSetupRig(t)
	bin, sum := r.fakeRunnerd(r.binDir, 0)
	pinnedRunnerdSHA256 = strings.Repeat("0", 64)
	out, err := r.run("--token", "wdn_personal", "--runnerd", bin)
	if err == nil || !strings.Contains(err.Error(), sum) || !strings.Contains(err.Error(), pinnedRunnerdSHA256) || !strings.Contains(err.Error(), "--runnerd") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if r.record("argv") != "" || r.mints != 0 {
		t.Fatalf("a refused runnerd ran (%q) or a token was minted (%d)", r.record("argv"), r.mints)
	}
}

func TestRunnerSetupFoundRunnerdIsHashCheckedUnlessThePackagesOwn(t *testing.T) {
	r := newSetupRig(t)
	cliDir := t.TempDir()
	setupExecutable = func() (string, error) { return filepath.Join(cliDir, "wardyn"), nil }
	_, sum := r.fakeRunnerd(cliDir, 0)
	pinnedRunnerdSHA256 = strings.Repeat("1", 64)
	if _, err := r.run("--token", "wdn_personal"); err == nil || !strings.Contains(err.Error(), sum) || r.record("argv") != "" {
		t.Fatalf("a runnerd beside the CLI with another hash: err = %v, ran %q", err, r.record("argv"))
	}

	// The package's own copy is run without the hash only while it and its directory are root-owned and
	// not writable by others; the test seam stands in for the path and the owner.
	resolved, err := filepath.EvalSymlinks(filepath.Join(cliDir, "wardyn-runnerd"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cliDir, 0o755); err != nil {
		t.Fatal(err)
	}
	packageRunnerd = resolved
	packageOwner = func(fs.FileInfo) int { return 1000 }
	if _, err := r.run("--token", "wdn_personal"); err == nil || r.record("argv") != "" {
		t.Fatalf("a package path not owned by root ran unchecked: err = %v", err)
	}
	packageOwner = func(fs.FileInfo) int { return 0 }
	if out, err := r.run("--token", "wdn_personal"); err != nil || !strings.Contains(out, fmt.Sprintf(copySetupPackage, resolved)) {
		t.Fatalf("the root-owned package runnerd: err = %v\n%s", err, out)
	}
}

func TestRunnerSetupRefusesAnotherOrganisationsRunner(t *testing.T) {
	r := newSetupRig(t)
	bin, sum := r.fakeRunnerd(r.binDir, 0)
	pinnedRunnerdSHA256 = sum
	other := "https://other.example.com"
	if err := os.MkdirAll(r.stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf(`{"runner_id":%q,"key_fingerprint":"fp-1","org_url_sha256":%q}`, uuid.New(), federation.OrgURLSHA256(other))
	if err := os.WriteFile(filepath.Join(r.stateDir(), "runner.json"), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.home, ".config", "wardyn", "runnerd.env"), []byte("WARDYN_RUNNER_ORG_URL="+other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := r.run("--token", "wdn_personal", "--runnerd", bin)
	if err == nil || err.Error() != fmt.Sprintf(errSetupOtherOrg, other, r.srv.URL, other, r.stateDir()) {
		t.Fatalf("err = %v", err)
	}
	if r.mints != 0 || r.record("argv") != "" {
		t.Fatal("another organisation's runner went on to mint or run runnerd")
	}
}

func TestRunnerSetupRefusesWindowsAndTheAdminToken(t *testing.T) {
	r := newSetupRig(t)
	setupGOOS = "windows"
	if _, err := r.run("--token", "wdn_personal"); err == nil || err.Error() != errSetupWindows {
		t.Fatalf("windows: err = %v", err)
	}
	setupGOOS = "linux"
	t.Setenv("WARDYN_ADMIN_TOKEN", "wda_admin")
	if _, err := r.run(); err == nil || err.Error() != errSetupAdminToken {
		t.Fatalf("admin token: err = %v", err)
	}
	if calls := r.called(""); len(calls) != 0 {
		t.Fatalf("a refusal still called the organisation: %v", calls)
	}
}

func TestRunnerSetupDownloadBaseOrder(t *testing.T) {
	cases := []struct {
		name, flag, saved, advertised, want string
	}{
		{name: "flag", flag: "/flag", saved: "/saved", advertised: "/adv", want: "/flag/v0.10.0/wardyn-runnerd-linux-amd64"},
		{name: "saved", saved: "/saved", advertised: "/adv", want: "/saved/v0.10.0/wardyn-runnerd-linux-amd64"},
		{name: "advertised", advertised: "/adv", want: "/adv/v0.10.0/wardyn-runnerd-linux-amd64"},
		{name: "release, through a redirect", want: "/objects/runnerd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newSetupRig(t)
			script, sum := r.fakeRunnerd(r.binDir, 0)
			body, err := os.ReadFile(script)
			if err != nil {
				t.Fatal(err)
			}
			pinnedRunnerdSHA256 = sum
			r.assets[tc.want] = body
			abs := func(p string) string {
				if p == "" {
					return ""
				}
				return r.srv.URL + p
			}
			r.advertised = abs(tc.advertised)
			dir := filepath.Join(r.home, ".config", "wardyn")
			if tc.saved != "" {
				if err := cliprofile.WriteArtifactBase(dir, abs(tc.saved)); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"--token", "wdn_personal"}
			if tc.flag != "" {
				args = append(args, "--artifact-base", abs(tc.flag))
			}
			out, err := r.run(args...)
			if err != nil {
				t.Fatalf("setup: %v\n%s", err, out)
			}
			if got := r.called("GET " + tc.want); len(got) != 1 {
				t.Fatalf("fetched %v, want %s", r.called("GET /"), tc.want)
			}
			if tc.flag != "" {
				if saved, _ := cliprofile.ReadArtifactBase(dir); saved != abs(tc.flag) {
					t.Fatalf("--artifact-base not saved: %q", saved)
				}
			}
		})
	}
}

func TestRunnerSetupSelfHostRefusalIsTheServersLine(t *testing.T) {
	r := newSetupRig(t)
	bin, sum := r.fakeRunnerd(r.binDir, 0)
	pinnedRunnerdSHA256 = sum
	const serverLine = "hosting your own runner isn't available to you. Ask your admin for the self_host_runner permission."
	r.mintStatus, r.mintBody = http.StatusForbidden, `{"error":"`+serverLine+`","reason":"capability_feature"}`
	_, err := r.run("--token", "wdn_personal", "--runnerd", bin)
	if err == nil || err.Error() != serverLine+"\n"+copySetupStepUp {
		t.Fatalf("err = %q", err)
	}
	if r.mints != 1 || r.record("argv") != "" {
		t.Fatalf("after the refusal: mints=%d, runnerd ran %q", r.mints, r.record("argv"))
	}
}

func TestRunnerSetupNotConnectedNamesTheRestoreLine(t *testing.T) {
	r := newSetupRig(t)
	bin, sum := r.fakeRunnerd(r.binDir, 0)
	pinnedRunnerdSHA256 = sum
	if err := os.MkdirAll(r.stateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf(`{"runner_id":%q,"key_fingerprint":"fp-1","org_url_sha256":%q}`, r.runnerID, federation.OrgURLSHA256(r.srv.URL))
	if err := os.WriteFile(filepath.Join(r.stateDir(), "runner.json"), []byte(id), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.stateDir(), "status.json"), []byte(`{"state":"offline","last_error":"WARDYN_ORG_CA_FILE: not a PEM file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(r.home, ".local", "share", "wardyn", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	prev := filepath.Join(binDir, "wardyn-runnerd.previous")
	if err := os.WriteFile(prev, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.claimed, r.online, r.version = true, false, "0.9.6"
	_, err := r.run("--token", "wdn_personal", "--runnerd", bin)
	want := fmt.Sprintf(errSetupNotConnected, setupConnectWait) + "\n" + fmt.Sprintf(copySetupLastError, "WARDYN_ORG_CA_FILE: not a PEM file") +
		"\n" + fmt.Sprintf(copySetupRestore, "0.9.6", prev, r.srv.URL)
	if err == nil || err.Error() != want {
		t.Fatalf("err = %q\nwant  %q", err, want)
	}
}

func TestRunnerSetupFailuresLeaveNoHalfClaimedRunner(t *testing.T) {
	r := newSetupRig(t)
	bin, sum := r.fakeRunnerd(r.binDir, 3)
	pinnedRunnerdSHA256 = sum
	_, err := r.run("--token", "wdn_personal", "--runnerd", bin)
	if err == nil || !strings.HasPrefix(err.Error(), "wardyn-runnerd setup did not finish (exit status 3)") {
		t.Fatalf("err = %v", err)
	}
	if _, serr := os.Stat(filepath.Join(r.stateDir(), "runner.json")); serr == nil || len(r.called("POST /api/v1/me/runners/"+r.runnerID.String()+"/claim")) != 0 {
		t.Fatal("a failed runnerd setup left an identity or was claimed")
	}

	// The claim refused: the identity stays, and the error says a rerun finishes it.
	r2 := newSetupRig(t)
	bin2, sum2 := r2.fakeRunnerd(r2.binDir, 0)
	pinnedRunnerdSHA256 = sum2
	r2.claimStatus = http.StatusForbidden
	if _, err := r2.run("--token", "wdn_personal", "--runnerd", bin2); err == nil || !strings.HasPrefix(err.Error(), "this computer is registered but not claimed:") {
		t.Fatalf("claim refused: err = %v", err)
	}
}

func TestRunnerSetupSignsInThroughTheBrowserWithNoCredential(t *testing.T) {
	r := newSetupRig(t)
	f := newSignInOrg(t)
	keyring.MockInit()
	prevReady, prevOpen, prevCan := keychainReady, openBrowser, canOpenBrowser
	t.Cleanup(func() { keychainReady, openBrowser, canOpenBrowser = prevReady, prevOpen, prevCan })
	keychainReady = func() error { return nil }
	canOpenBrowser = func() bool { return true }
	browser := approveInBrowser(t, f, nil)
	openBrowser = func(approve string) error { go browser(approve); return nil }
	// The fake answers on this rig's origin, the one the device proof is signed over.
	r.signIn, r.fallback, f.srv.URL = true, f.srv.Config.Handler, r.srv.URL
	bin, sum := r.fakeRunnerd(r.binDir, 0)
	pinnedRunnerdSHA256 = sum
	out, err := r.run("--runnerd", bin)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	f.mu.Lock()
	_, issued := f.tokens[r.mintBearer]
	f.mu.Unlock()
	if !issued {
		t.Fatalf("the mint did not carry the browser sign-in's token (bearer %q)", r.mintBearer)
	}
	if strings.Contains(r.record("env"), r.mintBearer) || strings.Contains(r.record("argv"), r.mintBearer) {
		t.Fatal("the CLI sign-in's token reached wardyn-runnerd")
	}
	if got, _ := cliprofile.Read(filepath.Join(r.home, ".config", "wardyn")); got != r.srv.URL {
		t.Fatalf("profile URL = %q, want %q", got, r.srv.URL)
	}

	// With no sign-in on offer, the command names the token route instead.
	r2 := newSetupRig(t)
	bin2, sum2 := r2.fakeRunnerd(r2.binDir, 0)
	pinnedRunnerdSHA256 = sum2
	if _, err := r2.run("--runnerd", bin2); err == nil || err.Error() != fmt.Sprintf(errSetupNoCredential, noSignInLine) {
		t.Fatalf("no sign-in on offer: err = %v", err)
	}
}
