// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const hostboundMethod = "publickey-hostbound-v00@openssh.com"

// The agent signs a sign-in to the grant's own SSH host only: a host-bound
// one names a host key of the grant's known_hosts or of its forge's pinned
// keys, and a plain one is signed only for Azure DevOps, which has no
// host-bound sign-in.
func TestSSHAgent_SignsInToTheGrantHostOnly(t *testing.T) {
	keyPEM, pub := testSSHKey(t, false)
	gh, ado := knownHostKeys(pinnedSSHHostKeys, "ssh.github.com"), knownHostKeys(pinnedSSHHostKeys, "ssh.dev.azure.com")
	if len(gh) != 3 || len(ado) != 1 {
		t.Fatalf("pinned keys: %d for GitHub, %d for Azure DevOps; want 3 and 1", len(gh), len(ado))
	}
	foreign, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32)).Public())
	if err != nil {
		t.Fatal(err)
	}
	hb := func(hostKey []byte) []byte { return userauthData("git", pub, hostboundMethod, hostKey) }
	plain := userauthData("git", pub, "publickey")
	for _, tc := range []struct {
		name, host string
		data       []byte
		want       error
	}{
		{"github, the grant's known_hosts key", "github.com", hb(testHostKey.Marshal()), nil},
		{"github, a pinned key", "github.com", hb(gh[0]), nil},
		{"github, another pinned key", "ssh.github.com", hb(gh[2]), nil},
		{"github, a foreign host key", "github.com", hb(foreign.Marshal()), errSSHAgentHost},
		{"github, Azure DevOps's key", "github.com", hb(ado[0]), errSSHAgentHost},
		{"github, plain", "github.com", plain, errSSHAgentHost},
		{"azure devops, plain", "dev.azure.com", plain, nil},
		{"azure devops, its pinned key", "dev.azure.com", hb(ado[0]), nil},
		{"azure devops, GitHub's key", "dev.azure.com", hb(gh[0]), errSSHAgentHost},
	} {
		grant := uuid.New()
		p, buf, _ := sshAgentProxy(t, grant, keyPEM, tc.host, nil)
		sig, err := agent.NewClient(&agentConn{t: t, p: p, grant: grant}).Sign(pub, tc.data)
		d := lastDecision(t, buf)
		if tc.want == nil {
			if err != nil || pub.Verify(tc.data, sig) != nil || d.Decision != egress.Allow {
				t.Errorf("%s: sign = %v, decision %+v; want it signed", tc.name, err, d)
			}
			continue
		}
		if err == nil || d.Decision != egress.Deny || d.Cause != tc.want.Error() {
			t.Errorf("%s: sign = %v, decision %+v; want a deny saying %q", tc.name, err, d, tc.want)
		}
	}
}

// A grant whose mint names no SSH host, or one agent-run does not dial, gets
// no key.
func TestSSHAgent_NoGrantHostNoKey(t *testing.T) {
	keyPEM, _ := testSSHKey(t, false)
	for _, host := range []string{"", "gitlab.example"} {
		grant := uuid.New()
		p, buf, _ := sshAgentProxy(t, grant, keyPEM, host, nil)
		if _, err := agent.NewClient(&agentConn{t: t, p: p, grant: grant}).List(); err == nil {
			t.Errorf("host %q: List succeeded, want a refusal", host)
		}
		if d := lastDecision(t, buf); d.Decision != egress.Deny {
			t.Errorf("host %q: decision %+v, want a deny", host, d)
		}
	}
}

// sshBannerUpstream is an SSH server's first flight, then silence.
func sshBannerUpstream(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.WriteString(c, "SSH-2.0-forge\r\n")
				_, _ = io.Copy(io.Discard, c)
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// sshBoundProxy is a proxy holding a resident GitHub ssh_key grant, with
// evil.test and ssh.github.com allowed and both dialled to an SSH server.
func sshBoundProxy(t *testing.T) (p *Proxy, grant uuid.UUID, pub ssh.PublicKey, proxyURL string, done <-chan struct{}) {
	t.Helper()
	p, _ = newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"evil.test", "ssh.github.com"}}, sshBannerUpstream(t), nil, nil)
	grant = uuid.New()
	keyPEM, pub := testSSHKey(t, false)
	p.residentMints = residentMintsByGrant([]ResidentMint{{GrantID: grant, Kind: types.GrantSSHKey, Username: "git",
		Token: keyPEM, ExpiresAt: time.Now().Add(time.Hour), Host: "github.com"}})
	proxyURL, done = serveTunnelProxy(t, p)
	return p, grant, pub, proxyURL, done
}

// sshTunnelTo opens a CONNECT tunnel and returns it with the first line it
// relays ("" when the tunnel ends first).
func sshTunnelTo(t *testing.T, proxyURL, target string) (net.Conn, string) {
	t.Helper()
	conn, resp := connectThrough(t, proxyURL, target)
	if !strings.HasPrefix(resp, "HTTP/1.1 200") {
		t.Fatalf("CONNECT %s = %q, want the proxy's 200", target, resp)
	}
	// The banner may have arrived in the same read as the 200.
	_, early, _ := strings.Cut(resp, "\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(io.MultiReader(strings.NewReader(early), conn)).ReadString('\n')
	if isTimeout(err) {
		t.Fatalf("CONNECT %s: nothing relayed and the tunnel still open after 5s", target)
	}
	return conn, line
}

// While the agent holds a key, the proxy carries SSH only to the grant's own
// endpoint: before the key is held, and after close, SSH goes anywhere the
// policy allows, as before.
func TestSSHAgent_ConnectOnlyToTheGrantEndpoint(t *testing.T) {
	p, grant, _, proxyURL, done := sshBoundProxy(t)
	relays := func(target string) bool {
		conn, line := sshTunnelTo(t, proxyURL, target)
		_ = conn.Close()
		rows := awaitDecision(t, p, done)
		if line == "" {
			if f := tunnelFailedRows(rows); len(f) != 1 || !strings.Contains(f[0].Cause, "SSH goes only to its grants' own SSH hosts") {
				t.Errorf("CONNECT %s refused with rows %+v, want one naming the ssh bound", target, f)
			}
		}
		return line == "SSH-2.0-forge\r\n"
	}
	if !relays("evil.test:443") {
		t.Fatal("before the agent holds a key: SSH to evil.test was not relayed")
	}
	if _, err := agent.NewClient(&agentConn{t: t, p: p, grant: grant}).List(); err != nil {
		t.Fatalf("load the key: %v", err)
	}
	if relays("evil.test:443") {
		t.Fatal("while the agent holds a GitHub key: SSH to evil.test was relayed")
	}
	if !relays("ssh.github.com:443") {
		t.Fatal("while the agent holds a GitHub key: SSH to ssh.github.com was not relayed")
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, routeSSHAgentClose, nil))
	if !relays("evil.test:443") {
		t.Fatal("after close: SSH to evil.test was not relayed")
	}
}

// An SSH session to another host opened before the key was held cannot
// borrow a signature while it is open.
func TestSSHAgent_NoSignWhileOtherSSHIsOpen(t *testing.T) {
	p, grant, pub, proxyURL, done := sshBoundProxy(t)
	conn, line := sshTunnelTo(t, proxyURL, "evil.test:443")
	if line == "" {
		t.Fatal("before the agent holds a key: SSH to evil.test was not relayed")
	}
	sign := func() error {
		_, err := agent.NewClient(&agentConn{t: t, p: p, grant: grant}).Sign(pub,
			userauthData("git", pub, hostboundMethod, knownHostKeys(pinnedSSHHostKeys, "ssh.github.com")[0]))
		return err
	}
	if err := sign(); err == nil {
		t.Fatal("signed while an SSH session to evil.test is open")
	}
	_ = conn.Close()
	awaitDecision(t, p, done)
	if err := sign(); err != nil {
		t.Fatalf("sign once that session ended: %v", err)
	}
}

// The client's SSH identification is seen even split across writes, and
// refused before it completes upstream.
func TestTunnelWatchSeesASplitClientSSHIdent(t *testing.T) {
	client, clientPeer := net.Pipe()
	upstream, upstreamPeer := net.Pipe()
	w := newTunnelWatch(upstream)
	w.onSSH = func() error { return errSSHTunnel }
	go func() { _, _ = tunnel(client, w) }()
	t.Cleanup(func() { _ = clientPeer.Close(); _ = upstreamPeer.Close() })
	got := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(upstreamPeer); got <- b }()
	_, _ = clientPeer.Write([]byte("SS"))
	_, _ = clientPeer.Write([]byte("H-2.0-client\r\n"))
	select {
	case b := <-got:
		if string(b) != "SS" {
			t.Fatalf("upstream got %q, want only the bytes before the identification completed", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tunnel was still open 5s after the client's SSH identification")
	}
	if f := w.failure(false, nil); !strings.Contains(f, "SSH goes only") {
		t.Fatalf("failure = %q, want the ssh bound", f)
	}
}

// The agent's pinned keys are the ones the agent images bake.
func TestPinnedSSHHostKeysMatchImage(t *testing.T) {
	re := regexp.MustCompile(`'((?:github\.com,ssh\.github\.com|ssh\.dev\.azure\.com) [^']+)'`)
	for _, f := range []string{"../../../deploy/images/claude-code/Dockerfile", "../../../deploy/images/base/Dockerfile"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			lines = append(lines, m[1])
		}
		if got := strings.Join(lines, "\n") + "\n"; got != pinnedSSHHostKeys {
			t.Errorf("%s bakes\n%s\nwant pinnedSSHHostKeys\n%s", f, got, pinnedSSHHostKeys)
		}
	}
}
