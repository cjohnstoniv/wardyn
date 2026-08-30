// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"time"
)

// Per-kind credential minters (api_key / git_pat / ssh_key). Carved out of
// broker.go by seam (file-size gate); mint() and mintKind() still own the
// approval + audit invariants and dispatch here by GrantKind.

// ownerOf is the secretstore.Store.For namespace a mint resolves its secret
// from: the caller run's own identity Sub, or "" when caller is nil.
// An approval-path mint synthesizes run-scoped claims carrying the run's
// CreatedBy as Sub, so an approval-gated git_pat/ssh_key mint reached that way
// resolves the same namespace the auto-mint path (MintForGrant's
// fully-populated caller) does.
// An operator-created run's own Sub never collides with a member's stamped
// row (secretOwnerFromRequest's own doc comment).
func ownerOf(caller *identity.Claims) string {
	if caller == nil {
		return ""
	}
	return caller.Sub
}

// mintAPIKey resolves the secret NAME to a proxy InjectionRule. The secret
// VALUE is never read or returned here — late binding happens proxy-side, at
// egress time, by name. This is intentional late binding, not an oversight:
// existence is checked earlier, at create time, by validateInlineSecretRefs
// (internal/api/inline_policy.go) for both the inline and stored/default
// policy paths; mintAPIKey itself does NOT re-check existence here.
func (b *Broker) mintAPIKey(spec types.GrantSpec) (Minted, error) {
	var sc apiKeyScope
	if err := json.Unmarshal(spec.Scope, &sc); err != nil {
		return Minted{}, fmt.Errorf("broker: decode api_key scope: %w", err)
	}
	if sc.Host == "" || sc.SecretName == "" {
		return Minted{}, errors.New("broker: api_key scope requires host and secret_name")
	}
	format := sc.Format
	if format == "" {
		format = "Bearer %s"
	}
	header := sc.Header
	if header == "" {
		header = "Authorization"
	}
	return Minted{
		Kind:      types.GrantAPIKey,
		JTI:       newJTI(),
		ExpiresAt: time.Now().Add(ttlFor(spec)),
		Injection: &egress.InjectionRule{
			Host:       sc.Host,
			Header:     header,
			SecretName: sc.SecretName,
			Format:     format,
		},
		Metadata: map[string]string{"secret_name": sc.SecretName, "host": sc.Host},
	}, nil
}

// mintGitPAT resolves a stored Personal Access Token and returns its VALUE to
// the git credential helper as username/password for a matched non-GitHub host.
//
// This is the OPPOSITE of mintAPIKey (whose secret value never leaves the
// broker; the proxy injects it header-side): git-over-HTTPS to ADO/GitLab is an
// opaque CONNECT tunnel the proxy cannot inject Basic-auth into without MITM, so
// the PAT MUST reach git through the helper — exactly like the minted
// github_token. Fails closed on missing host/secret_name, a reserved secret
// name (defense-in-depth at the sink), or an unresolvable secret.
//
// ExpiresAt is only an emission/freshness window (ttlFor) — the PAT
// itself is a long-lived, operator-managed secret that Wardyn CANNOT expire or
// down-scope; per-use revocation/scoping would need the host's token API
// (ADO/GitLab), out of scope. This is the honesty ceiling for this grant kind.
// The returned Token is masked from PTY/asciicast by the maskReg.Add in mint().
func (b *Broker) mintGitPAT(ctx context.Context, caller *identity.Claims, spec types.GrantSpec) (Minted, error) {
	var sc gitPATScope
	if err := json.Unmarshal(spec.Scope, &sc); err != nil {
		return Minted{}, fmt.Errorf("broker: decode git_pat scope: %w", err)
	}
	if sc.Host == "" || sc.SecretName == "" {
		return Minted{}, errors.New("broker: git_pat scope requires host and secret_name")
	}
	if reservedBrokerSecret(sc.SecretName) {
		return Minted{}, fmt.Errorf("broker: git_pat secret name %q is reserved for platform internals", sc.SecretName)
	}
	if b.secrets == nil {
		return Minted{}, errors.New("broker: git_pat grant but no secret store configured (fail closed)")
	}
	// The run's own owner's row wins, falling back to the operator's (ownerOf).
	value, err := b.secrets.For(ownerOf(caller)).Get(ctx, sc.SecretName)
	if err != nil {
		return Minted{}, fmt.Errorf("broker: read git_pat secret %q: %w", sc.SecretName, err)
	}
	return Minted{
		Kind:      types.GrantGitPAT,
		JTI:       newJTI(),
		ExpiresAt: time.Now().Add(ttlFor(spec)),
		Token:     string(value),
		Username:  gitPATUsername(sc.Host, sc.Username),
		Metadata:  map[string]string{"secret_name": sc.SecretName, "host": sc.Host},
	}, nil
}

// mintSSHKey resolves a stored SSH PRIVATE KEY and returns its VALUE (plus, when
// named, the known_hosts material) to agent-run for a git-over-SSH clone.
//
// SECURITY EXCEPTION (documented honestly, mirrors mintGitPAT's honesty ceiling):
// git's SSH transport has NO credential-helper seam (git credential.helper is
// HTTP-only), so — unlike git_pat (returned to the helper, never on disk) or
// api_key (never leaves the broker; the proxy injects it) — an SSH key CANNOT be
// brokered without becoming resident: the ssh client reads it from a file. So the
// key material is returned here and agent-run writes it 0400, agent-owned, then
// WIPES it right after the clone (deploy/images/*/agent-run). The readable window
// is the clone only, but within it code running AS the agent uid can read the key
// — the same residual as WARDYN_GIT_HELPER_SECRET. This is the accepted
// exception the owner chose when enabling the SSH SCM lane; there is no way to
// down-scope or expire an SSH private key from Wardyn's side (out of scope, host
// SSH-key API). The returned Token is mask-registered by mint()'s maskReg.Add.
//
// Fails closed on missing host/key_secret_ref, a reserved secret name (defense-
// in-depth at the sink), an unresolvable key secret, or an unresolvable
// known_hosts secret when one was named.
func (b *Broker) mintSSHKey(ctx context.Context, caller *identity.Claims, spec types.GrantSpec) (Minted, error) {
	var sc sshKeyScope
	if err := json.Unmarshal(spec.Scope, &sc); err != nil {
		return Minted{}, fmt.Errorf("broker: decode ssh_key scope: %w", err)
	}
	if sc.Host == "" || sc.KeySecretRef == "" {
		return Minted{}, errors.New("broker: ssh_key scope requires host and key_secret_ref")
	}
	if reservedBrokerSecret(sc.KeySecretRef) || reservedBrokerSecret(sc.KnownHostsSecretRef) {
		return Minted{}, fmt.Errorf("broker: ssh_key secret name is reserved for platform internals")
	}
	if b.secrets == nil {
		return Minted{}, errors.New("broker: ssh_key grant but no secret store configured (fail closed)")
	}
	// Same owner-then-operator-fallback rule as mintGitPAT, for both the key
	// and its optional known_hosts material below.
	owned := b.secrets.For(ownerOf(caller))
	key, err := owned.Get(ctx, sc.KeySecretRef)
	if err != nil {
		return Minted{}, fmt.Errorf("broker: read ssh_key secret %q: %w", sc.KeySecretRef, err)
	}
	// Optional operator-supplied known_hosts (for a custom host the image-baked
	// /etc/ssh/ssh_known_hosts does not cover). For github.com / ADO the baked file
	// is authoritative and this ref is normally unset.
	var knownHosts string
	if sc.KnownHostsSecretRef != "" {
		kh, kerr := owned.Get(ctx, sc.KnownHostsSecretRef)
		if kerr != nil {
			return Minted{}, fmt.Errorf("broker: read ssh_key known_hosts secret %q: %w", sc.KnownHostsSecretRef, kerr)
		}
		knownHosts = string(kh)
	}
	username := sc.Username
	if username == "" {
		username = "git" // github.com and ssh.dev.azure.com both authenticate as user "git"
	}
	return Minted{
		Kind:       types.GrantSSHKey,
		JTI:        newJTI(),
		ExpiresAt:  time.Now().Add(ttlFor(spec)),
		Token:      string(key),
		Username:   username,
		KnownHosts: knownHosts,
		Metadata:   map[string]string{"key_secret_ref": sc.KeySecretRef, "host": sc.Host},
	}, nil
}
