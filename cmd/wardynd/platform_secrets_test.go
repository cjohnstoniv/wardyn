// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/broker"
)

// TestPlatformSecretsAreReservedEverywhere is the parity check the reserved-name
// sets never had. Two packages keep their own map of names nobody may write or
// resolve — internal/api's reservedSecretNames and internal/broker's
// reservedBrokerSecretNames — and NEITHER can see the constants above, so every
// platform key so far has been added to the maps by hand and by memory.
// wardyn-ui-session-key was missed in both; wardyn-ssh-host-key was missed in
// one. This is the only package that can see all three, which is why the test
// lives here rather than beside either map.
//
// The list is the keys the daemon GENERATES for itself (loadOrCreateSecret):
// nobody authors them, nobody may overwrite them, and no grant may name them, so
// both guards must refuse all of them. The GitHub App pair (secretGitHubAppID /
// secretGitHubAppKey) is deliberately NOT here — those are operator-PROVIDED
// credentials that must stay Put-able through the secrets API, and they are
// sealed broker-side only (see api.sinkReservedSecret's doc comment).
//
// Adding a key to the const block above and not to both maps fails here.
func TestPlatformSecretsAreReservedEverywhere(t *testing.T) {
	for _, name := range []string{
		secretSigningKey,
		secretSessionKey,
		secretSSHHostKey,
		secretUISessionKey,
	} {
		if !api.ReservedPlatformSecret(name) {
			t.Errorf("%s: not in internal/api's reserved set — GET /secrets lists it and PUT/DELETE clobbers it", name)
		}
		if !broker.ReservedSecretName(name) {
			t.Errorf("%s: not in internal/broker's reserved set — a git_pat/ssh_key grant naming it hands the raw key into a sandbox", name)
		}
	}
}
