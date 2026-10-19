// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// fakeAgentProxy answers the ssh agent routes for grant from a keyring holding
// a throwaway key, and refuses every other grant.
func fakeAgentProxy(t *testing.T, grant string) (*httptest.Server, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	signer, _ := ssh.NewSignerFromKey(priv)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == sshAgentRoute+grant+"/known_hosts":
			_, _ = io.WriteString(w, "forge.test ssh-ed25519 AAAAhostkey\n")
		case r.Method == http.MethodPost && r.URL.Path == sshAgentRoute+grant:
			var out bytes.Buffer
			_ = agent.ServeAgent(ring, struct {
				io.Reader
				io.Writer
			}{r.Body, &out})
			_, _ = w.Write(out.Bytes())
		default:
			http.Error(w, "no agent", http.StatusForbidden)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, signer.PublicKey()
}

// TestSSHExecHelperProcess is the ssh child ssh-exec runs in
// TestSSHExec_SocketLivesForTheCommand: it signs through each named socket and
// writes what it saw to WARDYN_TEST_SSH_OUT.
func TestSSHExecHelperProcess(t *testing.T) {
	if os.Getenv("WARDYN_TEST_SSH_CHILD") != "1" {
		return
	}
	var report strings.Builder
	dir := os.Getenv("WARDYN_SSH_AGENT_DIR")
	fmt.Fprintf(&report, "dir=%s\n", dir)
	for _, name := range []string{"good", "refused"} {
		c, err := net.Dial("unix", filepath.Join(dir, name+".sock"))
		if err != nil {
			fmt.Fprintf(&report, "%s dial: %v\n", name, err)
			continue
		}
		keys, err := agent.NewClient(c).List()
		fmt.Fprintf(&report, "%s keys=%d err=%v\n", name, len(keys), err != nil)
		_ = c.Close()
	}
	_ = os.WriteFile(os.Getenv("WARDYN_TEST_SSH_OUT"), []byte(report.String()), 0o600)
	os.Exit(7)
}

// ssh-exec serves one socket per grant for the life of its command only,
// relays each frame to the proxy (a refusing grant gets an agent failure, not
// a hang), passes the command's exit status through and removes the sockets.
func TestSSHExec_SocketLivesForTheCommand(t *testing.T) {
	good, refused := uuid.NewString(), uuid.NewString()
	srv, _ := fakeAgentProxy(t, good)
	out := filepath.Join(t.TempDir(), "report")
	t.Setenv("WARDYN_PROXY_URL", srv.URL)
	t.Setenv("WARDYN_TEST_SSH_CHILD", "1")
	t.Setenv("WARDYN_TEST_SSH_OUT", out)
	code := runSSHVerb("ssh-exec", []string{"--agent", "good=" + good, "--agent", "refused=" + refused,
		"--", os.Args[0], "-test.run=^TestSSHExecHelperProcess$"}, io.Discard, io.Discard)
	if code != 7 {
		t.Fatalf("exit status = %d, want the command's 7", code)
	}
	report, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(report)
	if !strings.Contains(got, "good keys=1 err=false") || !strings.Contains(got, "refused keys=0 err=true") {
		t.Fatalf("child saw:\n%s", got)
	}
	dir := strings.TrimPrefix(strings.SplitN(got, "\n", 2)[0], "dir=")
	if _, err := os.Stat(dir); dir == "" || !os.IsNotExist(err) {
		t.Fatalf("agent dir %q still exists after the command (%v)", dir, err)
	}
}

func TestSSHExec_Usage(t *testing.T) {
	for _, args := range [][]string{
		{"--", "ssh"},
		{"--agent", "x=" + uuid.NewString()},
		{"--agent", "bad/name=" + uuid.NewString(), "--", "ssh"},
		{"--agent", "x=not-a-grant", "--", "ssh"},
	} {
		if code := runSSHVerb("ssh-exec", args, io.Discard, io.Discard); code != 2 {
			t.Errorf("ssh-exec %q = %d, want usage 2", args, code)
		}
	}
}

func TestSSHKnownHosts(t *testing.T) {
	good := uuid.NewString()
	srv, _ := fakeAgentProxy(t, good)
	t.Setenv("WARDYN_PROXY_URL", srv.URL)
	var stdout, stderr bytes.Buffer
	if code := runSSHVerb("ssh-known-hosts", []string{good}, &stdout, &stderr); code != 0 || stdout.String() != "forge.test ssh-ed25519 AAAAhostkey\n" {
		t.Fatalf("known hosts = %d %q (%s)", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	if code := runSSHVerb("ssh-known-hosts", []string{uuid.NewString()}, &stdout, &stderr); code != 1 || stdout.Len() != 0 {
		t.Fatalf("refused grant = %d %q, want 1 and nothing printed", code, stdout.String())
	}
}
