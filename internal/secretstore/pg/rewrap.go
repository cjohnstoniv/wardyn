// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

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
	// (WARDYN_KEK=transit, azurekv or awskms), or "" when the local keys do.
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
		return res, errors.New("pg secretstore: rewrap needs a key to wrap under: WARDYN_AGE_KEY, or a WARDYN_KEK key service (transit, azurekv or awskms)")
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
	defer clear(dek)
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
