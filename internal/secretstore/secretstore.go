// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package secretstore defines the at-rest secret storage contract.
//
// Providers:
//   - pg: envelope-encrypted Postgres rows (default).
//   - vaultkv: store mode — the value lives in the organisation's Vault KV v2
//     (OpenBao is a supported endpoint) and the Postgres row is a pointer to it
//     (package vaultkv; credential-storage design §2.3a).
//
// Secrets are late-bound: they are resolved at use time by the broker or
// injected proxy-side, so as a RULE no value lands in a sandbox's environment
// or disk. It is a rule with named, bounded exceptions, not an invariant — a
// credential that structurally cannot be handed over on the wire (no header to
// swap, no broker seam to mint through) has to go resident instead.
// ARCHITECTURE.md invariant 1 is the authoritative list of which credentials
// those are and what bounds each one. Deliberately NOT restated here: a second
// copy of that list is the thing that drifts out of date.
// Audit coverage of reads is partial and honestly bounded: the proxy-side
// api_key injection path emits a dedicated secret.read event
// (internal/api/injection.go); the broker's git_pat and GitHub App private-key
// reads are audited via the higher-level credential.mint event instead of an
// individual secret.read (see internal/broker); and boot-time reads — the
// platform signing/session keys (cmd/wardynd) — are currently NOT audited. Do
// not treat "every read is an audit event" as a guarantee.
package secretstore

import (
	"context"
	"errors"
)

// ErrNotFound is the typed sentinel a Store.Get returns (wrapped) when no secret
// exists for the name, so callers can distinguish "never stored" from a backend
// error. Every Store implementation must honor it (the conformance suite checks).
var ErrNotFound = errors.New("secretstore: secret not found")

// ErrUnavailable marks a TRANSIENT failure of an external store (sealed,
// throttled, 5xx, network, timeout): the value may well be there, the store
// just could not answer. Every other Get error is DEFINITIVE — the row, the
// value, the binding or the access is gone, and retrying will not bring it
// back. A 401/403 is definitive by design, so revoking Wardyn's access at the
// store bites at once (design §2.3a.4, K8).
var ErrUnavailable = errors.New("secretstore: secret store unavailable")

type Store interface {
	Name() string
	Put(ctx context.Context, name string, value []byte) error
	// Get returns the plaintext. Callers are responsible for emitting the
	// corresponding audit event before using the value.
	Get(ctx context.Context, name string) ([]byte, error)
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]string, error)
	// For returns a view of the store scoped to owner, the per-principal
	// namespace introduced by migration 0050 (member BYOK). owner "" is the
	// OPERATOR namespace — the zero value of every existing caller, so a call
	// site that never invokes For is unaffected by this seam's existence.
	//
	// The four methods above behave differently under a non-"" owner:
	//   - Get first tries the owner's own row, then FALLS BACK to the
	//     operator's ("") row — a member with no key of their own still
	//     resolves the operator's, exactly as before For existed.
	//   - Put and Delete are scoped to the owner's row ONLY. They never read
	//     or write the operator's row, and never fall back — a write always
	//     means what it says.
	//   - List returns the owner's OWN rows only, never unioned with the
	//     operator's. A caller that wants "everything a principal may see"
	//     composes it itself: For("").List() ∪ For(owner).List().
	// This is a real backend implementation, not policy: a plugged-in
	// alternate (OpenBao, KMS) implements the same fallback/isolation
	// contract, held to it by the shared conformance suite.
	For(owner string) Store
}

// External is a store-mode backend (design §2.3a): the value lives in the
// organisation's secret manager, and the Postgres row is a pointer to it
// (enc_version 2, kek_id "<Name()>:<ref>"). The pg store owns the row, the
// owner fallback and the ordering (external first on Put and Delete); an
// External only moves bytes to and from the store and checks the binding.
type External interface {
	// Name is the registered store name and the kek_id prefix ("vaultkv").
	Name() string
	// Describe names the store for an operator ("Vault at vault.example:8200").
	Describe() string
	// Put writes value for the row (owner, name) and returns the ref the row
	// records. prev is the row's current ref ("" if none). createOnly refuses
	// to overwrite a value that is already there (the migrator's guard against
	// racing a concurrent Put).
	Put(ctx context.Context, owner, name, prev string, value []byte, createOnly bool) (ref string, err error)
	// Get reads the value a pointer row names. It refuses a ref that is not the
	// one derived from (owner, name), and a value whose store-side owner/name
	// differs from the row. A value that is absent is a definitive refusal,
	// never ErrNotFound: the row exists, so the credential was lost.
	Get(ctx context.Context, owner, name, ref string) ([]byte, error)
	// Check reports whether the value behind ref exists and is bound to
	// (owner, name), without reading it (-reconcile).
	Check(ctx context.Context, owner, name, ref string) error
	// Ref is the ref DERIVED from (owner, name): where the row's value must
	// live, whatever the row records (-reconcile).
	Ref(owner, name string) (string, error)
	// Delete removes every version of the value behind ref. Idempotent.
	Delete(ctx context.Context, owner, name, ref string) error
	// Walk lists every value this install holds in the store (-reconcile).
	Walk(ctx context.Context) ([]ExternalEntry, error)
}

// ExternalEntry is one value found in an external store by Walk.
type ExternalEntry struct {
	Owner, Name, Ref string
}
