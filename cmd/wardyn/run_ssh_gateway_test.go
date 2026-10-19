// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	gatewayOnHealthz  = `{"ssh":{"enabled":true,"advertise_addr":"gw.example.com:2222","host_key_fingerprint":"SHA256:x"}}`
	gatewayOffHealthz = `{"ssh":null}`
	oneKey            = `[{"id":"k1","name":"laptop","fingerprint":"SHA256:abc","role":"user"}]`
)

// gatewayFixture is a fake deployment: /healthz, the caller's registered keys,
// and a count of every other request (a key must never be created).
type gatewayFixture struct {
	url    string
	others atomic.Int32 // requests other than GET /healthz and GET /api/v1/me/ssh-keys
	keyGET atomic.Int32
}

func newGatewayFixture(t *testing.T, healthz, keys string) *gatewayFixture {
	t.Helper()
	f := &gatewayFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /healthz":
			_, _ = w.Write([]byte(healthz))
		case "GET /api/v1/me/ssh-keys":
			f.keyGET.Add(1)
			_, _ = w.Write([]byte(keys))
		default:
			f.others.Add(1)
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// fakeSSH puts a recording ssh(1) on PATH and returns the file it writes its
// arguments to, one per line; the file does not exist if ssh never ran.
func fakeSSH(t *testing.T, exit string) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "ssh-args")
	fakeBinOnPath(t, "ssh", "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > "+log+"\nexit "+exit+"\n")
	return log
}

func readArgs(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("ssh was not run: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func neverRan(t *testing.T, log string) {
	t.Helper()
	if _, err := os.Stat(log); err == nil {
		t.Errorf("ssh ran, want it never started")
	}
}

// homeWithKey points HOME at a temp dir, holding ~/.wardyn/id_ed25519 when withKey.
func homeWithKey(t *testing.T, withKey bool) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if withKey {
		if err := os.MkdirAll(filepath.Join(home, ".wardyn"), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(home, ".wardyn", "id_ed25519"), "private")
	}
	return home
}

func runGateway(t *testing.T, mk func(clientFn) *cobra.Command, f *gatewayFixture, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := mk(func() *sdk.Client { return &sdk.Client{BaseURL: f.url} })
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetContext(context.Background())
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	err := cmd.Execute()
	return errOut.String(), err
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("ssh argv =\n  %q\nwant\n  %q", got, want)
	}
}

// --------------------------------------------------------------------------
// exec
// --------------------------------------------------------------------------

func TestRunExec_SSHArgvKeepsEachWordAsOneArgument(t *testing.T) {
	homeWithKey(t, false)
	log := fakeSSH(t, "0")
	f := newGatewayFixture(t, gatewayOnHealthz, oneKey)
	if _, err := runGateway(t, runExecCmd, f, "", testRunID, "--", "sh", "-c", "echo 'a b'; ls -la", "plain-word_1.txt"); err != nil {
		t.Fatal(err)
	}
	eq(t, readArgs(t, log), []string{"-T", testRunID + "@gw.example.com", "-p", "2222", "--",
		`sh -c 'echo '\''a b'\''; ls -la' plain-word_1.txt`})
}

func TestRunExec_UsesTheCLIKeyWhenItExists(t *testing.T) {
	home := homeWithKey(t, true)
	log := fakeSSH(t, "0")
	f := newGatewayFixture(t, gatewayOnHealthz, oneKey)
	if _, err := runGateway(t, runExecCmd, f, "", testRunID, "--", "true"); err != nil {
		t.Fatal(err)
	}
	eq(t, readArgs(t, log), []string{"-T", "-i", filepath.Join(home, ".wardyn", "id_ed25519"), "-o", "IdentitiesOnly=yes",
		testRunID + "@gw.example.com", "-p", "2222", "--", "true"})
}

func TestRunExec_PassesTheRemoteExitStatusThrough(t *testing.T) {
	homeWithKey(t, false)
	fakeSSH(t, "7")
	f := newGatewayFixture(t, gatewayOnHealthz, oneKey)
	_, err := runGateway(t, runExecCmd, f, "", testRunID, "--", "false")
	if code := exitCodeFor(err); code != 7 {
		t.Errorf("exit code = %d (%v), want ssh's own 7", code, err)
	}
}

func TestRunExec_NeedsTheDashAndACommand(t *testing.T) {
	homeWithKey(t, false)
	log := fakeSSH(t, "0")
	f := newGatewayFixture(t, gatewayOnHealthz, oneKey)
	for _, args := range [][]string{{testRunID}, {testRunID, "ls"}, {testRunID, "--"}, {"--", "ls"}} {
		if _, err := runGateway(t, runExecCmd, f, "", args...); err == nil || !strings.Contains(err.Error(), "usage: wardyn run exec") {
			t.Errorf("args %q: err = %v, want the usage line", args, err)
		}
	}
	neverRan(t, log)
}

// --------------------------------------------------------------------------
// the shared refusals, each against all three commands
// --------------------------------------------------------------------------

var gatewayCommands = []struct {
	name string
	mk   func(clientFn) *cobra.Command
	args []string
}{
	{"exec", runExecCmd, []string{testRunID, "--", "true"}},
	{"forward", runForwardCmd, []string{testRunID, "3000"}},
	{"editor", runEditorCmd, []string{testRunID}},
}

func TestGatewayCommands_GatewayOffIsOneLineAndTouchesNothing(t *testing.T) {
	for _, tc := range gatewayCommands {
		t.Run(tc.name, func(t *testing.T) {
			home := homeWithKey(t, false)
			log := fakeSSH(t, "0")
			f := newGatewayFixture(t, gatewayOffHealthz, oneKey)
			_, err := runGateway(t, tc.mk, f, "y\n", tc.args...)
			if err == nil {
				t.Fatal("want a refusal when the operator has not turned SSH on")
			}
			want := tc.name + ": the operator has not turned SSH on for this deployment, so there is no gateway to reach (WARDYN_SSH_LISTEN)"
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
			neverRan(t, log)
			if f.keyGET.Load() != 0 {
				t.Errorf("read the account's keys before knowing the gateway is on")
			}
			if _, serr := os.Stat(filepath.Join(home, ".wardyn")); serr == nil {
				t.Errorf("wrote under ~/.wardyn although there is no gateway")
			}
		})
	}
}

func TestGatewayCommands_NoKeyOnTheAccountNamesSSHKeyEnsureAndCreatesNothing(t *testing.T) {
	for _, tc := range gatewayCommands {
		t.Run(tc.name, func(t *testing.T) {
			home := homeWithKey(t, false)
			log := fakeSSH(t, "0")
			f := newGatewayFixture(t, gatewayOnHealthz, `[]`)
			_, err := runGateway(t, tc.mk, f, "y\n", tc.args...)
			if err == nil {
				t.Fatal("want a refusal when the account has no SSH key")
			}
			want := tc.name + ": no SSH key is registered for your account: run `wardyn ssh-key ensure` once, then retry"
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err, want)
			}
			neverRan(t, log)
			if f.others.Load() != 0 {
				t.Errorf("sent %d request(s) beyond /healthz and the key list: a key must never be registered here", f.others.Load())
			}
			for _, p := range []string{".wardyn", ".ssh"} {
				if _, serr := os.Stat(filepath.Join(home, p)); serr == nil {
					t.Errorf("created ~/%s: no key, and no file, may be made on the user's behalf", p)
				}
			}
		})
	}
}

func TestGatewayCommands_AdvertisedProxyNeedsConsent(t *testing.T) {
	for _, tc := range gatewayCommands {
		t.Run(tc.name, func(t *testing.T) {
			homeWithKey(t, false)
			log := fakeSSH(t, "0")
			f := newGatewayFixture(t, proxyHealthz, oneKey)
			_, err := runGateway(t, tc.mk, f, "y\n", tc.args...)
			if err == nil || !strings.Contains(err.Error(), "--advertised-proxy") || !strings.Contains(err.Error(), testProxyCommand) {
				t.Errorf("error = %v, want the ProxyCommand shown with the consent flag", err)
			}
			neverRan(t, log)
		})
	}
}

func TestGatewayCommands_RefuseABadRunIDBeforeAnyRequest(t *testing.T) {
	for _, tc := range gatewayCommands {
		t.Run(tc.name, func(t *testing.T) {
			homeWithKey(t, false)
			log := fakeSSH(t, "0")
			f := newGatewayFixture(t, gatewayOnHealthz, oneKey)
			args := append([]string{"not-a-uuid"}, tc.args[1:]...)
			_, err := runGateway(t, tc.mk, f, "", args...)
			if err == nil || !strings.Contains(err.Error(), "invalid run id") {
				t.Errorf("error = %v, want invalid run id", err)
			}
			neverRan(t, log)
		})
	}
}

// --------------------------------------------------------------------------
// run ssh -i, and --print / --config unchanged
// --------------------------------------------------------------------------

func TestRunSSH_OffersTheCLIKeyOnlyWhenItExists(t *testing.T) {
	for _, withKey := range []bool{false, true} {
		home := homeWithKey(t, withKey)
		log := fakeSSH(t, "0")
		f := newGatewayFixture(t, gatewayOnHealthz, oneKey)
		if _, err := runGateway(t, sshCmd, f, "", testRunID); err != nil {
			t.Fatal(err)
		}
		want := []string{testRunID + "@gw.example.com", "-p", "2222"}
		if withKey {
			want = append([]string{"-i", filepath.Join(home, ".wardyn", "id_ed25519"), "-o", "IdentitiesOnly=yes"}, want...)
		}
		eq(t, readArgs(t, log), want)
	}
}

func TestRunSSH_PrintAndConfigNeverCarryTheKey(t *testing.T) {
	homeWithKey(t, true)
	f := newGatewayFixture(t, gatewayOnHealthz, oneKey)
	cmd := sshCmd(func() *sdk.Client { return &sdk.Client{BaseURL: f.url} })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{testRunID, "--print"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if want := "ssh " + testRunID + "@gw.example.com -p 2222\n"; out.String() != want {
		t.Errorf("--print = %q, want %q (byte-identical to the console card)", out.String(), want)
	}
	out.Reset()
	cmd = sshCmd(func() *sdk.Client { return &sdk.Client{BaseURL: f.url} })
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{testRunID, "--config"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if want := "Host wardyn-a1b2c3d4\n  HostName gw.example.com\n  Port 2222\n  User " + testRunID + "\n"; out.String() != want {
		t.Errorf("--config = %q, want %q", out.String(), want)
	}
}

// --------------------------------------------------------------------------
// forward
// --------------------------------------------------------------------------

func TestRunForward_SSHArgv(t *testing.T) {
	home := homeWithKey(t, true)
	log := fakeSSH(t, "0")
	f := newGatewayFixture(t, gatewayOnHealthz, oneKey)
	if _, err := runGateway(t, runForwardCmd, f, "", testRunID, "3000", "8080:4000"); err != nil {
		t.Fatal(err)
	}
	eq(t, readArgs(t, log), []string{"-N", "-o", "ExitOnForwardFailure=yes",
		"-L", "127.0.0.1:3000:127.0.0.1:3000", "-L", "127.0.0.1:8080:127.0.0.1:4000",
		"-i", filepath.Join(home, ".wardyn", "id_ed25519"), "-o", "IdentitiesOnly=yes",
		testRunID + "@gw.example.com", "-p", "2222"})
}

func TestRunForward_RefusesABadPortBeforeAnyRequest(t *testing.T) {
	homeWithKey(t, false)
	log := fakeSSH(t, "0")
	f := newGatewayFixture(t, gatewayOnHealthz, oneKey)
	for _, spec := range []string{"0", "65536", "web", "1:2:3", "80:", ":80", "0x50", "+80"} {
		if _, err := runGateway(t, runForwardCmd, f, "", testRunID, spec); err == nil || !strings.Contains(err.Error(), "forward: "+`"`+spec+`"`) {
			t.Errorf("spec %q: err = %v, want the port refusal", spec, err)
		}
	}
	neverRan(t, log)
}

// --------------------------------------------------------------------------
// editor
// --------------------------------------------------------------------------

func fakeCode(t *testing.T) string {
	t.Helper()
	log := filepath.Join(t.TempDir(), "code-args")
	fakeBinOnPath(t, "code", "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > "+log+"\n")
	return log
}

func TestRunEditor_WritesTheHostBlockAsksThenOpensCode(t *testing.T) {
	home := homeWithKey(t, true)
	codeLog := fakeCode(t)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sshDir, "config"), "Host other\n  User me\n")
	f := newGatewayFixture(t, gatewayOnHealthz, oneKey)

	prompt, err := runGateway(t, runEditorCmd, f, "y\n", testRunID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, `Include ~/.wardyn/ssh_config`) || !strings.Contains(prompt, "[y/N]") {
		t.Errorf("prompt = %q, want the Include line and a y/N question", prompt)
	}
	cfg, _ := os.ReadFile(filepath.Join(home, ".wardyn", "ssh_config"))
	wantCfg := "Host wardyn-a1b2c3d4\n  HostName gw.example.com\n  Port 2222\n  User " + testRunID +
		"\n  IdentityFile ~/.wardyn/id_ed25519\n  IdentitiesOnly yes\n"
	if string(cfg) != wantCfg {
		t.Errorf("~/.wardyn/ssh_config =\n%q\nwant\n%q", cfg, wantCfg)
	}
	sshCfg, _ := os.ReadFile(filepath.Join(sshDir, "config"))
	if want := "Include ~/.wardyn/ssh_config\n\nHost other\n  User me\n"; string(sshCfg) != want {
		t.Errorf("~/.ssh/config = %q, want the Include line first and the old content kept: %q", sshCfg, want)
	}
	eq(t, readArgs(t, codeLog), []string{"--remote", "ssh-remote+wardyn-a1b2c3d4", "/home/agent/work"})

	// A second run finds the Include and does not ask again.
	prompt, err = runGateway(t, runEditorCmd, f, "", testRunID, "--dir", "/home/agent/app")
	if err != nil || prompt != "" {
		t.Errorf("second run: err = %v, prompt = %q, want no prompt", err, prompt)
	}
	eq(t, readArgs(t, codeLog), []string{"--remote", "ssh-remote+wardyn-a1b2c3d4", "/home/agent/app"})
}

func TestRunEditor_DecliningLeavesSSHConfigAloneAndOpensNothing(t *testing.T) {
	for _, answer := range []string{"n\n", "\n", ""} {
		home := homeWithKey(t, false)
		codeLog := fakeCode(t)
		f := newGatewayFixture(t, gatewayOnHealthz, oneKey)
		out, err := runGateway(t, runEditorCmd, f, answer, testRunID)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "Include ~/.wardyn/ssh_config") || !strings.Contains(out, "Not changed") {
			t.Errorf("answer %q: output = %q, want the line to add by hand", answer, out)
		}
		if _, serr := os.Stat(filepath.Join(home, ".ssh", "config")); serr == nil {
			t.Errorf("answer %q: ~/.ssh/config was created without consent", answer)
		}
		neverRan(t, codeLog)
		if _, serr := os.Stat(filepath.Join(home, ".wardyn", "ssh_config")); serr != nil {
			t.Errorf("answer %q: the Wardyn-owned file should still be written: %v", answer, serr)
		}
	}
}

func TestUpsertSSHHost_ReplacesOneHostAndKeepsTheRest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "d", "ssh_config")
	for _, b := range []string{"Host wardyn-a\n  Port 1\n", "Host wardyn-b\n  Port 2\n", "Host wardyn-a\n  Port 3\n"} {
		if err := upsertSSHHost(p, b); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile(p)
	if want := "Host wardyn-b\n  Port 2\n\nHost wardyn-a\n  Port 3\n"; string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestRunEditor_RefusesARelativeDir(t *testing.T) {
	homeWithKey(t, false)
	f := newGatewayFixture(t, gatewayOnHealthz, oneKey)
	if _, err := runGateway(t, runEditorCmd, f, "y\n", testRunID, "--dir", "work"); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("err = %v, want the absolute-path refusal", err)
	}
}
