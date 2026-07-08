// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package secretstore defines the at-rest secret storage contract.
//
// Providers:
//   - pg: age-encrypted Postgres column (default).
//   - openbao: OpenBao KV + leases (v0.5).
//
// Secrets are late-bound: they are resolved at use time by the broker or
// injected proxy-side. They never enter a sandbox's environment or disk.
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
}
