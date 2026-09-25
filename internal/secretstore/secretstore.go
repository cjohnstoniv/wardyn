// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package secretstore defines the at-rest secret storage contract.
//
// Providers:
//   - pg: envelope-encrypted Postgres rows (default).
//   - vaultkv: store mode — the value lives in the organisation's Vault KV v2
//     (OpenBao is a supported endpoint) and the Postgres row is a pointer to it
//     (package vaultkv; credential-storage design §2.3a).
//   - azurekv: store mode in the organisation's Azure Key Vault (package
//     azurekv; design §2.3a.3).
//
// Secrets are late-bound: they are resolved at use time by the broker or
// injected proxy-side, so as a RULE no value lands in a sandbox's environment
// or disk. It is a rule with named, bounded exceptions, not an invariant — a
// credential that structurally cannot be handed over on the wire (no header to
// swap, no broker seam to mint through) has to go resident instead.
// ARCHITECTURE.md invariant 1 is the authoritative list of which credentials
// those are and what bounds each one. Deliberately NOT restated here: a second
// copy of that list is the thing that drifts out of date.
// Every read is audited once (credential-storage design §2.6): wardynd wraps
// the store in Audited, which records a secret.read for each Get with the
// purpose its caller put in the context (WithPurpose). The injection sinks
// record their own richer secret.read and mark the context SiteAudited so the
// decorator stays silent. cmd/wardynd's secret-read guard pins that every Get
// site does one or the other.
package secretstore

import (
	"context"
	"errors"
	"strings"
)

// ErrNotFound is the typed sentinel a Store.Get returns (wrapped) when no secret
// exists for the name, so callers can distinguish "never stored" from a backend
// error. Every Store implementation must honor it (the conformance suite checks).
var ErrNotFound = errors.New("secretstore: secret not found")

// ErrUnavailable marks a TRANSIENT failure: an external store sealed,
// throttled, 5xx, unreachable or timing out, or the database holding the rows
// not answering. The value may well be there; the store just could not answer. Every other Get error is DEFINITIVE — the row, the
// value, the binding or the access is gone, and retrying will not bring it
// back. A 401/403 is definitive by design, so revoking Wardyn's access at the
// store bites at once (design §2.3a.4, K8).
var ErrUnavailable = errors.New("secretstore: secret store unavailable")

// ErrRowNotWritten marks a store-mode Put whose value reached the external
// store while its row was not written (design rule 18). The caller audits it:
// depending on where the value landed, the store may already serve it.
var ErrRowNotWritten = errors.New("secretstore: the value reached the external store, but its row was not written")

type Store interface {
	Name() string
	Put(ctx context.Context, name string, value []byte) error
	// Get returns the plaintext. The context says why (WithPurpose) or that
	// the caller records the read itself (SiteAudited); see Audited.
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
//
// A ref names one object in the store, optionally followed by "#<version>"
// (azurekv counts the versions it wrote into the object). Two refs that
// differ only after the "#" name the same object: see RefObject.
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
	// Ref is the object DERIVED from (owner, name): where the row's value
	// must live, whatever the row records (-reconcile). A store whose object
	// names carry state no row can derive (Key Vault's generation) takes that
	// part from ref, and refuses a ref whose derivable part is not the row's.
	Ref(owner, name, ref string) (string, error)
	// Delete removes every version of the value behind ref. Idempotent.
	Delete(ctx context.Context, owner, name, ref string) error
	// Walk lists every value this install holds in the store (-reconcile).
	Walk(ctx context.Context) ([]ExternalEntry, error)
}

// PlatformNames are the boot keys wardynd mints and reads at every boot
// (cmd/wardynd loadOrCreateSecret). An external store files them under their
// own kind, so the org can audit, filter and (with a second identity)
// restrict them apart from people's credentials (design §2.13).
var PlatformNames = map[string]bool{
	"wardyn-signing-key":    true,
	"wardyn-session-key":    true,
	"wardyn-ui-session-key": true,
	"wardyn-ssh-host-key":   true,
}

// Kind is the store-side kind of the row (owner, name): "platform" for a boot
// key, "operator" for the rest of the operator namespace, "people" for every
// other owner.
func Kind(owner, name string) string {
	switch {
	case owner != "":
		return "people"
	case PlatformNames[name]:
		return "platform"
	default:
		return "operator"
	}
}

// RefObject is the object a ref names: the ref without its "#<version>".
func RefObject(ref string) string {
	obj, _, _ := strings.Cut(ref, "#")
	return obj
}

// DeleteReport is what an external store's Delete says about what it left
// behind (design §2.3a.3), for the caller's audit row: whether the value was
// purged, and if not, for how many days the organisation can still recover
// it (0: unknown).
type DeleteReport struct {
	Store           string
	Purged          bool
	RecoverableDays int
}

type deleteReportKey struct{}

// WithDeleteReport returns a context under which an external store's Delete
// fills the returned report. Store stays "" when nothing reported, which is
// every delete that never reached an external store.
func WithDeleteReport(ctx context.Context) (context.Context, *DeleteReport) {
	r := &DeleteReport{}
	return context.WithValue(ctx, deleteReportKey{}, r), r
}

// ReportDelete records r for the caller that asked with WithDeleteReport.
func ReportDelete(ctx context.Context, r DeleteReport) {
	if p, ok := ctx.Value(deleteReportKey{}).(*DeleteReport); ok {
		*p = r
	}
}

// ExternalEntry is one value found in an external store by Walk.
type ExternalEntry struct {
	Owner, Name, Ref string
	// SoftDeleted marks a value the store has deleted but can still recover
	// (Key Vault's soft delete), for RecoverableDays more days (0: unknown).
	SoftDeleted     bool
	RecoverableDays int
}
