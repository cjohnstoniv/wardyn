// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package secretstore defines the at-rest secret storage contract.
//
// Providers:
//   - pg: envelope-encrypted Postgres rows (default).
//   - vaultkv: store mode — the value lives in the organisation's Vault KV v2
//     (OpenBao supported) and the Postgres row is a pointer to it.
//   - azurekv: store mode in the organisation's Azure Key Vault.
//
// In pg, the key wrapping each row's data key is its own seam (package kek,
// selected by WARDYN_KEK): the local key derived from WARDYN_AGE_KEY, or
// Vault Transit.
//
// SECURITY: secrets are late-bound — resolved at use time by the broker or
// injected proxy-side, so as a RULE no value lands in a sandbox's environment
// or disk. Named, bounded exceptions exist for a credential that structurally
// cannot be handed over on the wire; ARCHITECTURE.md invariant 1 is the
// authoritative list, deliberately not restated here.
//
// Every read is audited once: wardynd wraps the store in Audited, which
// records a secret.read for each Get with the purpose from context
// (WithPurpose). Injection sinks record their own richer secret.read and mark
// the context SiteAudited so the decorator stays silent; cmd/wardynd's
// secret-read guard pins that every Get site does one or the other.
package secretstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is the typed sentinel a Store.Get returns (wrapped) when no
// secret exists for the name, distinguishing "never stored" from a backend
// error. Every Store implementation must honor it.
var ErrNotFound = errors.New("secretstore: secret not found")

// ErrUnavailable marks a TRANSIENT failure: an external store sealed,
// throttled, 5xx, unreachable, timing out, or the database not answering —
// the value may well be there. Every other Get error is DEFINITIVE (row,
// value, binding, or access is gone; retrying won't help). A 401/403 is
// definitive by design, so revoking Wardyn's access at the store bites at once.
var ErrUnavailable = errors.New("secretstore: secret store unavailable")

// ErrRowNotWritten marks a store-mode Put whose value reached the external
// store while its row was not written. The caller audits it: depending on
// where the value landed, the store may already serve it.
var ErrRowNotWritten = errors.New("secretstore: the value reached the external store, but its row was not written")

type Store interface {
	Name() string
	Put(ctx context.Context, name string, value []byte) error
	// Get returns the plaintext. The context says why (WithPurpose) or that
	// the caller records the read itself (SiteAudited); see Audited.
	Get(ctx context.Context, name string) ([]byte, error)
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]string, error)
	// DeleteEverywhere removes every namespace's row of each name — the
	// operator's and every principal's — and returns how many rows it
	// removed. The one cross-owner write: a model provider whose address
	// changes must take every person's credential for it with it, and no
	// caller knows every owner to Delete them one by one.
	DeleteEverywhere(ctx context.Context, names []string) (int, error)
	// Holders is DeleteEverywhere's read twin: for each of names, every
	// namespace holding a row of it (the operator's "" included). A name
	// nobody holds is absent. Reads rows, never a value, so a store-mode
	// backend's external store is not asked.
	Holders(ctx context.Context, names []string) (map[string][]string, error)
	// For returns a view of the store scoped to owner, the per-principal
	// namespace introduced by migration 0050 (member BYOK). owner "" is the
	// OPERATOR namespace — the zero value of every existing caller, so a call
	// site that never invokes For is unaffected.
	//
	// The four methods above behave differently under a non-"" owner:
	//   - Get first tries the owner's own row, then FALLS BACK to the
	//     operator's ("") row. A read under GrantRead(ctx, true) (owner_only)
	//     never falls back.
	//   - Put and Delete are scoped to the owner's row ONLY, never falling
	//     back — a write always means what it says.
	//   - List returns the owner's OWN rows only, never unioned with the
	//     operator's; a caller wanting everything composes
	//     For("").List() ∪ For(owner).List() itself.
	// Real backend implementation, not policy: a plugged-in alternate (a
	// store-mode backend, a KEK) keeps the same contract, held to it by the
	// shared conformance suite.
	For(owner string) Store
}

// External is a store-mode backend: the value lives in the organisation's
// secret manager, and the Postgres row is a pointer to it (enc_version 2,
// kek_id "<Name()>:<ref>"). The pg store owns the row, the owner fallback and
// the ordering (external first on Put and Delete); an External only moves
// bytes to and from the store and checks the binding.
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
	// to overwrite a value already there (the migrator's guard against a
	// racing concurrent Put).
	Put(ctx context.Context, owner, name, prev string, value []byte, createOnly bool) (ref string, err error)
	// Get reads the value a pointer row names, refusing a ref not derived from
	// (owner, name) or a value whose store-side owner/name differs from the
	// row. An absent value is a definitive refusal, never ErrNotFound: the
	// row exists, so the credential was lost.
	Get(ctx context.Context, owner, name, ref string) ([]byte, error)
	// Check reports whether the value behind ref exists and is bound to
	// (owner, name), without reading it.
	Check(ctx context.Context, owner, name, ref string) error
	// Ref is the object DERIVED from (owner, name): where the row's value
	// must live, whatever the row records. A store whose object names carry
	// state no row can derive (Key Vault's generation) takes that part from
	// ref, refusing a ref whose derivable part isn't the row's.
	Ref(owner, name, ref string) (string, error)
	// Delete removes every version of the value behind ref. Idempotent.
	Delete(ctx context.Context, owner, name, ref string) error
	// Walk lists every value this install holds in the store.
	Walk(ctx context.Context) ([]ExternalEntry, error)
}

// PlatformNames are the boot keys wardynd mints and reads at every boot
// (cmd/wardynd loadOrCreateSecret). An external store files them under their
// own kind, so the org can audit, filter and (with a second identity)
// restrict them apart from people's credentials; local mode wraps them under
// a KEK of their own. cmd/wardynd's TestBootKeysAreThePlatformSet derives the
// boot keys from the loadOrCreateSecret call sites and fails if this map differs.
var PlatformNames = map[string]bool{
	"wardyn-signing-key":    true,
	"wardyn-session-key":    true,
	"wardyn-ui-session-key": true,
	"wardyn-ssh-host-key":   true,
	"wardyn-internal-ca":    true,
	// Seals each run's stored proxy config (cmd/wardynd's loadOrCreateRunConfigKey).
	"wardyn-run-config-key": true,
	// The hybrid laptop's org device credential (cmd/wardynd's bootHybrid).
	"wardyn-org-device-credential": true,
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
// behind, for the caller's audit row: whether the value was purged, and if
// not, for how many days the organisation can still recover it (0: unknown).
type DeleteReport struct {
	Store           string
	Purged          bool
	RecoverableDays int
	// PrincipalKey marks a row sealed under its owner's principal key
	// (enc_version 3): deleting it is a crypto-erasure only once that key is
	// destroyed (EraseOwner does, after the rows).
	PrincipalKey bool
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

type expiryKey struct{}

// WithExpiry returns a context under which a Put records at as the latest time
// the value can still be used or renewed (the row's expires_at): the daily
// sweep deletes it after that. A Put without it records no expiry, so a
// replace always describes the value it wrote.
func WithExpiry(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, expiryKey{}, at)
}

// ExpiryFrom is the expiry WithExpiry put on ctx, if any.
func ExpiryFrom(ctx context.Context) (time.Time, bool) {
	at, ok := ctx.Value(expiryKey{}).(time.Time)
	return at, ok && !at.IsZero()
}

// Expired names one row a sweep deleted because its expiry had passed.
type Expired struct {
	Owner, Name string
	ExpiresAt   time.Time
}

// ExpiredKept is one row a sweep found expired but could not delete. The
// sweep's error joins one per such row, so its caller can name each.
type ExpiredKept struct {
	Owner, Name string
	Err         error
}

func (e *ExpiredKept) Error() string {
	return fmt.Sprintf("expired (owned_by=%q, name=%q) kept: %v", e.Owner, e.Name, e.Err)
}

func (e *ExpiredKept) Unwrap() error { return e.Err }

// EraseReport is what EraseOwner removed.
type EraseReport struct {
	// Count is how many credentials were deleted.
	Count int
	// Store, Purged and RecoverableDays aggregate the external store's
	// DeleteReports: Store is "" when nothing reported, Purged is false when
	// any value was left recoverable, RecoverableDays is the longest window.
	Store           string
	Purged          bool
	RecoverableDays int
	// CryptoErased is how many of Count were sealed under the person's
	// principal key and are unreadable now that the key is destroyed. The rest
	// of Count (v1 rows, rows written with principal keys off, external pointer
	// rows) were only deleted, which holds to the backup horizon.
	CryptoErased int
}

// CredentialKeyDestroyer is a store that seals credentials under per-person
// keys (the pg store). EraseOwner calls it once an owner's rows are gone.
type CredentialKeyDestroyer interface {
	// DestroyCredentialKey destroys every generation of owner's credential key
	// and returns the generations it destroyed (none when there was no live
	// key). Idempotent.
	DestroyCredentialKey(ctx context.Context, owner string) ([]int, error)
}

// ErrOperatorNamespace refuses an erase of the operator namespace (""): it
// holds the platform keys and the deployment's shared credentials, not one
// person's.
var ErrOperatorNamespace = errors.New("secretstore: the operator namespace is not a person's and cannot be erased")

// EraseOwner deletes every credential in owner's own namespace. Each Delete
// removes the external value before the row, so a failure keeps the row and a
// retry resumes. When the store seals credentials under per-person keys
// (CredentialKeyDestroyer) it then destroys the owner's key and reports the
// rows that were under it as crypto-erased. It never reports success with a row left behind: every
// failure is returned, and a namespace not empty afterwards (a write racing
// the erase) is an error naming how many remain.
//
// The re-list is a backstop, not the coordination: it cannot see a write that
// lands after it. The caller (internal/api, handleErasePersonCredentials) holds
// the locks that serialise the writers of a stored credential across the
// delete and this re-list, so a refresh or stamp already in flight cannot
// write back behind a success.
func EraseOwner(ctx context.Context, st Store, owner string) (EraseReport, error) {
	rep := EraseReport{Purged: true}
	if owner == "" {
		return rep, ErrOperatorNamespace
	}
	view := st.For(owner)
	names, err := view.List(ctx)
	if err != nil {
		return rep, fmt.Errorf("list %q: %w", owner, err)
	}
	var errs []error
	underKey := 0
	for _, n := range names {
		dctx, dr := WithDeleteReport(ctx)
		if err := view.Delete(dctx, n); err != nil {
			errs = append(errs, err)
			continue
		}
		rep.Count++
		if dr.PrincipalKey {
			underKey++
		}
		if dr.Store != "" {
			rep.Store = dr.Store
			rep.Purged = rep.Purged && dr.Purged
			rep.RecoverableDays = max(rep.RecoverableDays, dr.RecoverableDays)
		}
	}
	if len(errs) == 0 {
		left, err := view.List(ctx)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("re-list %q: %w", owner, err))
		case len(left) > 0:
			errs = append(errs, fmt.Errorf("%d credentials of %q were written while the erase ran; erase again", len(left), owner))
		}
	}
	if d, ok := st.(CredentialKeyDestroyer); ok && len(errs) == 0 {
		if _, err := d.DestroyCredentialKey(ctx, owner); err != nil {
			errs = append(errs, fmt.Errorf("destroy the credential key of %q (its rows are deleted; erase again to retry): %w", owner, err))
		} else {
			rep.CryptoErased = underKey
		}
	}
	return rep, errors.Join(errs...)
}
