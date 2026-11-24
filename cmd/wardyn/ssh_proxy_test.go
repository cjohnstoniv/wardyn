// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// The ticket path: `run ssh-proxy`, and `run ssh`, `exec`, `forward` and `editor` with SSH tickets on.

const doorBanner = "SSH-2.0-door\r\n"

// testHostKey is a gateway host key: its authorized_keys line and its fingerprint.
type testHostKey struct {
	key         ssh.PublicKey
	line, print string
}

func newTestHostKey(t *testing.T) testHostKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return testHostKey{key: k, line: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))), print: ssh.FingerprintSHA256(k)}
}

// ticketHealthz is /healthz with the gateway on, tickets on or not, and extra fields appended to "ssh".
func ticketHealthz(hk testHostKey, tickets bool, extra string) string {
	t := ""
	if tickets {
		t = `,"tickets":true,"host_key":"` + hk.line + `"`
	}
	return `{"ssh":{"enabled":true,"advertise_addr":"gw.example.com:2222","host_key_fingerprint":"` + hk.print + `"` + t + extra + `}}`
}

// ticketFixture is a deployment: /healthz, the key listing, the mint and the SSH ticket door, each counted.
type ticketFixture struct {
	url                   string
	keyGET, mints, dials  atomic.Int32
	mu                    sync.Mutex
	mintQ                 []url.Values
	mintAuth              []string
	doorTicket, doorQuery []string
	doorAuth, doorEnv     []string
	mint                  func(w http.ResponseWriter, q url.Values)
	door                  func(w http.ResponseWriter, r *http.Request)
}

func newTicketFixture(t *testing.T, healthz string) *ticketFixture {
	t.Helper()
	f := &ticketFixture{}
	f.mint = func(w http.ResponseWriter, q url.Values) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ticket":"tkt-%d","expires_in_seconds":30,"purpose":%q}`, f.mints.Load(), q.Get("purpose"))
	}
	f.door = func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		nc := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
		_, _ = nc.Write([]byte(doorBanner))
		_ = nc.Close()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(healthz)) })
	mux.HandleFunc("/api/v1/me/ssh-keys", func(w http.ResponseWriter, _ *http.Request) {
		f.keyGET.Add(1)
		_, _ = w.Write([]byte(oneKey))
	})
	mux.HandleFunc("POST /api/v1/runs/"+testRunID+"/attach/ticket", func(w http.ResponseWriter, r *http.Request) {
		f.mints.Add(1)
		f.mu.Lock()
		f.mintQ = append(f.mintQ, r.URL.Query())
		f.mintAuth = append(f.mintAuth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		f.mint(w, r.URL.Query())
	})
	mux.HandleFunc("/api/v1/runs/"+testRunID+"/ssh", func(w http.ResponseWriter, r *http.Request) {
		f.dials.Add(1)
		f.mu.Lock()
		f.doorTicket = append(f.doorTicket, r.Header.Get("Wardyn-Ticket"))
		f.doorQuery = append(f.doorQuery, r.URL.RawQuery)
		f.doorAuth = append(f.doorAuth, r.Header.Get("Authorization"))
		f.doorEnv = append(f.doorEnv, os.Getenv(sshTicketEnv))
		f.mu.Unlock()
		f.door(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.url = srv.URL
	return f
}

// refuseMint makes every mint answer status with the server's envelope.
func (f *ticketFixture) refuseMint(status int, body string) {
	f.mint = func(w http.ResponseWriter, _ url.Values) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// sshChild is one ssh(1) child the command would have run, and the parent's own WARDYN_SSH_TICKET when it started.
type sshChild struct {
	cmd       *exec.Cmd
	parentEnv string
}

// captureSSH replaces the ssh(1) child with a recorder; ssh never runs.
func captureSSH(t *testing.T) *[]sshChild {
	t.Helper()
	var got []sshChild
	prev := runSSHChild
	runSSHChild = func(c *exec.Cmd) error {
		got = append(got, sshChild{cmd: c, parentEnv: os.Getenv(sshTicketEnv)})
		return nil
	}
	t.Cleanup(func() { runSSHChild = prev })
	return &got
}

// runTicketCmd runs mk's command against base with token and stdin, and returns its stdout and stderr.
func runTicketCmd(t *testing.T, mk func(clientFn) *cobra.Command, base, token string, stdin io.Reader, args ...string) (string, string, error) {
	t.Helper()
	cmd := mk(func() *sdk.Client { return &sdk.Client{BaseURL: base, Token: token} })
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetIn(stdin)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd.SetContext(ctx)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// openStdin is a stdin that stays open until the test ends, so the tunnel ends from the door's side.
func openStdin(t *testing.T) io.Reader {
	t.Helper()
	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	return r
}

// verbCase runs one ticket-path command against f: exec, forward and ssh in the parent, editor through the proxy's
// own mint (VS Code starts it with no ticket handed over).
type verbCase struct {
	verb string
	mk   func(clientFn) *cobra.Command
	args []string
}

var ticketVerbs = []verbCase{
	{"exec", runExecCmd, []string{testRunID, "--", "echo", "hi there"}},
	{"forward", runForwardCmd, []string{testRunID, "8080:3000", "5000"}},
	{"ssh", sshCmd, []string{testRunID}},
	{"editor", sshProxyCmd, []string{testRunID, "--for", "editor"}},
}

func (vc verbCase) run(t *testing.T, f *ticketFixture) (string, error) {
	t.Helper()
	t.Setenv(sshTicketEnv, "")
	_, stderr, err := runTicketCmd(t, vc.mk, f.url, "bearer-x", openStdin(t), vc.args...)
	return stderr, err
}

func TestSSHProxy_TicketRidesTheHeaderAndNeverTheURL(t *testing.T) {
	homeWithKey(t, false)
	f := newTicketFixture(t, "")
	t.Setenv(sshTicketEnv, "handed-over")
	stdout, _, err := runTicketCmd(t, sshProxyCmd, f.url, "bearer-x", openStdin(t), testRunID, "--for", "exec")
	if err != nil {
		t.Fatalf("ssh-proxy: %v", err)
	}
	if stdout != doorBanner {
		t.Errorf("stdout = %q, want only the tunnel's bytes %q", stdout, doorBanner)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.doorTicket) != 1 || f.doorTicket[0] != "handed-over" || f.doorQuery[0] != "" || f.doorAuth[0] != "" {
		t.Errorf("door saw ticket %q query %q auth %q, want the handed-over ticket in the header, no query, no bearer",
			f.doorTicket, f.doorQuery, f.doorAuth)
	}
	if f.doorEnv[0] != "" || os.Getenv(sshTicketEnv) != "" {
		t.Errorf("%s = %q while the door answered, %q after; want it unset before the dial", sshTicketEnv, f.doorEnv[0], os.Getenv(sshTicketEnv))
	}
	if f.mints.Load() != 0 {
		t.Errorf("mints = %d, want none with a ticket handed over", f.mints.Load())
	}
}

func TestSSHProxy_DoorRefusalPrintsTheServersSentence(t *testing.T) {
	f := newTicketFixture(t, "")
	f.door = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"Invalid, expired, or already-used attach ticket","reason":"ui_gateway_ticket_invalid"}`))
	}
	t.Setenv(sshTicketEnv, "spent")
	stdout, _, err := runTicketCmd(t, sshProxyCmd, f.url, "", openStdin(t), testRunID, "--for", "exec")
	if err == nil || err.Error() != "ssh-proxy: Invalid, expired, or already-used attach ticket" {
		t.Fatalf("error = %v, want the door's sentence after ssh-proxy:", err)
	}
	if code := exitCodeFor(err); code != 2 || stdout != "" {
		t.Errorf("exit %d, stdout %q; want 2 (a 403) and nothing on stdout", code, stdout)
	}
}

func TestSSHProxy_MintsForEachConnectionWithTheSignIn(t *testing.T) {
	home := homeWithKey(t, false)
	hk := newTestHostKey(t)
	f := newTicketFixture(t, ticketHealthz(hk, true, ""))
	t.Setenv(sshTicketEnv, "")
	stdout, _, err := runTicketCmd(t, sshProxyCmd, f.url, "bearer-x", openStdin(t), testRunID, "--for", "editor")
	if err != nil || stdout != doorBanner {
		t.Fatalf("ssh-proxy --for editor: %v, stdout %q", err, stdout)
	}
	f.mu.Lock()
	if len(f.mintQ) != 1 || f.mintQ[0].Encode() != "purpose=ssh-editor" || f.mintAuth[0] != "Bearer bearer-x" || f.doorTicket[0] != "tkt-1" {
		t.Errorf("mint %v (auth %q), door ticket %q; want one ssh-editor mint with the sign-in, its ticket at the door", f.mintQ, f.mintAuth, f.doorTicket)
	}
	f.mu.Unlock()
	kh, _ := os.ReadFile(filepath.Join(home, ".wardyn", "known_hosts"))
	if want := gatewayHostKeyAlias(f.url) + " " + string(ssh.MarshalAuthorizedKey(hk.key)); string(kh) != want {
		t.Errorf("known_hosts = %q, want %q", kh, want)
	}

	t.Run("no sign-in", func(t *testing.T) {
		g := newTicketFixture(t, ticketHealthz(hk, true, ""))
		_, _, err := runTicketCmd(t, sshProxyCmd, g.url, "", openStdin(t), testRunID, "--for", "editor")
		if err == nil || err.Error() != "ssh-proxy: sign in first (wardyn login)" || g.mints.Load() != 0 || g.dials.Load() != 0 {
			t.Errorf("error = %v, mints %d, dials %d; want the sign-in line and nothing sent", err, g.mints.Load(), g.dials.Load())
		}
	})
	t.Run("tickets off", func(t *testing.T) {
		g := newTicketFixture(t, ticketHealthz(hk, false, ""))
		_, _, err := runTicketCmd(t, sshProxyCmd, g.url, "bearer-x", openStdin(t), testRunID, "--for", "editor")
		if err == nil || !strings.Contains(err.Error(), "WARDYN_SSH_TICKETS") || g.mints.Load() != 0 {
			t.Errorf("error = %v, mints %d; want a refusal naming WARDYN_SSH_TICKETS and no mint", err, g.mints.Load())
		}
	})
}

// TestSSHProxy_RootPrintsNothingToStdout: the root command's pre-run writes nothing to stdout before the tunnel.
func TestSSHProxy_RootPrintsNothingToStdout(t *testing.T) {
	home := homeWithKey(t, false)
	for _, k := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(k, home)
	}
	for _, k := range []string{"WARDYN_TOKEN", "WARDYN_ADMIN_TOKEN", "WARDYN_URL"} {
		t.Setenv(k, "")
	}
	f := newTicketFixture(t, "")
	f.door = func(w http.ResponseWriter, r *http.Request) {
		if c, err := websocket.Accept(w, r, nil); err == nil {
			_ = c.Close(websocket.StatusNormalClosure, "")
		}
	}
	t.Setenv(sshTicketEnv, "handed-over")
	root := rootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(openStdin(t))
	root.SetArgs([]string{"run", "ssh-proxy", testRunID, "--for", "exec", "--url", f.url})
	if err := root.Execute(); err != nil {
		t.Fatalf("ssh-proxy: %v (stderr %q)", err, errOut.String())
	}
	if out.Len() != 0 || f.dials.Load() != 1 {
		t.Errorf("stdout = %q, dials %d; want nothing on stdout and one dial", out.String(), f.dials.Load())
	}
}

func TestSSHProxy_RefusesAPlainHTTPServer(t *testing.T) {
	t.Setenv(sshTicketEnv, "handed-over")
	_, _, err := runTicketCmd(t, sshProxyCmd, "http://wardyn.example.test", "", openStdin(t), testRunID, "--for", "exec")
	if err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("error = %v, want the plain-http refusal", err)
	}
	if os.Getenv(sshTicketEnv) != "" {
		t.Error("the refused proxy left the ticket in its environment")
	}
}

// wantTicketArgv is a verb's argv on the ticket path against base.
func wantTicketArgv(t *testing.T, verb, base string, front, tail []string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	line, err := proxyCommandLine(runtime.GOOS, exe, testRunID, verb, base)
	if err != nil {
		t.Fatal(err)
	}
	conn := []string{"-o", "ProxyCommand=" + line, "-o", "HostKeyAlias=" + gatewayHostKeyAlias(base),
		"-o", "UserKnownHostsFile=~/.wardyn/known_hosts", "-o", "StrictHostKeyChecking=yes",
		"-o", "ControlMaster=no", "-o", "ControlPath=none", testRunID + "@wardyn-" + testRunID}
	return append(append(append([]string{}, front...), conn...), tail...)
}

func TestTicketPath_SSHArgvAndTheMint(t *testing.T) {
	for _, tc := range []struct {
		verbCase
		front, tail []string
		mint        string
	}{
		{ticketVerbs[0], []string{"-T"}, []string{"--", "echo 'hi there'"}, "purpose=ssh-exec"},
		{ticketVerbs[1], []string{"-N", "-o", "ExitOnForwardFailure=yes", "-L", "127.0.0.1:8080:127.0.0.1:3000", "-L", "127.0.0.1:5000:127.0.0.1:5000"}, nil, "ports=3000%2C5000&purpose=ssh-forward"},
		{ticketVerbs[2], nil, nil, "purpose=ssh-shell"},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			homeWithKey(t, true) // a key on this computer is still not offered
			hk := newTestHostKey(t)
			f := newTicketFixture(t, ticketHealthz(hk, true, ""))
			got := captureSSH(t)
			if _, err := tc.run(t, f); err != nil {
				t.Fatal(err)
			}
			if len(*got) != 1 {
				t.Fatalf("ssh ran %d times, want 1", len(*got))
			}
			eq(t, (*got)[0].cmd.Args[1:], wantTicketArgv(t, tc.verb, f.url, tc.front, tc.tail))
			if f.mintQ[0].Encode() != tc.mint || f.keyGET.Load() != 0 || f.mintAuth[0] != "Bearer bearer-x" {
				t.Errorf("mint %q (auth %q), key listings %d; want %q with the bearer and no key listing", f.mintQ[0].Encode(), f.mintAuth[0], f.keyGET.Load(), tc.mint)
			}
		})
	}
}

// TestTicketPath_ParentNeverSetsTheTicket: the ticket is in ssh(1)'s environment once, after the scrubbed entries,
// and never in this process's own.
func TestTicketPath_ParentNeverSetsTheTicket(t *testing.T) {
	for _, vc := range ticketVerbs[:3] {
		t.Run(vc.verb, func(t *testing.T) {
			homeWithKey(t, false)
			hk := newTestHostKey(t)
			f := newTicketFixture(t, ticketHealthz(hk, true, ""))
			got := captureSSH(t)
			if _, err := vc.run(t, f); err != nil {
				t.Fatal(err)
			}
			c := (*got)[0]
			if c.parentEnv != "" || os.Getenv(sshTicketEnv) != "" {
				t.Errorf("the parent's %s = %q at the child's start, %q after; want it never set", sshTicketEnv, c.parentEnv, os.Getenv(sshTicketEnv))
			}
			env := c.cmd.Env
			n := 0
			for _, kv := range env {
				if strings.HasPrefix(kv, sshTicketEnv+"=") {
					n++
				}
			}
			if n != 1 || env[len(env)-1] != sshTicketEnv+"=tkt-1" {
				t.Fatalf("child env holds the ticket %d times, last entry %q; want once, last", n, env[len(env)-1])
			}
			if !slices.Equal(env[:len(env)-1], cliutil.ScrubChildEnv(os.Environ())) {
				t.Error("the entries before the ticket are not the scrubbed environment")
			}
		})
	}
}

// TestProxyCommandLine_QuoteThenShell: the quoted line, before % doubling, reaches a POSIX shell as exactly the
// words it was built from, whatever the server URL holds.
func TestProxyCommandLine_QuoteThenShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell quoting")
	}
	dir := filepath.Join(t.TempDir(), "my bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "wardyn")
	writeFile(t, exe, "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n")
	if err := os.Chmod(exe, 0o755); err != nil {
		t.Fatal(err)
	}
	base := "http://127.0.0.1:8443/p?a=$(id)&b=`id`;id;&id&%h"
	words, err := proxyCommandWords("linux", exe, testRunID, "exec", base)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-c", words).Output()
	if err != nil {
		t.Fatalf("sh -c %q: %v", words, err)
	}
	eq(t, strings.Split(strings.TrimRight(string(out), "\n"), "\n"), []string{"run", "ssh-proxy", testRunID, "--for", "exec", "--url", base})
}

// sshPercentExpand is ssh_config(5)'s TOKENS expansion: %% is %, and any other %x a token (marked here).
func sshPercentExpand(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		if s[i] == '%' {
			b.WriteByte('%')
		} else {
			b.WriteString("<TOKEN " + string(s[i]) + ">")
		}
	}
	return b.String()
}

// TestProxyCommandLine_PercentRoundTrip: ssh's token expansion of the line gives back the quoted words byte for
// byte, so no %h or %p in the URL is ever expanded.
func TestProxyCommandLine_PercentRoundTrip(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		base := "https://wardyn.example.test/a%25h/%41?q=%%h"
		words, err := proxyCommandWords(goos, "/opt/wardyn%p/wardyn", testRunID, "exec", base)
		if err != nil {
			t.Fatal(err)
		}
		line, err := proxyCommandLine(goos, "/opt/wardyn%p/wardyn", testRunID, "exec", base)
		if err != nil {
			t.Fatal(err)
		}
		if got := sshPercentExpand(line); got != words {
			t.Errorf("%s: ssh expands %q to %q, want %q", goos, line, got, words)
		}
	}
}

func TestProxyCommandLine_RefusesWhatItCannotCarry(t *testing.T) {
	for _, tc := range []struct{ name, exe, base string }{
		{"a control character in the URL", "/usr/bin/wardyn", "https://wardyn.example.test/\x01"},
		{"a newline in the URL", "/usr/bin/wardyn", "https://wardyn.example.test/\n"},
		{"a URL that does not parse", "/usr/bin/wardyn", "https://wardyn.example.test/%zz"},
		{"a quote in the URL", "/usr/bin/wardyn", "https://wardyn.example.test/'"},
		{"a double quote in the URL", "/usr/bin/wardyn", `https://wardyn.example.test/"`},
		{"plain http elsewhere", "/usr/bin/wardyn", "http://wardyn.example.test/"},
		{"another scheme", "/usr/bin/wardyn", "ftp://wardyn.example.test/"},
		{"no host", "/usr/bin/wardyn", "https:///x"},
		{"a quote in the path", "/usr/bin/war'dyn", "https://wardyn.example.test/"},
		{"a newline in the path", "/usr/bin/war\ndyn", "https://wardyn.example.test/"},
		{"no path", "", "https://wardyn.example.test/"},
	} {
		for _, goos := range []string{"linux", "windows"} {
			if line, err := proxyCommandLine(goos, tc.exe, testRunID, "exec", tc.base); err == nil {
				t.Errorf("%s (%s): line %q, want a refusal", tc.name, goos, line)
			}
		}
	}
}

// TestProxyCommandLine_WindowsForm pins the Windows line on every OS.
func TestProxyCommandLine_WindowsForm(t *testing.T) {
	const want = `"C:\Program Files\Wardyn CLI\wardyn.exe" run ssh-proxy "00000000-0000-4000-8000-000000000001" --for "exec" --url "https://wardyn.example.test:8443/"`
	got, err := proxyCommandLine("windows", `C:\Program Files\Wardyn CLI\wardyn.exe`, "00000000-0000-4000-8000-000000000001", "exec", "https://wardyn.example.test:8443/")
	if err != nil || got != want {
		t.Fatalf("line = %q, %v\nwant  %q", got, err, want)
	}
}

// crtSplit splits a Windows command line by the Microsoft C runtime's documented argument rules ("Parsing C
// command-line arguments"). The first argument is the program name: a double quote only groups, and backslashes are
// literal. In the later ones, 2n backslashes before a double quote give n and the quote starts or ends a quoted
// part; 2n+1 give n and a literal double quote; backslashes not before a double quote are literal; inside a quoted
// part a doubled double quote is one literal double quote; a space or tab outside a quoted part ends the argument.
// It is the oracle for TestProxyCommandLine_WindowsQuoting: never edit it to make a row pass.
func crtSplit(line string) []string {
	rs := []rune(line)
	i, inQ := 0, false
	var cur []rune
	for ; i < len(rs) && (inQ || (rs[i] != ' ' && rs[i] != '\t')); i++ {
		if rs[i] == '"' {
			inQ = !inQ
		} else {
			cur = append(cur, rs[i])
		}
	}
	args := []string{string(cur)}
	for {
		for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t') {
			i++
		}
		if i >= len(rs) {
			return args
		}
		cur, inQ = nil, false
	arg:
		for i < len(rs) {
			switch r := rs[i]; {
			case r == '\\':
				n := 0
				for ; i < len(rs) && rs[i] == '\\'; i++ {
					n++
				}
				if i < len(rs) && rs[i] == '"' {
					cur = append(cur, []rune(strings.Repeat(`\`, n/2))...)
					if n%2 == 1 {
						cur = append(cur, '"')
						i++
					}
				} else {
					cur = append(cur, []rune(strings.Repeat(`\`, n))...)
				}
			case r == '"' && inQ && i+1 < len(rs) && rs[i+1] == '"':
				cur = append(cur, '"')
				i += 2
			case r == '"':
				inQ = !inQ
				i++
			case !inQ && (r == ' ' || r == '\t'):
				break arg
			default:
				cur = append(cur, r)
				i++
			}
		}
		args = append(args, string(cur))
	}
}

// TestProxyCommandLine_WindowsQuoting: each Windows line equals its literal, and undoing ssh's %% then splitting it
// by the C runtime's rules gives back exactly the words it was built from.
func TestProxyCommandLine_WindowsQuoting(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	const tail = ` run ssh-proxy "` + id + `" --for "exec" --url `
	for _, tc := range []struct{ name, exe, base, want string }{
		{"a space in a directory", `C:\Program Files\Wardyn CLI\wardyn.exe`, `https://wardyn.example.test:8443/`,
			`"C:\Program Files\Wardyn CLI\wardyn.exe"` + tail + `"https://wardyn.example.test:8443/"`},
		{"a trailing-backslash directory", `C:\Program Files\Wardyn CLI\bin\`, `https://wardyn.example.test/`,
			`"C:\Program Files\Wardyn CLI\bin\"` + tail + `"https://wardyn.example.test/"`},
		{"a backslash before the closing quote", `C:\Wardyn\wardyn.exe`, `https://wardyn.example.test/base\`,
			`"C:\Wardyn\wardyn.exe"` + tail + `"https://wardyn.example.test/base\\"`},
		{"backslash runs inside and at the end", `C:\Wardyn\wardyn.exe`, `https://wardyn.example.test/a\\b\\`,
			`"C:\Wardyn\wardyn.exe"` + tail + `"https://wardyn.example.test/a\\b\\\\"`},
		{"a UNC path and a percent", `\\fileserver\tools\wardyn.exe`, `https://wardyn.example.test/p%25q`,
			`"\\fileserver\tools\wardyn.exe"` + tail + `"https://wardyn.example.test/p%%25q"`},
		{"a doubled percent", `C:\Wardyn\wardyn.exe`, `https://wardyn.example.test/p?x=%%25`,
			`"C:\Wardyn\wardyn.exe"` + tail + `"https://wardyn.example.test/p?x=%%%%25"`},
		{"an ampersand", `C:\Wardyn & Co\wardyn.exe`, `https://wardyn.example.test/a&b`,
			`"C:\Wardyn & Co\wardyn.exe"` + tail + `"https://wardyn.example.test/a&b"`},
		{"a caret", `C:\Wardyn^Tools\wardyn.exe`, `https://wardyn.example.test/a^b`,
			`"C:\Wardyn^Tools\wardyn.exe"` + tail + `"https://wardyn.example.test/a^b"`},
		{"parentheses", `C:\Program Files (x86)\Wardyn\wardyn.exe`, `https://wardyn.example.test/(a)`,
			`"C:\Program Files (x86)\Wardyn\wardyn.exe"` + tail + `"https://wardyn.example.test/(a)"`},
		{"a non-ASCII directory", `C:\Users\Zoë Ångström\wardyn.exe`, `https://wardyn.example.test/ünï`,
			`"C:\Users\Zoë Ångström\wardyn.exe"` + tail + `"https://wardyn.example.test/ünï"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line, err := proxyCommandLine("windows", tc.exe, id, "exec", tc.base)
			if err != nil {
				t.Fatal(err)
			}
			if line != tc.want {
				t.Errorf("line = %s\nwant   %s", line, tc.want)
			}
			got := crtSplit(strings.ReplaceAll(line, "%%", "%"))
			want := []string{tc.exe, "run", "ssh-proxy", id, "--for", "exec", "--url", tc.base}
			if !slices.Equal(got, want) {
				t.Errorf("split = %q\nwant    %q", got, want)
			}
		})
	}
}

func TestTicketConfigBlock_HoldsNoTicketAndNoKey(t *testing.T) {
	got := ticketConfigBlock("https://wardyn.example.test:8443/", testRunID, "LINE")
	want := "Host wardyn-" + testRunID + "\n  HostName wardyn.example.test\n  User " + testRunID + "\n  ProxyCommand LINE\n" +
		"  HostKeyAlias wardyn-gateway-wardyn.example.test-8443\n  UserKnownHostsFile ~/.wardyn/known_hosts\n  StrictHostKeyChecking yes\n"
	if got != want {
		t.Errorf("block =\n%q\nwant\n%q", got, want)
	}
}

func TestRunEditor_TicketPathWritesTheProxyBlock(t *testing.T) {
	home := homeWithKey(t, true)
	codeLog := fakeCode(t)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".ssh", "config"), "Include ~/.wardyn/ssh_config\n")
	hk := newTestHostKey(t)
	f := newTicketFixture(t, ticketHealthz(hk, true, ""))
	if _, _, err := runTicketCmd(t, runEditorCmd, f.url, "bearer-x", strings.NewReader(""), testRunID); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	line, err := proxyCommandLine(runtime.GOOS, exe, testRunID, "editor", f.url)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := os.ReadFile(filepath.Join(home, ".wardyn", "ssh_config"))
	if want := ticketConfigBlock(f.url, testRunID, line); string(cfg) != want {
		t.Errorf("~/.wardyn/ssh_config =\n%q\nwant\n%q", cfg, want)
	}
	if f.mints.Load() != 0 || f.keyGET.Load() != 0 {
		t.Errorf("mints %d, key listings %d; want none (the proxy mints for each connection)", f.mints.Load(), f.keyGET.Load())
	}
	kh, _ := os.ReadFile(filepath.Join(home, ".wardyn", "known_hosts"))
	if !strings.HasPrefix(string(kh), gatewayHostKeyAlias(f.url)+" ") {
		t.Errorf("known_hosts = %q, want the gateway's key under its alias", kh)
	}
	eq(t, readArgs(t, codeLog), []string{"--remote", "ssh-remote+wardyn-" + testRunID, "/home/agent/work"})
}

func TestRefreshGatewayHostKey_KeepsOtherLinesAndRefusesAMismatch(t *testing.T) {
	home := homeWithKey(t, false)
	hk := newTestHostKey(t)
	path := filepath.Join(home, ".wardyn", "known_hosts")
	const base = "https://Wardyn.Example.test:8443"
	alias := "wardyn-gateway-wardyn.example.test-8443"
	old := "other.example ssh-ed25519 AAAAother\n" + alias + " ssh-ed25519 AAAAold\n# a comment\n"
	writeFile(t, path, old)
	gw := sshGateway{hostKey: hk.line, fingerprint: hk.print}
	if err := refreshGatewayHostKey("exec", base, gw); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if want := "other.example ssh-ed25519 AAAAother\n# a comment\n" + alias + " " + string(ssh.MarshalAuthorizedKey(hk.key)); string(got) != want {
		t.Errorf("known_hosts =\n%q\nwant\n%q", got, want)
	}
	for _, bad := range []sshGateway{{hostKey: hk.line, fingerprint: "SHA256:nope"}, {fingerprint: hk.print}, {hostKey: "not a key", fingerprint: hk.print}} {
		before, _ := os.ReadFile(path)
		if err := refreshGatewayHostKey("exec", base, bad); err == nil {
			t.Errorf("host key %q with fingerprint %q: no error", bad.hostKey, bad.fingerprint)
		}
		if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
			t.Error("a refused host key rewrote known_hosts")
		}
	}
}

// TestTicketPath_ChoosesThePath: tickets absent and --key take the key path; a kind that is off is refused before
// any mint; tickets on take the ticket path.
func TestTicketPath_ChoosesThePath(t *testing.T) {
	hk := newTestHostKey(t)
	for _, tc := range []struct {
		name, healthz string
		extra         []string
		ticket        bool
		refusal       string
	}{
		{"tickets absent", ticketHealthz(hk, false, ""), nil, false, ""},
		{"tickets on", ticketHealthz(hk, true, ""), nil, true, ""},
		{"tickets on with --key", ticketHealthz(hk, true, ""), []string{"--key"}, false, ""},
		{"tickets on, exec off", ticketHealthz(hk, true, `,"channels":["shell"]`), nil, false, "does not serve commands (exec)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homeWithKey(t, false)
			f := newTicketFixture(t, tc.healthz)
			got := captureSSH(t)
			args := append([]string{testRunID}, tc.extra...)
			_, _, err := runTicketCmd(t, runExecCmd, f.url, "bearer-x", strings.NewReader(""), append(args, "--", "true")...)
			if tc.refusal != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refusal) || f.mints.Load() != 0 || len(*got) != 0 {
					t.Fatalf("error = %v, mints %d, ssh runs %d; want the channel refusal before any mint", err, f.mints.Load(), len(*got))
				}
				return
			}
			if err != nil || len(*got) != 1 {
				t.Fatalf("error = %v, ssh runs %d", err, len(*got))
			}
			argv := strings.Join((*got)[0].cmd.Args, "\x00")
			if onTicket := strings.Contains(argv, "ssh-proxy"); onTicket != tc.ticket || (f.mints.Load() == 1) != tc.ticket || (f.keyGET.Load() == 0) != tc.ticket {
				t.Errorf("argv %q, mints %d, key listings %d; want the ticket path = %v", (*got)[0].cmd.Args, f.mints.Load(), f.keyGET.Load(), tc.ticket)
			}
			if !tc.ticket {
				eq(t, (*got)[0].cmd.Args[1:], []string{"-T", testRunID + "@gw.example.com", "-p", "2222", "--", "true"})
			}
		})
	}
}

func TestRunSSH_PrintModesKeepTheKeyForm(t *testing.T) {
	homeWithKey(t, true)
	hk := newTestHostKey(t)
	on := newTicketFixture(t, ticketHealthz(hk, true, ""))
	off := newTicketFixture(t, ticketHealthz(hk, false, ""))
	for _, mode := range []string{"--print", "--config", "--json"} {
		got, _, err := runTicketCmd(t, sshCmd, on.url, "", strings.NewReader(""), testRunID, mode)
		want, _, werr := runTicketCmd(t, sshCmd, off.url, "", strings.NewReader(""), testRunID, mode)
		if err != nil || werr != nil || got != want {
			t.Errorf("%s with tickets = %q (%v), want %q (%v) as without", mode, got, err, want, werr)
		}
	}
	if on.mints.Load() != 0 {
		t.Errorf("mints = %d, want none", on.mints.Load())
	}
}

func TestTicketMint_HelperAndPortalHints(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{
		{"support_door_not_granted", ": SSH tickets are for the run's owner; a support helper connects with a registered key: run again with --key"},
		{"ssh_ticket_unsupported_caller", ": SSH tickets need a person signed in directly (wardyn login); run again with --key to use a registered key"},
	} {
		for _, vc := range ticketVerbs {
			t.Run(tc.reason+"/"+vc.verb, func(t *testing.T) {
				homeWithKey(t, false)
				hk := newTestHostKey(t)
				f := newTicketFixture(t, ticketHealthz(hk, true, ""))
				f.refuseMint(http.StatusForbidden, `{"error":"refused","reason":"`+tc.reason+`"}`)
				got := captureSSH(t)
				_, err := vc.run(t, f)
				if err == nil || err.Error() != vc.verb+tc.want || exitCodeFor(err) != 1 || len(*got) != 0 || f.dials.Load() != 0 {
					t.Errorf("error = %v (exit %d), ssh runs %d, dials %d; want %q, exit 1, nothing started", err, exitCodeFor(err), len(*got), f.dials.Load(), vc.verb+tc.want)
				}
			})
		}
	}
}

func TestTicketMint_CapabilityDenied(t *testing.T) {
	const sentence = "SSH tickets aren't available to you. Ask your admin."
	for _, vc := range ticketVerbs {
		t.Run(vc.verb, func(t *testing.T) {
			homeWithKey(t, false)
			hk := newTestHostKey(t)
			f := newTicketFixture(t, ticketHealthz(hk, true, ""))
			f.refuseMint(http.StatusForbidden, `{"error":"`+sentence+`","reason":"capability_feature","feature":"ssh_key"}`)
			got := captureSSH(t)
			_, err := vc.run(t, f)
			if err == nil || err.Error() != vc.verb+": "+sentence || exitCodeFor(err) != 1 || len(*got) != 0 || f.dials.Load() != 0 {
				t.Errorf("error = %v (exit %d), ssh runs %d, dials %d; want %q, exit 1, nothing started", err, exitCodeFor(err), len(*got), f.dials.Load(), vc.verb+": "+sentence)
			}
			if err != nil && strings.Contains(err.Error(), "--key") {
				t.Errorf("error = %q points to --key", err)
			}
		})
	}
	t.Run("sync", func(t *testing.T) {
		homeWithKey(t, false)
		f := newSyncTicketFixture(t, syncHealthz(""))
		f.mint = func(w http.ResponseWriter, _ url.Values) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"` + sentence + `","reason":"capability_feature","feature":"ssh_key"}`))
		}
		err := runTicketSync(t, f)
		if err == nil || err.Error() != "sync: "+sentence || exitCodeFor(err) != 1 || f.dials.Load() != 0 {
			t.Errorf("error = %v (exit %d), dials %d; want %q, exit 1, no dial", err, exitCodeFor(err), f.dials.Load(), "sync: "+sentence)
		}
	})
}

func TestTicketMint_NoEchoRefuses(t *testing.T) {
	for _, vc := range ticketVerbs {
		t.Run(vc.verb, func(t *testing.T) {
			homeWithKey(t, false)
			hk := newTestHostKey(t)
			f := newTicketFixture(t, ticketHealthz(hk, true, ""))
			f.mint = func(w http.ResponseWriter, _ url.Values) {
				_, _ = w.Write([]byte(`{"ticket":"terminal-ticket","expires_in_seconds":30}`))
			}
			got := captureSSH(t)
			_, err := vc.run(t, f)
			want := vc.verb + ": this server minted a ticket that does not open SSH; run again with --key"
			if err == nil || err.Error() != want || len(*got) != 0 || f.dials.Load() != 0 {
				t.Errorf("error = %v, ssh runs %d, dials %d; want %q and nothing started", err, len(*got), f.dials.Load(), want)
			}
		})
	}
}

func TestTicketMint_InconclusiveIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		mint func(w http.ResponseWriter, q url.Values)
	}{
		{"a non-JSON 502", func(w http.ResponseWriter, _ url.Values) { http.Error(w, "bad gateway", http.StatusBadGateway) }},
		{"a transport error", func(w http.ResponseWriter, _ url.Values) { dropConn(w) }},
	} {
		for _, vc := range ticketVerbs {
			t.Run(tc.name+"/"+vc.verb, func(t *testing.T) {
				homeWithKey(t, false)
				hk := newTestHostKey(t)
				f := newTicketFixture(t, ticketHealthz(hk, true, ""))
				f.mint = tc.mint
				got := captureSSH(t)
				if _, err := vc.run(t, f); err == nil || len(*got) != 0 || f.dials.Load() != 0 {
					t.Errorf("error = %v, ssh runs %d, dials %d; want an error and nothing started", err, len(*got), f.dials.Load())
				}
			})
		}
		t.Run(tc.name+"/the terminal's mint falls back", func(t *testing.T) {
			f := newTicketFixture(t, "")
			f.mint = tc.mint
			if tok, err := mintAttachTicket(context.Background(), &sdk.Client{BaseURL: f.url}, testRunID); tok != "" || err != nil {
				t.Errorf("mintAttachTicket = %q, %v; want \"\" and nil (the caller falls back)", tok, err)
			}
		})
	}
}

func TestRunForward_TicketCoversAtMost16Ports(t *testing.T) {
	homeWithKey(t, false)
	hk := newTestHostKey(t)
	f := newTicketFixture(t, ticketHealthz(hk, true, ""))
	got := captureSSH(t)
	args := []string{testRunID}
	for p := 3001; p <= 3017; p++ {
		args = append(args, strconv.Itoa(p))
	}
	_, _, err := runTicketCmd(t, runForwardCmd, f.url, "bearer-x", strings.NewReader(""), args...)
	if err == nil || err.Error() != "forward: a ticket covers at most 16 sandbox ports; split the command" || f.mints.Load() != 0 || len(*got) != 0 {
		t.Fatalf("17 ports: error = %v, mints %d, ssh runs %d; want the refusal before any mint", err, f.mints.Load(), len(*got))
	}
	// Sixteen sandbox ports, one forwarded twice, fit: each port is named once.
	args = append(args[:17], "9000:3001")
	if _, _, err := runTicketCmd(t, runForwardCmd, f.url, "bearer-x", strings.NewReader(""), args...); err != nil {
		t.Fatal(err)
	}
	if ports := strings.Split(f.mintQ[0].Get("ports"), ","); len(ports) != 16 || ports[0] != "3001" || ports[15] != "3016" {
		t.Errorf("ports = %q, want 3001 to 3016 once each", f.mintQ[0].Get("ports"))
	}
}

// TestResolveSSHAccess_TicketPathSkipsAdvertisedProxy: on the ticket path the advertised ProxyCommand needs no
// consent and no key is listed; --key keeps both.
func TestResolveSSHAccess_TicketPathSkipsAdvertisedProxy(t *testing.T) {
	homeWithKey(t, false)
	hk := newTestHostKey(t)
	f := newTicketFixture(t, ticketHealthz(hk, true, `,"proxy_command":"nc %h %p"`))
	c := &sdk.Client{BaseURL: f.url}
	for _, verb := range []string{"exec", "forward", "editor"} {
		acc, err := resolveSSHAccessKey(context.Background(), c, verb, testRunID, false, false)
		if err != nil || !acc.ticket {
			t.Errorf("%s: %v, ticket %v; want the ticket path", verb, err, acc.ticket)
		}
		if _, err := resolveSSHAccessKey(context.Background(), c, verb, testRunID, false, true); err == nil || !strings.Contains(err.Error(), "advertises a ProxyCommand") {
			t.Errorf("%s --key: %v, want today's ProxyCommand refusal", verb, err)
		}
	}
	if f.keyGET.Load() != 0 {
		t.Errorf("key listings = %d, want none", f.keyGET.Load())
	}
	got := captureSSH(t)
	if _, _, err := runTicketCmd(t, sshCmd, f.url, "bearer-x", strings.NewReader(""), testRunID); err != nil || len(*got) != 1 {
		t.Fatalf("run ssh: %v, ssh runs %d", err, len(*got))
	}
	if argv := strings.Join((*got)[0].cmd.Args, " "); strings.Contains(argv, "nc %h") || !strings.Contains(argv, "ssh-proxy") {
		t.Errorf("argv %q, want the ssh-proxy line and not the advertised one", argv)
	}
}
