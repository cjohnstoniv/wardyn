// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package secretstore defines the at-rest secret storage contract.
//
// Providers:
//   - pg: age-encrypted Postgres column (default).
//   - openbao: OpenBao KV + leases (v1.0).
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
// composer LLM API key (cmd/wardynd/composer.go) and the platform
// signing/session keys (cmd/wardynd) — are currently NOT audited. Do not treat
// "every read is an audit event" as a guarantee.
package secretstore

import (
	"context"
	"errors"
)

// ErrNotFound is the typed sentinel a Store.Get returns (wrapped) when no secret
// exists for the name, so callers can distinguish "never stored" from a backend
// error. Every Store implementation must honor it (the conformance suite checks).
var ErrNotFound = errors.New("secretstore: secret not found")

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
