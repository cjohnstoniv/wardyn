// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

// direct-tcpip reaches only the sandbox's own loopback: "localhost" in any case and the loopback literals, never a
// trailing-dot name or another address.
func TestSSHIsSandboxLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "LOCALHOST": true, "LocalHost": true, "127.0.0.1": true, "127.0.0.2": true, "::1": true,
		"::FFFF:127.0.0.1": true, "localhost.": false, "example.com": false, "10.0.0.1": false, "": false,
	} {
		if got := sshIsSandboxLoopback(host); got != want {
			t.Errorf("sshIsSandboxLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}
