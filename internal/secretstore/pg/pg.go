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
	"github.com/jackc/pgx/v5/pgconn"
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

// KeyServiceStaticKey reports whether the key service that wraps every write
// is reached with a long-lived credential (AWS KMS under an IAM user's access
// key): metadata for the setup row, never a value.
func (s *Store) KeyServiceStaticKey() bool {
	d, ok := s.service.(interface{ StaticKey() bool })
	return ok && s.serviceWrites && !s.writeExt && d.StaticKey()
}

// KeyServiceNodeRole reports whether the key service that wraps every write is
// reached with the EC2 node's instance role, shared by every pod on the node:
// metadata for the same setup row, never a value.
func (s *Store) KeyServiceNodeRole() bool {
	d, ok := s.service.(interface{ NodeRole() bool })
	return ok && s.serviceWrites && !s.writeExt && d.NodeRole()
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
	if s.ext != nil {
		return s.putOverPointer(ctx, name, r)
	}
	if gen, fenced := s.generationFence(ctx); fenced {
		return s.putFenced(ctx, name, gen, r)
	}
	return s.putRow(ctx, s.pool, name, r)
}

// putRow writes the sealed row through q (the pool, or the fenced Put's
// transaction): a plain upsert, or the compare-and-set on the row's revision
// when the context carries one.
func (s *Store) putRow(ctx context.Context, q execer, name string, r sealedRow) error {
	if rev, guarded := secretstore.IfRevisionFrom(ctx); guarded {
		return s.putIfRevision(ctx, q, name, rev, r)
	}
	_, err := q.Exec(ctx, `
		INSERT INTO secrets (owned_by, name, enc_version, kek_id, wrapped_dek, ciphertext, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (owned_by, name) DO UPDATE
			SET enc_version=$3, kek_id=$4, wrapped_dek=$5, ciphertext=$6, expires_at=$7, updated_at=now(), value_rev=DEFAULT`,
		s.owner, name, r.version, r.kekID, r.wrapped, r.ct, expiresAt(ctx),
	)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s: %w", rowRef(s.owner, name), err)
	}
	return nil
}

// execer is what pgxpool.Pool and pgx.Tx share for a write.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
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
	defer clear(dek)
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
	// row's locks, before deleteLocked touches an external store.
	if rev, guarded := secretstore.IfRevisionFrom(ctx); guarded {
		if err := lockRow(ctx, tx, s.owner, name); err != nil {
			return fmt.Errorf("pg secretstore: delete %s: %w", ref, err)
		}
		if found, err := s.lockRevision(ctx, tx, name, rev); err != nil || !found {
			return err // nil when the holder expected no row and there is none
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
	if err := recordProviderChanges(ctx, tx); err != nil {
		return 0, err
	}
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
