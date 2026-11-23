// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net"
	"testing"
	"time"
)

// The SSH gateway's default bounds, per replica.
func TestSSHGatewayDefaults(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"DefaultSSHMaxConnections", DefaultSSHMaxConnections, 256},
		{"defaultSSHSessionsPerRun", defaultSSHSessionsPerRun, 8},
		{"DefaultSSHMaxHandshakes", DefaultSSHMaxHandshakes, 64},
		{"DefaultSSHMaxConnectionsPerPrincipal", DefaultSSHMaxConnectionsPerPrincipal, 8},
		{"DefaultSSHMaxHandshakesPerSource", DefaultSSHMaxHandshakesPerSource, 16},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// Config.SSHMaxHandshakes sets the handshake total: with 3, the fourth connection is closed before any byte.
func TestSSHGateway_HandshakeTotalFollowsConfig(t *testing.T) {
	h := newSSHTestHarness(t, newSSHMemStore(), &sshFakeRunner{}, func(c *Config) {
		c.SSHMaxHandshakes = 3
		c.SSHMaxHandshakesPerSource = 4
	})

	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < 3; i++ {
		c, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
		if err != nil {
			t.Fatalf("conn %d: %v", i, err)
		}
		conns = append(conns, c)
	}

	over, err := net.DialTimeout("tcp", h.addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial over the total: %v", err)
	}
	defer over.Close()
	_ = over.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	if n, rerr := over.Read(buf); n > 0 || rerr == nil {
		t.Errorf("the fourth connection got a byte / no error (n=%d, err=%v), want it closed immediately", n, rerr)
	}
}
