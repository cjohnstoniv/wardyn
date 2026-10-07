// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
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
