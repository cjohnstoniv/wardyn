// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Per-kind credential minters (api_key / git_pat / ssh_key). Carved out of
// broker.go by seam (file-size gate); mint() and mintKind() still own the
// approval + audit invariants and dispatch here by GrantKind.

// ownerOf is the secretstore.Store.For namespace a mint resolves its secret
// from: the caller run's own identity Sub, or "" when caller is nil. An
// approval-path mint synthesizes run-scoped claims carrying the run's
// CreatedBy as Sub, so it resolves the same namespace MintForGrant's
// fully-populated auto-mint path does; an operator-created run's own Sub
// never collides with a member's stamped row (see secretOwnerFromRequest).
//
// SECURITY: this holds only because Sub is NOT caller-chosen — an arbitrary
// LocalMode X-Wardyn-Principal value picked to equal a member's OIDC sub
// would steer this very namespace, since pg.Store.Get's `ORDER BY
// (owned_by = $1) DESC` makes the named owner's row win over the operator's.
// api.runIdentitySubject mints the subject from the injected local principal,
// never the raw header, which carries attribution only.
func ownerOf(caller *identity.Claims) string {
	if caller == nil {
		return ""
	}
	return caller.Sub
}

// grantOwner is the namespace a grant's stored secret is read from: ownerOf,
// except that an owner_only grant on a run the operator itself owns (the
// signed Claims.OperatorOwned, never Sub) reads the operator's "", its own.
func grantOwner(caller *identity.Claims, spec types.GrantSpec) string {
	if spec.OwnerOnly && caller != nil && caller.OperatorOwned {
		return ""
	}
	return ownerOf(caller)
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
		Metadata:  map[string]string{"secret_name": sc.SecretName, "host": sc.Host},
		OwnerOnly: spec.OwnerOnly,
	}, nil
}

// mintGitPAT resolves a stored Personal Access Token and returns its VALUE to
// the git credential helper as username/password for a matched non-GitHub host.
//
// This is the OPPOSITE of mintAPIKey (whose value never leaves the broker; the
// proxy injects it header-side): git-over-HTTPS to ADO/GitLab is an opaque
// CONNECT tunnel the proxy cannot inject Basic-auth into without MITM, so the
// PAT must reach git through the helper — like the minted github_token. Fails
// closed on missing host/secret_name, a reserved secret name (defense-in-depth
// at the sink), or an unresolvable secret.
//
// HONESTY CEILING: ExpiresAt is only an emission/freshness window (ttlFor) —
// the PAT itself is a long-lived, operator-managed secret Wardyn cannot expire
// or down-scope; per-use revocation would need the host's own token API
// (ADO/GitLab), out of scope. The returned Token is masked from PTY/asciicast
// by mint()'s maskReg.Add.
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
	// Own owner's row wins, falling back to the operator's unless owner_only.
	gctx, row := secretstore.GrantRead(ctx, spec.OwnerOnly)
	value, err := b.secrets.For(grantOwner(caller, spec)).Get(secretstore.WithPurpose(gctx, secretstore.PurposeBrokerMint), sc.SecretName)
	if err != nil {
		return Minted{}, grantReadError("git_pat", sc.SecretName, spec.OwnerOnly, err)
	}
	return Minted{
		Kind:      types.GrantGitPAT,
		JTI:       newJTI(),
		ExpiresAt: time.Now().Add(ttlFor(spec)),
		Token:     string(value),
		Username:  gitPATUsername(sc.Host, sc.Username),
		Metadata:  map[string]string{"secret_name": sc.SecretName, "host": sc.Host, "secret_scope": row.Scope()},
	}, nil
}

// mintSSHKey resolves a stored SSH PRIVATE KEY and returns its VALUE (plus, when
// named, the known_hosts material) to agent-run for a git-over-SSH clone.
//
// SECURITY EXCEPTION (mirrors mintGitPAT's honesty ceiling): git's SSH
// transport has no credential-helper seam (git credential.helper is
// HTTP-only), so — unlike git_pat (goes to the helper, never on disk) or
// api_key (never leaves the broker) — an SSH key cannot be brokered without
// becoming resident: the ssh client reads it from a file. The key material is
// returned here and agent-run writes it 0400, agent-owned, then wipes it right
// after the clone (deploy/images/*/agent-run). The readable window is the
// clone only, but within it code running as the agent uid can read the key —
// the same residual as WARDYN_GIT_HELPER_SECRET. This is the accepted
// exception the owner chose enabling the SSH SCM lane; there is no way to
// down-scope or expire an SSH private key from Wardyn's side (out of scope,
// host SSH-key API). The returned Token is mask-registered by mint()'s
// maskReg.Add.
//
// Fails closed on missing host/key_secret_ref, a reserved secret name
// (defense-in-depth at the sink), an unresolvable key secret, or an
// unresolvable known_hosts secret when one was named.
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
	owned := b.secrets.For(grantOwner(caller, spec))
	gctx, row := secretstore.GrantRead(ctx, spec.OwnerOnly)
	rctx := secretstore.WithPurpose(gctx, secretstore.PurposeBrokerMint)
	key, err := owned.Get(rctx, sc.KeySecretRef)
	if err != nil {
		return Minted{}, grantReadError("ssh_key", sc.KeySecretRef, spec.OwnerOnly, err)
	}
	keyScope := row.Scope()
	// Optional operator-supplied known_hosts for a custom host the image-baked
	// /etc/ssh/ssh_known_hosts does not cover; normally unset for github.com/ADO.
	var knownHosts string
	if sc.KnownHostsSecretRef != "" {
		kh, kerr := owned.Get(rctx, sc.KnownHostsSecretRef)
		if kerr != nil {
			return Minted{}, grantReadError("ssh_key known_hosts", sc.KnownHostsSecretRef, spec.OwnerOnly, kerr)
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
		Metadata:   map[string]string{"key_secret_ref": sc.KeySecretRef, "host": sc.Host, "secret_scope": keyScope},
	}, nil
}

// grantReadError names why a grant's stored secret could not be read. An
// owner_only grant with no row of its owner's own gets its own sentence: the
// operator's row of that name, if any, was deliberately not consulted.
func grantReadError(kind, name string, ownerOnly bool, err error) error {
	if ownerOnly && errors.Is(err, secretstore.ErrNotFound) {
		return fmt.Errorf("broker: %s grant is owner_only and the run's owner has no secret %q of their own "+
			"(an operator secret of that name is never used for it): %w", kind, name, err)
	}
	return fmt.Errorf("broker: read %s secret %q: %w", kind, name, err)
}

// withSecretScope records on a credential.mint row whose row the stored
// secret came from ("own" or "operator"), so an operator-row fallback is
// visible on the mint itself. A kind that read no stored secret adds nothing.
func withSecretScope(data json.RawMessage, m Minted) json.RawMessage {
	if sc := m.Metadata["secret_scope"]; sc != "" {
		return withDataField(data, "secret_scope", sc)
	}
	return data
}

// withDataField sets one field of an event's Data, returning data unchanged
// when it does not decode.
func withDataField(data json.RawMessage, key string, v any) json.RawMessage {
	var d map[string]any
	if err := json.Unmarshal(data, &d); err != nil {
		return data
	}
	d[key] = v
	out, err := json.Marshal(d)
	if err != nil {
		return data
	}
	return out
}
