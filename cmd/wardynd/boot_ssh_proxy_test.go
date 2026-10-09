// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"
)

func TestValidateSSHProxyCommand(t *testing.T) {
	const good = "openssl s_client -quiet -verify_return_error -verify_hostname %h -connect %h:443 -servername %h"
	for _, ok := range []string{"", good, strings.Repeat("a", 512)} {
		if err := validateSSHProxyCommand(ok); err != nil {
			t.Fatalf("%d-byte value refused: %v", len(ok), err)
		}
	}
	for _, tc := range []struct{ name, value, want string }{
		{"control character", good + "\x1b[2J", "control character"},
		{"newline", good + "\nLocalCommand id", "newline"},
		{"single quote", good + "'; id; '", "single quote"},
		{"over 512 bytes", strings.Repeat("a", 513), "over 512 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSSHProxyCommand(tc.value)
			if err == nil {
				t.Fatal("want refused, got accepted")
			}
			for _, part := range []string{"-ssh-proxy-command", "WARDYN_SSH_PROXY_COMMAND", tc.want} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not mention %q", err, part)
				}
			}
		})
	}
}

func TestSSHMaxSessionsPerRunBoot(t *testing.T) {
	ensureUnset(t, "WARDYN_SSH_MAX_SESSIONS_PER_RUN")
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"wardynd-test"}

	resetFlags(t)
	if got := *parseBootFlags().sshMaxSessionsPerRun; got != 4 {
		t.Fatalf("default = %d, want 4 (today's cap, so an upgrade changes nothing)", got)
	}
	t.Setenv("WARDYN_SSH_MAX_SESSIONS_PER_RUN", "8")
	resetFlags(t)
	if got := *parseBootFlags().sshMaxSessionsPerRun; got != 8 {
		t.Fatalf("env 8 = %d", got)
	}
	for _, ok := range []int{1, 4, 64} {
		if err := validateSSHMaxSessionsPerRun(ok); err != nil {
			t.Errorf("%d refused: %v", ok, err)
		}
	}
	for _, bad := range []int{0, -1, 65, 1000} {
		err := validateSSHMaxSessionsPerRun(bad)
		if err == nil || !strings.Contains(err.Error(), "WARDYN_SSH_MAX_SESSIONS_PER_RUN") {
			t.Errorf("%d: error = %v, want one naming WARDYN_SSH_MAX_SESSIONS_PER_RUN", bad, err)
		}
	}
	t.Setenv("WARDYN_SSH_MAX_SESSIONS_PER_RUN", "0")
	resetFlags(t)
	if err := validateBootPosture(parseBootFlags(), tlsPosture{}); err == nil ||
		!strings.Contains(err.Error(), "WARDYN_SSH_MAX_SESSIONS_PER_RUN") {
		t.Errorf("boot with 0 = %v, want a refusal naming the variable", err)
	}
}
