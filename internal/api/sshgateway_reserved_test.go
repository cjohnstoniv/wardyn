// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSSHGateway_ReservedPrincipalKey: under SSO, a key stored under a
// reserved principal (registered before #1162 by an IdP sub spelled like
// one) is refused into the runs that principal owns, and the refusal is an
// ssh.authenticate failure. Without SSO the same keys are the admin token's
// and the local seat's own, and keep working.
func TestSSHGateway_ReservedPrincipalKey(t *testing.T) {
	for _, sso := range []bool{true, false} {
		name := "local mode"
		if sso {
			name = "sso"
		}
		t.Run(name, func(t *testing.T) {
			st := newSSHMemStore()
			type seat struct {
				run  uuid.UUID
				priv []byte
			}
			seats := map[string]seat{}
			for _, principal := range []string{adminTokenPrincipal, "local:dev"} {
				run := uuid.New()
				st.putRun(types.AgentRun{ID: run, CreatedBy: principal, State: types.RunRunning, SandboxRef: "sbx-" + principal})
				priv, pub := mustSSHKeypair(t)
				st.putKey(types.SSHPublicKey{Fingerprint: ssh.FingerprintSHA256(pub), Principal: principal,
					PublicKey: string(ssh.MarshalAuthorizedKey(pub))})
				seats[principal] = seat{run: run, priv: priv}
			}
			h := newSSHTestHarness(t, st, &sshFakeRunner{}, func(c *Config) {
				if sso {
					c.OIDC = &oidc.Authenticator{}
				} else {
					c.LocalMode, c.LocalOperator = true, "local:dev"
				}
			})
			for principal, s := range seats {
				client, err := sshDial(t, h, s.run.String(), s.priv)
				if client != nil {
					_ = client.Close()
				}
				if !sso {
					if err != nil {
						t.Errorf("%s's key into its own run: %v, want accepted", principal, err)
					}
					continue
				}
				if err == nil {
					t.Errorf("%s's key into its run was accepted under SSO, want refused", principal)
					continue
				}
				ev := waitForAudit(t, h.audit, s.run, "ssh.authenticate", "failure")
				if ev == nil || !strings.Contains(string(ev.Data), "reserved principal") {
					t.Errorf("%s: failure row = %+v, want one naming the reserved principal", principal, ev)
				}
			}
		})
	}
}
