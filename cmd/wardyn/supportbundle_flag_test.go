// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A secret-named flag whose value is a separate argument has no ':' or '=' for
// secretPairRe to find: Compose argv lists and shell strings both carry one.
func TestRedactSecretsSeparatedFlagValues(t *testing.T) {
	for _, tc := range []struct{ name, in, keep string }{
		{"argv password", "services:\n  app:\n    command:\n      - helper\n      - --password\n      - first-sensitive-line\n", "--password"},
		{"argv token, one dash", "services:\n  app:\n    entrypoint: [helper, -token, first-sensitive-line]\n", "-token"},
		{"shell string password", "services:\n  app:\n    command: helper --password first-sensitive-line\n", "command:"},
		{"sh -c token payload", "services:\n  app:\n    command: [sh, -c, \"helper --api-token first-sensitive-line && serve\"]\n", "-c, "},
		{"argv switch before a secret flag", "services:\n  app:\n    command: [helper, --auth, --token, first-sensitive-line]\n", "--auth"},
		{"argv api-key", "services:\n  app:\n    command: [helper, --api-key, first-sensitive-line]\n", "--api-key"},
		{"argv private-key", "services:\n  app:\n    command: [helper, --private-key, first-sensitive-line]\n", "--private-key"},
		{"argv access-key", "services:\n  app:\n    command: [helper, --access-key, first-sensitive-line]\n", "--access-key"},
		{"argv requirepass", "services:\n  cache:\n    command: [redis-server, --requirepass, first-sensitive-line]\n", "--requirepass"},
		{"shell string api-key", "services:\n  app:\n    command: helper --api-key first-sensitive-line\n", "command:"},
		{"joined api-key", "services:\n  app:\n    command: [helper, --api-key=first-sensitive-line]\n", "helper"},
		{"flag after =", "services:\n  app:\n    environment:\n      - EXTRA_ARGS=--token first-sensitive-line\n", "environment:"},
		{"flag after a quote", "services:\n  app:\n    command: sh -c \"--password first-sensitive-line\"\n", "command:"},
		{"flag after (", "services:\n  app:\n    command: run (--api-key first-sensitive-line)\n", "command:"},
		{"flag written as an alias", "services:\n  app:\n    x-flag: &f --password\n    command: [helper, *f, first-sensitive-line]\n", "helper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := string(redactSecrets([]byte(tc.in + "diagnostic: kept\n")))
			if strings.Contains(got, "sensitive-line") {
				t.Errorf("secret survived: %s", got)
			}
			if !strings.Contains(got, tc.keep) || !strings.Contains(got, "diagnostic: kept") {
				t.Errorf("diagnostic content was lost (want %q kept): %s", tc.keep, got)
			}
			var parsed any
			if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
				t.Errorf("redacted YAML is invalid: %v", err)
			}
		})
	}
	// The counterweight: a flag that names no secret keeps its value.
	keep := "services:\n  app:\n    command: [wardynd, --listen, 127.0.0.1:8080]\n"
	if got := string(redactSecrets([]byte(keep))); got != keep {
		t.Errorf("non-secret argv was modified:\n got %q\nwant %q", got, keep)
	}
}
