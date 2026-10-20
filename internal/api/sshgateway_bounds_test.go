// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// closedBeforeAnyByte reports whether the server closed c without sending its
// version banner.
func closedBeforeAnyByte(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(make([]byte, 1))
	return n == 0 && err != nil
}

// One source address holds at most 16 (the documented default) of the
// handshake slots: the 17th connection from it is closed before any byte,
// while the slots it leaves stay free for every other source.
func TestSSHGateway_HandshakesPerSourceBounded(t *testing.T) {
	h := newSSHTestHarness(t, newSSHMemStore(), &sshFakeRunner{})
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := range 16 {
		c, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	over, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial over the per-source bound: %v", err)
	}
	defer over.Close()
	if !closedBeforeAnyByte(over) {
		t.Fatal("17th handshake from one source got a byte or stayed open, want it closed before any byte")
	}
	_ = conns[0].Close()
	conns = conns[1:]
	// The freed slot is taken back once the server sees the close; until then a
	// dial may still meet the full bound, so retry within the handshake window.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4)
		n, _ := io.ReadFull(c, buf)
		_ = c.Close()
		if n == 4 && string(buf) == "SSH-" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a slot freed by a closed handshake was never given back to its source")
		}
	}
}

// The established-connection ceiling is configurable; a connection that
// authenticates over it is closed, and the refusal is counted by bound.
func TestSSHGateway_MaxConnectionsConfigurableAndCounted(t *testing.T) {
	st := newSSHMemStore()
	runID := uuid.New()
	st.putRun(types.AgentRun{ID: runID, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1"})
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{}, func(c *Config) { c.SSHMaxConnections = 1 })

	first, err := sshDial(t, h, runID.String(), priv)
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	defer first.Close()
	if second, err := sshDial(t, h, runID.String(), priv); err == nil {
		_, _, serr := second.SendRequest("keepalive@openssh.com", true, nil)
		_ = second.Close()
		if serr == nil {
			t.Fatal("second session over a ceiling of 1 was served, want it closed")
		}
	}
	h.srv.metrics.mu.Lock()
	got := h.srv.metrics.sshRefused[sshBoundEstablished]
	h.srv.metrics.mu.Unlock()
	if got != 1 {
		t.Fatalf("established refusals counted = %d, want 1", got)
	}
	if _, _, err := first.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("first session after the refusal: %v", err)
	}
}

// With the default ceiling, DefaultSSHMaxConnections authenticated sessions are
// served and the next is closed after its key was accepted; closing one session
// frees its slot for a new connection.
func TestSSHGateway_DefaultCeilingRefusesTheNextEstablishedConnection(t *testing.T) {
	st := newSSHMemStore()
	runID := uuid.New()
	st.putRun(types.AgentRun{ID: runID, CreatedBy: "alice@example.com", State: types.RunRunning, SandboxRef: "sbx-1"})
	priv, pub := mustSSHKeypair(t)
	st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: "alice@example.com", PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
	h := newSSHTestHarness(t, st, &sshFakeRunner{})

	served := func(c *ssh.Client) bool {
		_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
		return err == nil
	}
	var clients []*ssh.Client
	defer func() {
		for _, c := range clients {
			_ = c.Close()
		}
	}()
	for i := 0; i < DefaultSSHMaxConnections; i++ {
		c, err := sshDial(t, h, runID.String(), priv)
		if err != nil {
			t.Fatalf("session %d of %d: %v", i+1, DefaultSSHMaxConnections, err)
		}
		clients = append(clients, c)
	}

	if over, err := sshDial(t, h, runID.String(), priv); err == nil {
		ok := served(over)
		_ = over.Close()
		if ok {
			t.Fatalf("session %d was served, want it closed", DefaultSSHMaxConnections+1)
		}
	}
	h.srv.metrics.mu.Lock()
	got := h.srv.metrics.sshRefused[sshBoundEstablished]
	h.srv.metrics.mu.Unlock()
	if got != 1 {
		t.Fatalf("established refusals counted = %d, want 1", got)
	}

	_ = clients[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := sshDial(t, h, runID.String(), priv)
		if err == nil {
			ok := served(c)
			if ok {
				clients = append(clients, c)
				break
			}
			_ = c.Close()
		}
		if time.Now().After(deadline) {
			t.Fatal("a session closed at the ceiling never freed its slot")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
