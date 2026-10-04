// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package pg implements secretstore.Store over the Postgres `secrets` table,
// one envelope-encrypted row per credential (envelope v1): every Put mints a
// fresh 32-byte data key (DEK), seals the value under it with AES-256-GCM
// bound to the row's (owned_by, name), and stores the DEK wrapped by a
// key-encryption key (package kek). The row records which KEK wrapped it
// (kek_id); a read dispatches on enc_version and kek_id.
//
// A person's row may instead be envelope enc_version 3 (principal_rows.go): the
// same sealed value, with the DEK wrapped under the owner's principal key, so
// destroying that key erases the row.
//
// SECURITY: the plaintext and the DEK are only in memory during the Put/Get
// call, and no error carries either. Reads are recorded by the
// secretstore.Audited decorator this store is wrapped in.
package pg

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

var _ secretstore.Store = (*Store)(nil)

// encVersion is the row format every local write produces. 0 is the legacy
// age payload, which only the boot conversion (ConvertV0) reads; extVersion is
// the store-mode pointer row (external.go).
const encVersion = 1

// pkVersion is the credential row sealed under its owner's principal key, whose
// data key is not under any root KEK: the `secrets` rewrap passes skip it, and
// subjectkey.Rewrap moves the principal keys instead.
const pkVersion = 3

// ageHeader opens every age payload, and so every row a pre-envelope wardynd
// writes.
var ageHeader = []byte("age-encryption.org/v1\n")

// unknownVersion refuses a row format this binary predates: a newer wardynd
// wrote it (a mixed-version window, or a rollback). It is never read as
// not-found and never handed to the age path — only ConvertV0 reads age, and
// only enc_version 0.
const unknownVersion = "has enc_version %d which this wardynd does not understand; upgrade wardynd"

// secretAADLabel is the domain label of AAD_secret, the value's binding.
const secretAADLabel = "wardyn/secret/v1"

// Store is an envelope-encrypted, Postgres-backed secret store.
// The zero value is unusable; use New.
type Store struct {
	pool *pgxpool.Pool
	// kek wraps the DEK of every credential row this store writes; platform
	// wraps every boot key, unless the key service writes. A read accepts
	// only the KEK the row's purpose writes with, or legacy. Both nil with no
	// WARDYN_AGE_KEY (store mode, or a writing key service).
	kek, platform kek.KEK
	// legacy is the pre-split KEK of the age key: wrote every row before the
	// purpose split, now only reads them, until `wardynd -rewrap`.
	legacy kek.KEK
	// separate: the platform KEK comes from WARDYN_PLATFORM_KEY_FILE, not the
	// age key, so no KEK the age key derives opens a platform row.
	separate bool
	// service is the configured key service (Deps.KEK, Vault Transit), or
	// nil. Opens rows sealed under it; with serviceWrites it also wraps every
	// new data key, boot keys included.
	service       kek.KEK
	serviceWrites bool
	// platformService is the second key service (Deps.PlatformKEK), or nil; it
	// opens boot keys only. With platformWrites it also wraps every boot key,
	// and a boot key under any other key is read only by `wardynd -rewrap
	// -rewrap-adopt-boot-keys`, which moves it here.
	platformService kek.KEK
	platformWrites  bool
	// ext is the configured external store, or nil. Pointer rows are read
	// through it in every mode; writeExt says whether Put writes there
	// (store mode) or seals locally.
	ext        secretstore.External
	writeExt   bool
	extTimeout time.Duration // bounds each call to ext
	// owner is the secretstore.Store.For namespace this view is scoped to;
	// "" is the operator namespace.
	owner string
	// subjects is the principal-key manager every view shares, nil on a Store
	// built only to rewrap (no pool).
	subjects *subjectkey.Manager
	// principalKeys is WARDYN_PRINCIPAL_KEYS=on: a person's credential row is
	// written under their principal key (enc_version 3). It governs writes
	// only; a v3 row is read whatever it says.
	principalKeys bool
	// domains are the declared key domains (Deps.KeyDomains): the KEK a
	// principal key in each is wrapped and opened under.
	domains secretstore.KeyDomains
}

// New constructs a Store whose KEKs are the local ones derived from identity
// (kek.NewLocalPurpose, and kek.NewLocal for the rows written before the
// purpose split). identity must be an *age.X25519Identity; it is used for that
// derivation only — the one other use of the age key is ConvertV0.
func New(pool *pgxpool.Pool, identity age.Identity) (*Store, error) {
	s := &Store{pool: pool}
	if err := s.setLocalKeys(identity, nil); err != nil {
		return nil, err
	}
	s.initSubjects()
	return s, nil
}

// withKEK adds the configured key service (Deps.KEK), if any.
func (s *Store) withKEK(d secretstore.Deps) {
	s.principalKeys = d.PrincipalKeys
	s.domains = d.KeyDomains
	if d.KEK != nil {
		s.service, s.serviceWrites = d.KEK, d.KEKWrites
	}
	if d.PlatformKEK != nil {
		s.platformService, s.platformWrites = d.PlatformKEK, d.PlatformKEKWrites
	}
}

// setLocalKeys derives the store's local KEKs from the age identity and, when
// platform is not nil (WARDYN_PLATFORM_KEY_FILE), the platform KEK from that
// second identity instead (design §2.13 c).
func (s *Store) setLocalKeys(identity, platform age.Identity) error {
	id, err := x25519(identity)
	if err != nil {
		return err
	}
	if s.kek, err = kek.NewLocalPurpose(id, kek.PurposeCred); err != nil {
		return err
	}
	if s.legacy, err = kek.NewLocal(id); err != nil {
		return err
	}
	s.separate = platform != nil
	if s.separate {
		if id, err = x25519(platform); err != nil {
			return err
		}
	}
	s.platform, err = kek.NewLocalPurpose(id, kek.PurposePlatform)
	return err
}

func x25519(identity age.Identity) (*age.X25519Identity, error) {
	x, ok := identity.(*age.X25519Identity)
	if !ok {
		return nil, fmt.Errorf("pg secretstore: identity is %T; use *age.X25519Identity", identity)
	}
	return x, nil
}

// writer is the KEK a new envelope for (owner, name) is wrapped under: the
// platform key service for a boot key when there is one, else the key service
// when it writes, else the local KEK of the row's purpose.
func (s *Store) writer(owner, name string) kek.KEK {
	if s.platformWrites && secretstore.Kind(owner, name) == "platform" {
		return s.platformService
	}
	if s.serviceWrites {
		return s.service
	}
	return s.localWriter(owner, name)
}

// localWriter is the local KEK of the row's purpose: the platform KEK for a
// boot key, the credential KEK for every other row. nil with no age key.
func (s *Store) localWriter(owner, name string) kek.KEK {
	if secretstore.Kind(owner, name) == "platform" {
		return s.platform
	}
	return s.kek
}

// reader is the KEK that may open row e, by its exact kek_id, never a
// fallback: the key service for its own rows; for a local row, the local KEK
// its purpose writes with, or the pre-split KEK — except a platform row once
// the platform key is separate, which only the platform key opens (anything
// the age key derives could otherwise forge a boot key). With a platform key
// service the same holds for every other key, the credential key service
// included: a boot key opens under the platform key service alone.
func (s *Store) reader(e envelope) (kek.KEK, error) {
	if s.platformService != nil && secretstore.Kind(e.ownedBy, e.name) == "platform" {
		if e.kekID == s.platformService.ID() {
			return s.platformService, nil
		}
		if s.platformWrites {
			return nil, &refusal{ErrAdoptNotRequested, fmt.Sprintf("is a boot key sealed under key %q, but this wardynd opens boot keys only under %q. %s", e.kekID, s.platformService.ID(), adoptHint)}
		}
	}
	if s.service != nil && e.kekID == s.service.ID() {
		return s.service, nil
	}
	if !isLocal(e.kekID) {
		return nil, fmt.Errorf("is sealed under key %q, which this wardynd is not configured to reach (WARDYN_KEK and its settings name the key service)", e.kekID)
	}
	if s.kek == nil {
		return nil, fmt.Errorf("is sealed under key %q, but this wardynd has no WARDYN_AGE_KEY — keep it set until `wardynd -migrate-secrets` or `wardynd -rewrap` reports none left", e.kekID)
	}
	w := s.localWriter(e.ownedBy, e.name)
	if e.kekID == w.ID() {
		return w, nil
	}
	if e.kekID == s.legacy.ID() && !(s.separate && w == s.platform) {
		return s.legacy, nil
	}
	if s.separate && secretstore.Kind(e.ownedBy, e.name) == "platform" {
		return nil, &refusal{ErrAdoptNotRequested, fmt.Sprintf("is a boot key sealed under key %q, but this wardynd opens boot keys only under %q. %s", e.kekID, w.ID(), adoptHint)}
	}
	return nil, fmt.Errorf("is sealed under key %q, but this wardynd opens it only under %q (a row sealed under another of this wardynd's own keys moves with `wardynd -rewrap`)", e.kekID, w.ID())
}

// adoptHint is the one instruction a boot key under the wrong key gets: adopting
// is something an operator does once, on purpose, so a row met after that is
// not Wardyn's.
const adoptHint = "If you have never adopted boot keys onto the platform key, run `wardynd -rewrap -rewrap-adopt-boot-keys`. " +
	"If you already have, these rows were not written by Wardyn: investigate before moving anything."

// ErrAdoptNotRequested is a boot key under a key other than the platform key,
// met by a `-rewrap` run that was not told to adopt boot keys.
var ErrAdoptNotRequested = errors.New("boot key under another key and -rewrap-adopt-boot-keys not given")

// ErrMixedBootKeys is a rewrap that found some boot keys under the platform key
// and others under another key, a state no run of wardynd leaves; adopting is
// refused even when asked for.
var ErrMixedBootKeys = errors.New("boot keys under the platform key and under another key")

// refusal is an error whose text is msg and which is kind (errors.Is), so the
// audit row can say why a run was refused without carrying the text, which
// names rows.
type refusal struct {
	kind error
	msg  string
}

func (r *refusal) Error() string { return r.msg }
func (r *refusal) Unwrap() error { return r.kind }

// isLocal reports whether kekID names a KEK an age identity derives: "local:"
// before the purpose split, "local/<purpose>:" after.
func isLocal(kekID string) bool {
	return strings.HasPrefix(kekID, "local:") || strings.HasPrefix(kekID, "local/")
}

// Name identifies this backend for audit and UI: "pg", or in store mode the
// external store's name ("vaultkv").
func (s *Store) Name() string {
	if s.writeExt {
		return s.ext.Name()
	}
	return "pg"
}

// ExternalName names the configured external store ("vaultkv", "azurekv"),
// or "" when there is none.
func (s *Store) ExternalName() string {
	if s.ext == nil {
		return ""
	}
	return s.ext.Name()
}

// StoresExternally describes the external store every write goes to ("Vault
// at vault.example:8200"), or "" in local mode.
func (s *Store) StoresExternally() string {
	if s.writeExt {
		return s.ext.Describe()
	}
	return ""
}

// KeyService describes the key service that wraps every write ("Vault
// Transit at vault.example:8200"), or "" when the local KEK does, or in store
// mode.
func (s *Store) KeyService() string {
	if d, ok := s.service.(interface{ Describe() string }); ok && s.serviceWrites && !s.writeExt {
		return d.Describe()
	}
	return ""
}

// For returns a view scoped to owner — see secretstore.Store.For's doc
// comment for the fallback/isolation contract. A shallow copy: owner is the
// only field that differs, so every view over the same *pgxpool.Pool sees
// the same rows, just through a different (owned_by) lens.
func (s *Store) For(owner string) secretstore.Store {
	cp := *s
	cp.owner = owner
	return &cp
}

// rowRef names a row in an error: its owner and name, never its value.
func rowRef(owner, name string) string {
	return fmt.Sprintf("(owned_by=%q, name=%q)", owner, name)
}

// Put seals value in a fresh envelope and upserts it, scoped to this view's
// owner. A replace re-keys the row: new DEK, new nonces. A different owner
// holding the same name is a DIFFERENT row (migration 0050) and is never
// touched. Nothing is written unless the whole envelope was built, so a failed
// wrap leaves any existing row as it was.
func (s *Store) Put(ctx context.Context, name string, value []byte) error {
	if s.writeExt {
		return s.putExternal(ctx, name, value)
	}
	r, err := s.sealRow(ctx, s.owner, name, value)
	if err != nil {
		return fmt.Errorf("pg secretstore: seal %s: %w", rowRef(s.owner, name), err)
	}
	if rev, guarded := secretstore.IfRevisionFrom(ctx); guarded {
		return s.putIfRevision(ctx, name, rev, r)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (owned_by, name) DO UPDATE
			SET enc_version=$3, kek_id=$4, wrapped_dek=$5, ciphertext=$6, expires_at=$7, updated_at=now()`,
		s.owner, name, r.version, r.kekID, r.wrapped, r.ct, expiresAt(ctx),
	)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s: %w", rowRef(s.owner, name), err)
	}
	return nil
}

// expiresAt is the row's expires_at for a Put under ctx: the time
// secretstore.WithExpiry named, or NULL.
func expiresAt(ctx context.Context) *time.Time {
	if at, ok := secretstore.ExpiryFrom(ctx); ok {
		return &at
	}
	return nil
}

// seal builds one v1 envelope: a fresh DEK seals value under AAD_secret, then
// k wraps the DEK bound to the same (owner, name).
func seal(ctx context.Context, k kek.KEK, owner, name string, value []byte) (wrapped, ct []byte, err error) {
	dek := make([]byte, kek.DEKSize)
	if _, err := rand.Read(dek); err != nil {
		return nil, nil, fmt.Errorf("draw data key: %w", err)
	}
	if ct, err = kek.Seal(dek, value, secretAAD(owner, name)); err != nil {
		return nil, nil, fmt.Errorf("seal value: %w", err)
	}
	if wrapped, err = k.Wrap(ctx, dek, kek.Bind(owner, name)); err != nil {
		return nil, nil, fmt.Errorf("wrap data key: %w", err)
	}
	return wrapped, ct, nil
}

// secretAAD is AAD_secret = Encode("wardyn/secret/v1", owned_by, name).
func secretAAD(owner, name string) []byte {
	return kek.Encode(secretAADLabel, owner, name)
}

// envelope is one row as read back: owned_by is the ROW's owner (the operator's
// "" on a fallback read), which is what both AADs are checked against.
type envelope struct {
	ownedBy, name string
	version       int16
	kekID         string
	wrapped, ct   []byte
}

// Get retrieves and opens a secret by name: this view's own (owner, name)
// row if one exists, else the operator's ("", name) row. Under
// secretstore.OwnRowOnly (an owner_only grant) only the view owner's own row
// matches. Returns an error wrapping pgx.ErrNoRows and secretstore.ErrNotFound
// when absent, and ONLY then: a row that exists but will not open is a
// distinct error, so loadOrCreateSecret can never mistake a tampered boot key
// for a missing one and mint over it.
func (s *Store) Get(ctx context.Context, name string) ([]byte, error) {
	e := envelope{name: name}
	q := `SELECT owned_by, enc_version, kek_id, wrapped_dek, ciphertext FROM secrets
		  WHERE owned_by IN ('', $1) AND name=$2 ORDER BY (owned_by = $1) DESC LIMIT 1`
	if secretstore.OwnRowOnly(ctx) {
		q = `SELECT owned_by, enc_version, kek_id, wrapped_dek, ciphertext FROM secrets
		  WHERE owned_by = $1 AND name=$2`
	}
	err := s.pool.QueryRow(ctx, q, s.owner, name).Scan(&e.ownedBy, &e.version, &e.kekID, &e.wrapped, &e.ct)
	if errors.Is(err, pgx.ErrNoRows) {
		// Satisfy BOTH the seam sentinel (secretstore.ErrNotFound, what the
		// conformance suite + callers check) and the historical pgx.ErrNoRows
		// match (existing tests + cmd/wardynd loadOrCreateSecret) via errors.Join.
		return nil, fmt.Errorf("pg secretstore: secret %q not found: %w", name,
			errors.Join(secretstore.ErrNotFound, pgx.ErrNoRows))
	}
	if err != nil {
		// The database did not answer: transient, like an external store's
		// outage, so a running run rides it out on its last-good value.
		return nil, fmt.Errorf("pg secretstore: get %s: %w: %w", rowRef(s.owner, name), secretstore.ErrUnavailable, err)
	}
	secretstore.NoteRow(ctx, secretstore.Row{Store: s.Name(), Owner: e.ownedBy, Name: e.name, Ref: e.kekID})
	return s.open(ctx, e)
}

// open checks and decrypts one row. Every refusal is fail-closed and names the
// row; none wraps a not-found sentinel.
func (s *Store) open(ctx context.Context, e envelope) ([]byte, error) {
	ref := rowRef(e.ownedBy, e.name)
	switch {
	case e.version == 0:
		return nil, fmt.Errorf("pg secretstore: %s is a pre-envelope (v0) row written after this database was converted — an older wardynd is still writing to it; stop every older replica, then restart this one to convert the row", ref)
	case e.version == extVersion:
		return s.openExternal(ctx, e)
	case e.version == pkVersion:
		return s.openPrincipal(ctx, e)
	case e.version != encVersion:
		return nil, fmt.Errorf("pg secretstore: row %s "+unknownVersion, ref, e.version)
	}
	k, err := s.reader(e)
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: %s %w", ref, err)
	}
	// An older wardynd's replace is `SET ciphertext=` alone: it leaves this
	// row's v1 columns in place around an age payload. Conversion never revisits
	// a v1 row, so say what happened instead of calling it tampering.
	if bytes.HasPrefix(e.ct, ageHeader) {
		return nil, fmt.Errorf("pg secretstore: %s was overwritten in place by an older wardynd (it holds an age payload under v1 columns) — an older wardynd is still writing to this database; stop every older replica, then set this secret again", ref)
	}
	dek, err := k.Unwrap(ctx, e.wrapped, kek.Bind(e.ownedBy, e.name))
	if errors.Is(err, secretstore.ErrUnavailable) {
		return nil, fmt.Errorf("pg secretstore: %s could not be unlocked — key service %q did not answer: %w", ref, e.kekID, err)
	}
	// The key service's own error (a deleted key, a revoked policy) is its to
	// name; calling it a forged row would send the operator to the database.
	if errors.Is(err, kek.ErrService) {
		return nil, fmt.Errorf("pg secretstore: %s could not be unlocked: %w", ref, err)
	}
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: %s refused — its data key does not unwrap for this row (moved, forged, corrupted, or its key version retired): %w", ref, err)
	}
	plain, err := kek.Open(dek, e.ct, secretAAD(e.ownedBy, e.name))
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: %s refused — its value fails the integrity check for this row (moved, forged or corrupted): %w", ref, err)
	}
	return plain, nil
}

// Delete removes this view's own (owner, name) row. Idempotent (no-op if
// absent) and never touches a different owner's row of the same name —
// deleting a member's row leaves the operator's readable, and a member can
// never reach another member's row to delete it in the first place.
func (s *Store) Delete(ctx context.Context, name string) error {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	ref := rowRef(s.owner, name)
	tx, err := beginReadCommitted(ctx, s.pool)
	if err != nil {
		return fmt.Errorf("pg secretstore: delete %s: begin: %w", ref, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// A guarded delete (WithIfRevision) is the Put's compare-and-set: a holder
	// that lost its lock must not delete a newer row. The check runs under the
	// row's lock, before deleteLocked touches an external store.
	if rev, guarded := secretstore.IfRevisionFrom(ctx); guarded {
		if err := lockRow(ctx, tx, s.owner, name); err != nil {
			return fmt.Errorf("pg secretstore: delete %s: %w", ref, err)
		}
		if err := s.checkRevision(ctx, tx, name, rev); err != nil {
			return err
		}
	}
	if _, err := s.deleteLocked(ctx, tx, s.owner, name); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg secretstore: delete %s: commit: %w", ref, err)
	}
	return nil
}

// DeleteEverywhere removes every owner's row of each name, deliberately NOT
// scoped to s.owner. One transaction, all or nothing. Rows are locked one at
// a time in (owned_by, name) order, so two calls take their locks in the same
// order and cannot deadlock; the listing takes no row lock, since Put takes a
// row's advisory lock before the row itself.
func (s *Store) DeleteEverywhere(ctx context.Context, names []string) (int, error) {
	tx, err := beginReadCommitted(ctx, s.pool)
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: delete everywhere: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `SELECT owned_by, name FROM secrets WHERE name = ANY($1) ORDER BY owned_by, name`, names)
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: delete everywhere: %w", err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Owner, Name string }])
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: delete everywhere: %w", err)
	}
	n := 0
	for _, r := range all {
		ok, err := s.deleteLocked(ctx, tx, r.Owner, r.Name)
		if err != nil {
			return 0, err
		}
		if ok {
			n++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("pg secretstore: delete everywhere: commit: %w", err)
	}
	return n, nil
}

// Holders returns every owner of each name's rows — see
// secretstore.Store.Holders. One query over the rows, never a value: a
// pointer row's owner is on the row, so the external store is not asked.
func (s *Store) Holders(ctx context.Context, names []string) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, owned_by FROM secrets WHERE name = ANY($1) ORDER BY name, owned_by`, names)
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: holders: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var name, owner string
		if err := rows.Scan(&name, &owner); err != nil {
			return nil, fmt.Errorf("pg secretstore: scan holder: %w", err)
		}
		out[name] = append(out[name], owner)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg secretstore: iterate holders: %w", err)
	}
	return out, nil
}

// List returns this view's OWN secret names only, in lexical order — never
// unioned with the operator's. A caller wanting "everything a principal may
// see" composes For("").List() ∪ For(owner).List() itself.
func (s *Store) List(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name FROM secrets WHERE owned_by=$1 ORDER BY name`, s.owner)
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: list: %w: %w", secretstore.ErrUnavailable, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("pg secretstore: scan name: %w", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg secretstore: iterate names: %w", err)
	}
	if names == nil {
		names = []string{}
	}
	return names, nil
}

// Rekey rewraps every row's data key from the local KEKs of oldID to those of
// newID and returns how many rows it rewrapped. It is the body of wardynd's
// `-rotate-age-key` maintenance mode and is NOT part of the secretstore.Store
// seam: Store is per-name late-bound access, this is a whole-table admin
// operation. wrapped_dek, kek_id and updated_at change, but the sealed value
// (and its DEK) is untouched, so a rotation never decrypts a credential.
// platform is the separate platform identity (WARDYN_PLATFORM_KEY_FILE), or
// nil: boot keys under it stay as they are.
//
// Pointer rows (store mode) and rows under a key service (Transit, Key Vault)
// hold nothing under the age key and are left alone; `wardynd -rewrap` moves
// those.
//
// ALL-OR-NOTHING: one transaction, and any row that is not a v1 row under the
// old keys, or whose data key does not unwrap, aborts the whole thing with
// nothing committed, so every secret stays readable with the OLD key. A v0
// row aborts too — the serving boot converts those (ConvertV0), not a rotation.
//
// The FOR UPDATE on the select buys lost-update prevention, not exclusivity:
// a concurrent Put of one of those names waits and lands after the commit
// instead of being clobbered, but does not keep rows out from under the
// retired key (a Put of a new name inserts straight past these locks under
// READ COMMITTED). That every committed row reads with newID is carried by
// the offline requirement below, not the lock.
//
// The caller supplies BOTH identities: the daemon must be offline (its
// in-memory Store still holds the old KEK), and the caller must persist
// newID before a restart and emit the secret.rekey audit event.
func Rekey(ctx context.Context, pool *pgxpool.Pool, oldID, newID, platform age.Identity) (int, error) {
	from, to := &Store{}, &Store{}
	if err := from.setLocalKeys(oldID, platform); err != nil {
		return 0, fmt.Errorf("pg secretstore: rekey old identity: %w", err)
	}
	if err := to.setLocalKeys(newID, platform); err != nil {
		return 0, fmt.Errorf("pg secretstore: rekey new identity: %w", err)
	}
	// A row under a key service holds nothing under the age key: left alone.
	// A row under any key no provider claims aborts, naming it (from.reader).
	target := func(e envelope) kek.KEK {
		if e.version == encVersion && kek.IsServiceID(e.kekID) {
			return nil
		}
		return to.writer(e.ownedBy, e.name)
	}
	// A principal key under a key service holds nothing under the age key either.
	pkTarget := func(domain, kekID string) (kek.KEK, error) {
		if kek.IsServiceID(kekID) {
			return nil, nil
		}
		return to.pkWriter(domain)
	}
	n, _, err := rewrapAll(ctx, pool, "rekey", from.reader, target, nil, nil, nil, func(tx pgx.Tx) (int, error) {
		return subjectkey.Rewrap(ctx, tx, from.pkReader, pkTarget, nil, nil)
	})
	return n, err
}

// RewrapResult is what RewrapKeys did.
type RewrapResult struct {
	// Rewrapped is how many rows' data keys moved.
	Rewrapped int
	// PrincipalKeys is how many principal keys' wraps moved. They move in the
	// transaction that moves the rows, so KeyVersion names a version only when
	// the `secrets` rows and the principal keys are both at it.
	PrincipalKeys int
	// KeyService is the kek_id of the key service every write now uses
	// (WARDYN_KEK=transit or azurekv), or "" when the local keys do.
	KeyService string
	// KeyVersion is the key version every row under a versioned key service
	// is now wrapped under, else "": retiring every other version at the
	// service (Transit's min_decryption_version, disabling Key Vault
	// versions) is then safe.
	KeyVersion string
	// PlatformKeyService and PlatformKeyVersion are the same for the platform
	// key service (Deps.PlatformKEK), which wraps the boot keys alone.
	PlatformKeyService string
	PlatformKeyVersion string
	// DomainKeyServices and DomainKeyVersions are the same, per declared key
	// domain (Deps.KeyDomains), whose key wraps that domain's principal keys
	// alone: the domain's kek_id, and the version every principal key in it is
	// now wrapped under, for a versioned key. A domain holding no key yet is
	// reported at its latest version, since nothing is under an older one.
	DomainKeyServices map[string]string
	DomainKeyVersions map[string]string
	// Rotated reports that a versioned key service named a newer version for
	// a wrap than the latest one read when the run began, so a rotation landed
	// mid-run. KeyVersion and PlatformKeyVersion are then "": run -rewrap again
	// until it moves no row, and retire no version before then.
	Rotated bool
}

// Rewrap is RewrapKeys over the local keys alone: identity, and the separate
// platform identity (WARDYN_PLATFORM_KEY_FILE) or nil, and whether the operator
// asked to adopt boot keys (-rewrap-adopt-boot-keys). It returns how many rows
// it moved.
func Rewrap(ctx context.Context, pool *pgxpool.Pool, identity, platform age.Identity, adopt bool) (int, error) {
	res, err := RewrapKeys(ctx, secretstore.Deps{Pool: pool, AgeIdentity: identity, PlatformIdentity: platform, AdoptBootKeys: adopt})
	return res.Rewrapped, err
}

// RewrapKeys moves every sealed row's data key to the KEK a write under d
// uses today, and returns what it did. It is the body of wardynd's `-rewrap`
// maintenance mode, the one command that moves data keys between KEKs:
//   - onto the local KEK of the row's purpose: a row written before the
//     purpose split, and, once WARDYN_PLATFORM_KEY_FILE is set, a boot key
//     still under the age key's platform KEK;
//   - between the local keys and a key service: with d.KEKWrites
//     (WARDYN_KEK=transit) every row moves to the key service, and with the
//     key service read-only every row under it moves back to the local keys;
//   - onto a versioned key service's latest version, so every other version
//     can be retired (Transit's min_decryption_version, disabled Key Vault
//     versions).
//   - with d.PlatformKEK writing (d.PlatformKEKWrites), every boot key moves
//     to the platform key service, at its latest version, from wherever it
//     sits (the credential key service, the age key, the separate platform
//     key); with it read-only, every boot key under it moves back to the key
//     a write would use.
//
// The rewrap is CLIENT-SIDE: each data key is unwrapped under the row's own
// KEK and wrapped again under the target, both bound to the row. Transit's
// server-side rewrap endpoint is never called — Vault doesn't document
// associated_data on it, and a rewrap that dropped the binding would be
// silent. The sealed value is never decrypted, and it is all-or-nothing:
// a key service that fails mid-run aborts with nothing committed. Pointer
// rows hold no data key and are never touched.
//
// Moving the boot keys onto a separate platform key trusts what the age key
// holds at that moment — the one step at which the age key vouches for a
// platform row. From then on nothing the age key derives opens one.
func RewrapKeys(ctx context.Context, d secretstore.Deps) (RewrapResult, error) {
	var res RewrapResult
	s := &Store{}
	if d.AgeIdentity != nil {
		if err := s.setLocalKeys(d.AgeIdentity, d.PlatformIdentity); err != nil {
			return res, fmt.Errorf("pg secretstore: rewrap: %w", err)
		}
	}
	s.withKEK(d)
	if s.kek == nil && !s.serviceWrites {
		return res, errors.New("pg secretstore: rewrap needs a key to wrap under: WARDYN_AGE_KEY, or a WARDYN_KEK key service (transit or azurekv)")
	}
	// A principal key naming a domain the file does not declare cannot be moved
	// or read: refuse before anything changes.
	if err := subjectkey.Verify(ctx, d.Pool, s.domainNames(), nil); err != nil {
		return res, fmt.Errorf("pg secretstore: rewrap REFUSED (nothing changed): %w", err)
	}
	source := s.reader
	// A boot key may still sit under an earlier key of its own purpose: the
	// age key's platform KEK, the credential key service, or, for the
	// platform key service, the key it is being moved off. Those readers
	// serve it only here, never at a serving boot, and only when the operator
	// attested to the adoption (d.AdoptBootKeys).
	var prior []*Store
	if d.AdoptBootKeys && s.platformWrites {
		p := *s
		p.platformService, p.platformWrites = nil, false
		prior = append(prior, &p)
	}
	var agePlatformID string // the platform KEK the age key alone derives
	if d.AdoptBootKeys && s.separate {
		shared := &Store{}
		if err := shared.setLocalKeys(d.AgeIdentity, nil); err != nil {
			return res, fmt.Errorf("pg secretstore: rewrap: %w", err)
		}
		prior = append(prior, shared)
		agePlatformID = shared.platform.ID()
	}
	if len(prior) > 0 {
		source = func(e envelope) (kek.KEK, error) {
			k, err := s.reader(e)
			if err == nil || secretstore.Kind(e.ownedBy, e.name) != "platform" {
				return k, err
			}
			for _, p := range prior {
				if k, err = p.reader(e); err == nil {
					break
				}
			}
			return k, err
		}
	}
	latest := map[string]string{}
	if s.serviceWrites {
		res.KeyService = s.service.ID()
		n, err := latestVersion(ctx, s.service)
		if err != nil {
			return res, fmt.Errorf("pg secretstore: rewrap: read the latest version of %s: %w", res.KeyService, err)
		}
		res.KeyVersion, latest[res.KeyService] = n, n
	}
	if s.platformWrites {
		res.PlatformKeyService = s.platformService.ID()
		n, err := latestVersion(ctx, s.platformService)
		if err != nil {
			return res, fmt.Errorf("pg secretstore: rewrap: read the latest version of %s: %w", res.PlatformKeyService, err)
		}
		res.PlatformKeyVersion, latest[res.PlatformKeyService] = n, n
	}
	target := func(e envelope) kek.KEK { return s.writer(e.ownedBy, e.name) }
	// The second check, whether or not boot keys may be adopted: a boot key
	// under another key beside any under the platform key was written there
	// after the move. Retiring the platform key is checked too: no
	// legitimate retire starts mixed.
	guard := func(all []envelope) error {
		switch {
		case s.platformService != nil:
			return refuseMixedBootKeys(s.platformService.ID(), all)
		case s.separate && !s.serviceWrites:
			return refuseMixedBootKeys(s.platform.ID(), all)
		case s.separate && agePlatformID != "":
			// A key service writes beside the platform file, and the age
			// reader is armed: a boot key under the age-derived platform KEK
			// is adopted only while no boot key has moved onto the service
			// or the file key yet.
			return refuseMixed(s.service.ID(), all,
				func(id string) bool { return id == s.service.ID() || id == s.platform.ID() },
				func(id string) bool { return id == agePlatformID })
		}
		return nil
	}
	// A principal key never moves between domains, so each domain's key is read
	// at its latest version and moves only that domain's keys onto it.
	if err := s.readDomainVersions(ctx, &res, latest); err != nil {
		return res, err
	}
	rotated := map[string]bool{}
	// Every principal key moves onto the latest version of its own domain's
	// KEK; boot keys are never among them.
	pkTarget := func(domain, _ string) (kek.KEK, error) { return s.pkWriter(domain) }
	n, pk, err := rewrapAll(ctx, d.Pool, "rewrap", source, target, latest, guard, rotated, func(tx pgx.Tx) (int, error) {
		return subjectkey.Rewrap(ctx, tx, s.pkReader, pkTarget, latest, rotated)
	})
	res.Rewrapped, res.PrincipalKeys = n, pk
	// A key service rotated while the run was moving rows: some rows are under
	// a newer version than the one read first, so no version is yet safe to
	// retire. Report none; another pass moves them and reports the truth.
	if err == nil && len(rotated) > 0 {
		res.Rotated = true
		res.KeyVersion, res.PlatformKeyVersion, res.DomainKeyVersions = "", "", nil
	}
	return res, err
}

// refuseMixedBootKeys refuses a rewrap that finds a boot key under any key but
// platformID while another is already under it. Every boot key a wardynd wrote
// or moved sits under the platform key once one does, so the other was written
// where it sits after the move, by whoever holds that key and can write the
// table. It changes nothing and names the rows.
func refuseMixedBootKeys(platformID string, all []envelope) error {
	return refuseMixed(platformID, all,
		func(id string) bool { return id == platformID },
		func(id string) bool { return id != platformID })
}

// refuseMixed is that check for a given split of the boot keys' kek_ids: it
// refuses when some boot key is under a key moved accepts and another is under
// a key planted accepts. A key service writing beside a platform file leaves
// boot keys under the service or the file key legitimately, and only a key the
// age key alone derives is what its holder can plant.
func refuseMixed(platformID string, all []envelope, moved, planted func(kekID string) bool) error {
	var under bool
	var other []string
	for _, e := range all {
		if secretstore.Kind(e.ownedBy, e.name) != "platform" {
			continue
		}
		switch {
		case moved(e.kekID):
			under = true
		case planted(e.kekID):
			other = append(other, fmt.Sprintf("%s under %q", rowRef(e.ownedBy, e.name), e.kekID))
		}
	}
	if !under || len(other) == 0 {
		return nil
	}
	return &refusal{ErrMixedBootKeys, fmt.Sprintf("pg secretstore: rewrap REFUSED (nothing changed): boot keys are already under the platform key %q, "+
		"yet %s sit under another key, a mixed state no run of wardynd leaves. These rows were not written by Wardyn: "+
		"find out who wrote them (updated_at, the audit log, database access logs) and restore the boot keys from a backup if they are forged",
		platformID, strings.Join(other, ", "))}
}

// latestVersion is the version a wrap under k would name, or "" when k has no
// versions.
func latestVersion(ctx context.Context, k kek.KEK) (string, error) {
	if v, ok := k.(kek.Versioned); ok {
		return v.LatestVersion(ctx)
	}
	return "", nil
}

// rewrapAll rewraps, in one transaction, every sealed row's data key from the
// KEK source names for it to the one target names. target is nil for a row
// the operation leaves alone; a row already under its target — and, when the
// target is versioned and latest holds its kek_id, at that version — is
// skipped. latest maps a versioned key service's kek_id to the version a wrap
// made now would name (nil: none). op names the operation in its errors. guard,
// when not nil, sees every selected (locked) row before any moves and may
// refuse the whole run. rotated, when not nil, collects the kek_id of each
// versioned target that wrapped a row under a version other than latest's.
// principal moves the principal keys inside the same transaction, after the
// rows, and returns how many it moved; rewrapAll returns that second count.
// Rows of version extVersion hold no data key and pkVersion rows hold theirs
// under a principal key, so neither is selected.
func rewrapAll(ctx context.Context, pool *pgxpool.Pool, op string, source func(envelope) (kek.KEK, error), target func(envelope) kek.KEK, latest map[string]string, guard func([]envelope) error, rotated map[string]bool, principal func(pgx.Tx) (int, error)) (int, int, error) {
	tx, err := beginReadCommitted(ctx, pool)
	if err != nil {
		return 0, 0, fmt.Errorf("pg secretstore: %s begin: %w", op, err)
	}
	// A Rollback after a successful Commit is a documented no-op; on every error
	// path below it is the thing that makes this all-or-nothing.
	defer func() { _ = tx.Rollback(ctx) }()

	// ORDER BY owned_by, name keeps the lock/abort order stable and readable;
	// the UPDATE below keys on BOTH columns, since two owners can share a name
	// (migration 0050).
	rows, err := tx.Query(ctx, `SELECT owned_by, name, enc_version, kek_id, wrapped_dek FROM secrets WHERE enc_version NOT IN ($1, $2) ORDER BY owned_by, name FOR UPDATE`, extVersion, pkVersion)
	if err != nil {
		return 0, 0, fmt.Errorf("pg secretstore: %s select: %w", op, err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (envelope, error) {
		var e envelope
		err := r.Scan(&e.ownedBy, &e.name, &e.version, &e.kekID, &e.wrapped)
		return e, err
	})
	if err != nil {
		return 0, 0, fmt.Errorf("pg secretstore: %s scan: %w", op, err)
	}

	if guard != nil {
		if err := guard(all); err != nil {
			return 0, 0, err
		}
	}
	n := 0
	for i, e := range all {
		to := target(e)
		if to == nil {
			continue
		}
		if e.version == encVersion && e.kekID == to.ID() {
			old, verr := kek.Behind(to, e.wrapped, latest[to.ID()])
			if verr != nil {
				return 0, 0, rewrapAbort(op, i, len(all), rowRef(e.ownedBy, e.name), verr)
			}
			if !old {
				continue
			}
		}
		wrapped, rerr := rewrap(ctx, source, to, e)
		if rerr != nil {
			return 0, 0, rewrapAbort(op, i, len(all), rowRef(e.ownedBy, e.name), rerr)
		}
		if rotated != nil {
			if was, verr := kek.Behind(to, wrapped, latest[to.ID()]); verr != nil {
				return 0, 0, rewrapAbort(op, i, len(all), rowRef(e.ownedBy, e.name), verr)
			} else if was {
				rotated[to.ID()] = true
			}
		}
		if _, uerr := tx.Exec(ctx,
			`UPDATE secrets SET kek_id=$3, wrapped_dek=$4, updated_at=now() WHERE owned_by=$1 AND name=$2`, e.ownedBy, e.name, to.ID(), wrapped,
		); uerr != nil {
			return 0, 0, rewrapAbort(op, i, len(all), rowRef(e.ownedBy, e.name), fmt.Errorf("update: %w", uerr))
		}
		n++
	}
	pk := 0
	if principal != nil {
		var perr error
		if pk, perr = principal(tx); perr != nil {
			return 0, 0, fmt.Errorf("pg secretstore: %s ABORTED in principal_keys (nothing committed — every secret is still readable with the OLD key): %w", op, perr)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("pg secretstore: %s commit (%d rows, %d principal keys, NOTHING committed — the old key still reads every secret): %w", op, n, pk, err)
	}
	return n, pk, nil
}

// rewrap moves one row's data key from the KEK source names for it to to.
func rewrap(ctx context.Context, source func(envelope) (kek.KEK, error), to kek.KEK, e envelope) ([]byte, error) {
	switch {
	case e.version == 0:
		return nil, fmt.Errorf("is a pre-envelope (v0) row — boot this wardynd once to convert it first")
	case e.version != encVersion:
		return nil, fmt.Errorf(unknownVersion, e.version)
	}
	from, err := source(e)
	if err != nil {
		return nil, err
	}
	bind := kek.Bind(e.ownedBy, e.name)
	dek, err := from.Unwrap(ctx, e.wrapped, bind)
	if err != nil {
		return nil, fmt.Errorf("unwrap with the old key: %w", err)
	}
	wrapped, err := to.Wrap(ctx, dek, bind)
	if err != nil {
		return nil, fmt.Errorf("wrap with the new key: %w", err)
	}
	return wrapped, nil
}

// beginReadCommitted starts a transaction on pool pinned to READ COMMITTED.
//
// Rekey and ConvertV0 each rewrite EVERY row they select under one
// transaction, so it must not inherit default_transaction_isolation: on a
// pool set to REPEATABLE READ a long rewrite takes a snapshot at its first
// statement and holds it for the whole rewrite, turning any concurrent
// writer into a serialization failure — and a ConvertV0 queued behind
// another's advisory lock would select rows that one already converted.
// READ COMMITTED is also the isolation the FOR UPDATE lock reasoning above
// is written against.
//
// SET TRANSACTION rather than pgx.TxOptions: equivalent as long as it's the
// first statement of the transaction, which it is. A failed SET rolls the
// half-open transaction back so no unpinned tx is ever returned (same shape
// as the broker's PgxStore.BeginReadCommitted and internal/db's beginReadCommitted).
func beginReadCommitted(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL READ COMMITTED`); err != nil {
		_ = tx.Rollback(context.Background())
		return nil, fmt.Errorf("pin read committed: %w", err)
	}
	return tx, nil
}

// rewrapAbort formats the one error Rekey and Rewrap fail with: what broke,
// on which row, and how far it had got — plus the load-bearing fact that the
// abort left the store untouched, which is what tells an operator to fix the
// row and retry rather than hunt for a half-rotated store.
func rewrapAbort(op string, done, total int, ref string, err error) error {
	return fmt.Errorf("pg secretstore: %s ABORTED after %d of %d rows (nothing committed — every secret is still readable with the OLD key): %s %w",
		op, done, total, ref, err)
}
