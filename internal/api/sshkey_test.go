// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func sshKeyPolicy(host, keyRef string) types.RunPolicySpec {
	return types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants: []types.GrantSpec{{
			Kind:  types.GrantSSHKey,
			Scope: mustJSON(map[string]any{"host": host, "key_secret_ref": keyRef}),
		}},
	}
}

// TestSSHOver443Endpoint asserts the clone-URL rewrite mapping: the supported SCM
// hosts rewrite to their SSH-over-443 endpoint (PORT-QUALIFIED :443, so the
// egress entry matches ONLY :443), and unsupported hosts are refused.
func TestSSHOver443Endpoint(t *testing.T) {
	cases := []struct {
		host   string
		wantEP string
		wantOK bool
	}{
		{"github.com", "ssh.github.com:443", true},
		{"ssh.github.com", "ssh.github.com:443", true},
		{"GitHub.com", "ssh.github.com:443", true},  // case-insensitive
		{"github.com.", "ssh.github.com:443", true}, // trailing dot tolerated
		{"dev.azure.com", "ssh.dev.azure.com:443", true},
		{"ssh.dev.azure.com", "ssh.dev.azure.com:443", true},
		{"gitlab.com", "", false},         // unsupported (no published :443 SSH)
		{"ghes.corp.internal", "", false}, // custom GHES: out of scope for SSH
		{"", "", false},
	}
	for _, c := range cases {
		ep, ok := sshOver443Endpoint(c.host)
		if ok != c.wantOK || ep != c.wantEP {
			t.Errorf("sshOver443Endpoint(%q) = (%q,%v), want (%q,%v)", c.host, ep, ok, c.wantEP, c.wantOK)
		}
	}
}

// TestSSHKeyScopeFields asserts the scope decoder returns the fields and fails
// closed on a missing host or key_secret_ref.
func TestSSHKeyScopeFields(t *testing.T) {
	host, keyRef, user, khRef, err := sshKeyScopeFields(mustJSON(map[string]any{
		"host": "github.com", "key_secret_ref": "gh-key", "username": "git", "known_hosts_secret_ref": "kh",
	}))
	if err != nil || host != "github.com" || keyRef != "gh-key" || user != "git" || khRef != "kh" {
		t.Fatalf("decode = (%q,%q,%q,%q,%v), want github.com/gh-key/git/kh/nil", host, keyRef, user, khRef, err)
	}
	if _, _, _, _, err := sshKeyScopeFields(mustJSON(map[string]any{"host": "github.com"})); err == nil {
		t.Fatal("missing key_secret_ref: expected error")
	}
	if _, _, _, _, err := sshKeyScopeFields(mustJSON(map[string]any{"key_secret_ref": "k"})); err == nil {
		t.Fatal("missing host: expected error")
	}
}

// TestValidatePolicySpec_SSHKey asserts the write-time invariants for ssh_key: a
// valid grant passes; empty host, empty key_secret_ref, an unsupported host, and
// a reserved secret name (key or known_hosts) are rejected (fail closed).
func TestValidatePolicySpec_SSHKey(t *testing.T) {
	if err := validatePolicySpec(sshKeyPolicy("github.com", "gh-ssh-key")); err != nil {
		t.Fatalf("valid ssh_key grant rejected: %v", err)
	}

	khReserved := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		EligibleGrants: []types.GrantSpec{{
			Kind: types.GrantSSHKey,
			Scope: mustJSON(map[string]any{
				"host": "github.com", "key_secret_ref": "gh-key", "known_hosts_secret_ref": "wardyn-signing-key",
			}),
		}},
	}

	bad := []struct {
		name string
		spec types.RunPolicySpec
	}{
		{"empty-host", sshKeyPolicy("", "gh-ssh-key")},
		{"empty-key-ref", sshKeyPolicy("github.com", "")},
		{"unsupported-host", sshKeyPolicy("gitlab.com", "gl-key")},
		{"reserved-key-secret", sshKeyPolicy("github.com", "wardyn-signing-key")},
		{"reserved-known-hosts", khReserved},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if err := validatePolicySpec(c.spec); err == nil {
				t.Fatalf("%s: expected validatePolicySpec to reject, got nil", c.name)
			}
		})
	}
}

// TestValidateInlineSecretRefs_SSHKey asserts an ssh_key grant referencing an
// unknown key secret is rejected at create time (422), and a present one passes.
func TestValidateInlineSecretRefs_SSHKey(t *testing.T) {
	h, _ := newSecretsHarness(t) // memSecrets seeded with "anthropic-api-key"
	ctx := context.Background()

	present := sshKeyPolicy("github.com", "anthropic-api-key") // reuse the seeded name
	if code, err := h.srv.validateInlineSecretRefs(ctx, present); err != nil || code != 0 {
		t.Fatalf("present ssh_key secret: code=%d err=%v, want (0,nil)", code, err)
	}

	missing := sshKeyPolicy("github.com", "no-such-key")
	if code, err := h.srv.validateInlineSecretRefs(ctx, missing); err == nil || code != http.StatusUnprocessableEntity {
		t.Fatalf("missing ssh_key secret: code=%d err=%v, want (422,err)", code, err)
	}
}
