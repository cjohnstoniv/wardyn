// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

// Credential rows under a person's principal key (enc_version 3, "envelope
// v2" in the design): the row's data key is wrapped with kek.Seal under the
// owner's `cred` principal key instead of the root KEK, with kek_id "pk:v<n>"
// naming the key generation. Destroying that key (subjectkey.Destroy)
// crypto-erases every such row.
//
// Only a person's rows (owned_by <> '') ever take this form. A boot key, the
// operator namespace and the mask key stay v1 under the platform and credential
// KEKs whatever WARDYN_PRINCIPAL_KEYS says, and subjectkey refuses owner "".

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

// pkAADLabel is the domain label of AAD_pk, the binding of a data key wrapped
// under a principal key. It differs from every other label, so a data key
// sealed this way never opens as a value, a KEK wrap or a principal-key wrap.
const pkAADLabel = "wardyn/pk/v1"

// pkKekPrefix opens the kek_id of a v3 row: "pk:v<n>".
const pkKekPrefix = "pk:v"

// pkAAD is AAD_pk = Encode("wardyn/pk/v1", owned_by, name, version).
func pkAAD(owner, name string, version int) []byte {
	return kek.Encode(pkAADLabel, owner, name, strconv.Itoa(version))
}

// pkKekID is the kek_id of a row under generation version.
func pkKekID(version int) string { return pkKekPrefix + strconv.Itoa(version) }

// pkVersionOf reads the generation out of a v3 row's kek_id.
func pkVersionOf(kekID string) (int, bool) {
	rest, ok := strings.CutPrefix(kekID, pkKekPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n >= 1
}

// sealedRow is one envelope as written: the row's columns after its owner and name.
type sealedRow struct {
	version     int16
	kekID       string
	wrapped, ct []byte
}

// sealRow builds the envelope a write of (owner, name) stores: v3 under the
// owner's principal key when principal keys are on and the row is a person's,
// else v1 under the KEK writer picks.
func (s *Store) sealRow(ctx context.Context, owner, name string, value []byte) (sealedRow, error) {
	if s.principalKeys && secretstore.Kind(owner, name) == "people" {
		return s.sealPrincipal(ctx, owner, name, value)
	}
	k := s.writer(owner, name)
	wrapped, ct, err := seal(ctx, k, owner, name, value)
	if err != nil {
		return sealedRow{}, err
	}
	return sealedRow{encVersion, k.ID(), wrapped, ct}, nil
}

// sealPrincipal builds a v3 envelope: a fresh data key seals value under
// AAD_secret, and the owner's live `cred` key generation seals the data key
// under AAD_pk.
func (s *Store) sealPrincipal(ctx context.Context, owner, name string, value []byte) (sealedRow, error) {
	version, key, err := s.subjects.Current(ctx, owner, subjectkey.PurposeCred)
	if err != nil {
		return sealedRow{}, fmt.Errorf("the owner's principal key: %w", err)
	}
	defer clear(key)
	dek := make([]byte, kek.DEKSize)
	defer clear(dek)
	if _, err := rand.Read(dek); err != nil {
		return sealedRow{}, fmt.Errorf("draw data key: %w", err)
	}
	ct, err := kek.Seal(dek, value, secretAAD(owner, name))
	if err != nil {
		return sealedRow{}, fmt.Errorf("seal value: %w", err)
	}
	wrapped, err := kek.Seal(key, dek, pkAAD(owner, name, version))
	if err != nil {
		return sealedRow{}, fmt.Errorf("wrap data key: %w", err)
	}
	return sealedRow{pkVersion, pkKekID(version), wrapped, ct}, nil
}

// openPrincipal opens a v3 row: the owner's key generation (a durable
// destroyed_at check on every use), then the data key, then the value. A
// destroyed key is a definitive refusal naming it, never not-found.
func (s *Store) openPrincipal(ctx context.Context, e envelope) ([]byte, error) {
	ref := rowRef(e.ownedBy, e.name)
	version, ok := pkVersionOf(e.kekID)
	switch {
	case e.ownedBy == "":
		return nil, fmt.Errorf("pg secretstore: %s is sealed under a principal key but has no owner; only a person's credential is", ref)
	case !ok:
		return nil, fmt.Errorf("pg secretstore: %s names the principal key %q, which is not of the form %sN", ref, e.kekID, pkKekPrefix)
	case s.subjects == nil:
		return nil, fmt.Errorf("pg secretstore: %s is sealed under a principal key, which this store cannot open", ref)
	}
	key, err := s.subjects.Key(ctx, e.ownedBy, subjectkey.PurposeCred, version)
	switch {
	case errors.Is(err, secretstore.ErrUnavailable):
		return nil, fmt.Errorf("pg secretstore: %s could not be unlocked — its owner's key could not be checked: %w", ref, err)
	case errors.Is(err, subjectkey.ErrDataLoss):
		return nil, fmt.Errorf("pg secretstore: %s refused — its owner's principal key was destroyed, so the value is unrecoverable: %w", ref, err)
	case err != nil:
		return nil, fmt.Errorf("pg secretstore: %s could not be unlocked: %w", ref, err)
	}
	defer clear(key)
	dek, err := kek.Open(key, e.wrapped, pkAAD(e.ownedBy, e.name, version))
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: %s refused — its data key does not unwrap for this row (moved, forged or corrupted): %w", ref, err)
	}
	defer clear(dek)
	plain, err := kek.Open(dek, e.ct, secretAAD(e.ownedBy, e.name))
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: %s refused — its value fails the integrity check for this row (moved, forged or corrupted): %w", ref, err)
	}
	return plain, nil
}

// DestroyCredentialKey destroys every generation of owner's credential key:
// the rows sealed under it become unreadable on every replica at its next use.
// It is secretstore.CredentialKeyDestroyer, which EraseOwner calls after the
// owner's rows are deleted.
func (s *Store) DestroyCredentialKey(ctx context.Context, owner string) ([]int, error) {
	return s.subjects.Destroy(ctx, owner, subjectkey.PurposeCred)
}

// SealToPrincipalKeysResult is what SealToPrincipalKeys did.
type SealToPrincipalKeysResult struct {
	// Moved is how many credential rows moved into their owner's current
	// generation, from v1 or from an older generation.
	Moved int
	// Remaining is how many person-owned rows are still not under their
	// owner's current generation: a writer wrote one while the run was moving
	// the rest. Run again until it is 0.
	Remaining int
}

// SealToPrincipalKeys is the body of `wardynd -rewrap-principal-keys`: it moves
// every person's v1 credential row into a v3 envelope under its owner's current
// principal key, and every v3 row under an older generation of it (one a key
// domain reassignment left behind) into the current one, and returns what it
// did. It is not a root rotation (-rewrap moves principal keys and rows onto a
// new root KEK): it changes which key a row's data key is under, and an old
// generation's key itself never moves. A v3 row whose key was destroyed is
// already crypto-erased and is left. Boot keys, the operator namespace and
// pointer rows are never touched.
//
// Key operations finish before the transaction. The complete row is then
// revalidated under its write lock, retrying a replacement and skipping a
// deletion. An abort leaves every earlier row committed, so the run resumes. The data key is
// unwrapped under its v1 KEK and sealed again under the principal key; the
// sealed value is never decrypted, so nothing here is a secret.read. The
// caller holds db.SecretRekeyLockKey.
func (s *Store) SealToPrincipalKeys(ctx context.Context) (SealToPrincipalKeysResult, error) {
	var res SealToPrincipalKeysResult
	rows, err := s.pool.Query(ctx, `SELECT owned_by, name FROM secrets WHERE owned_by <> '' AND enc_version IN ($1, $2) ORDER BY owned_by, name`, encVersion, pkVersion)
	if err != nil {
		return res, fmt.Errorf("pg secretstore: seal to principal keys select: %w", err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Owner, Name string }])
	if err != nil {
		return res, fmt.Errorf("pg secretstore: seal to principal keys scan: %w", err)
	}
	for _, r := range all {
		moved, err := s.sealRowToPrincipal(ctx, r.Owner, r.Name)
		if err != nil {
			return res, fmt.Errorf("pg secretstore: sealing to principal keys ABORTED at %s after moving %d rows (each moved row is committed; fix this row and re-run): %w",
				rowRef(r.Owner, r.Name), res.Moved, err)
		}
		if moved {
			res.Moved++
		}
	}
	// A v3 row counts only while its owner has a live current generation to move
	// it into: a destroyed key leaves rows nothing can move.
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM secrets s WHERE s.owned_by <> '' AND (s.enc_version=$1 OR (s.enc_version=$2 AND EXISTS (
			SELECT 1 FROM principal_keys k WHERE k.owner=s.owned_by AND k.purpose=$3 AND k.destroyed_at IS NULL AND k.superseded_at IS NULL
			AND s.kek_id <> $4 || k.version::text)))`,
		encVersion, pkVersion, subjectkey.PurposeCred, pkKekPrefix).Scan(&res.Remaining); err != nil {
		return res, fmt.Errorf("pg secretstore: count the rows left: %w", err)
	}
	return res, nil
}

// Prepare outside the transaction, then compare the entire row under its write
// lock. A concurrent replacement is retried; a deletion is never resurrected.
func (s *Store) sealRowToPrincipal(ctx context.Context, owner, name string) (bool, error) {
	ctx, cancel := s.bounded(ctx)
	defer cancel()
	for {
		e, err := readRowSnapshot(ctx, s.pool, owner, name, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		var version int
		var wrapped []byte
		switch e.version {
		case encVersion:
			version, wrapped, err = s.sealV1ToPrincipal(ctx, e.envelope)
		case pkVersion:
			version, wrapped, err = s.resealToCurrent(ctx, e.envelope)
		default:
			return false, nil
		}
		if err != nil || wrapped == nil {
			return false, err
		}
		err = s.commitPrincipalRow(ctx, e, sealedRow{pkVersion, pkKekID(version), wrapped, e.ct})
		if errors.Is(err, secretstore.ErrRevisionChanged) {
			continue
		}
		return err == nil, err
	}
}

func (s *Store) commitPrincipalRow(ctx context.Context, before rowSnapshot, after sealedRow) error {
	tx, err := beginReadCommitted(ctx, s.pool)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := lockRow(ctx, tx, before.ownedBy, before.name); err != nil {
		return err
	}
	if err := replaceSnapshot(ctx, tx, before, after); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// sealV1ToPrincipal unwraps a v1 row's data key under its KEK and seals it
// under the owner's current generation.
func (s *Store) sealV1ToPrincipal(ctx context.Context, e envelope) (int, []byte, error) {
	from, err := s.reader(e)
	if err != nil {
		return 0, nil, err
	}
	dek, err := from.Unwrap(ctx, e.wrapped, kek.Bind(e.ownedBy, e.name))
	if err != nil {
		return 0, nil, fmt.Errorf("unwrap with the old key: %w", err)
	}
	defer clear(dek)
	version, key, err := s.subjects.Current(ctx, e.ownedBy, subjectkey.PurposeCred)
	if err != nil {
		return 0, nil, fmt.Errorf("the owner's principal key: %w", err)
	}
	defer clear(key)
	wrapped, err := kek.Seal(key, dek, pkAAD(e.ownedBy, e.name, version))
	if err != nil {
		return 0, nil, fmt.Errorf("wrap with the principal key: %w", err)
	}
	return version, wrapped, nil
}

// resealToCurrent moves a v3 row's data key from an older generation of its
// owner's key into the current one. A nil result is a row already current, or
// one whose key was destroyed, which nothing can move.
func (s *Store) resealToCurrent(ctx context.Context, e envelope) (int, []byte, error) {
	old, ok := pkVersionOf(e.kekID)
	if !ok {
		return 0, nil, fmt.Errorf("names the principal key %q, which is not of the form %sN", e.kekID, pkKekPrefix)
	}
	version, wrapped, err := s.subjects.Reseal(ctx, e.ownedBy, subjectkey.PurposeCred, old, e.wrapped,
		pkAAD(e.ownedBy, e.name, old), func(v int) []byte { return pkAAD(e.ownedBy, e.name, v) })
	if errors.Is(err, subjectkey.ErrDataLoss) {
		return 0, nil, nil // crypto-erased
	}
	if err != nil {
		return 0, nil, fmt.Errorf("re-seal under the owner's current principal key: %w", err)
	}
	return version, wrapped, nil
}
