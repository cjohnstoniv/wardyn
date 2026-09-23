// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Boot keys: the process-global keys wardynd generates for itself on first
// boot and keeps in the secret store (loadOrCreateSecret and its four callers).

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// secretKeyStore is the minimal secret-store surface loadOrCreateSecret needs.
// Narrowing the dependency to Get/Put makes the load-or-create control flow
// unit-testable with a hand-rolled fake (cmd/wardynd/main_test.go) and documents
// that key bootstrap touches nothing else. secretstore.Store satisfies it.
type secretKeyStore interface {
	Get(ctx context.Context, name string) ([]byte, error)
	Put(ctx context.Context, name string, value []byte) error
}

// bootKeyStore is the secret store as the boot-key loaders see it: Get/Put
// plus lockCreate, which serializes a create across replicas (#754). Two
// replicas booting at once against an empty store would otherwise each
// generate a key and each Put it, and the loser would serve with a key nobody
// else holds — tokens and cookies it signs fail everywhere else.
type bootKeyStore struct {
	secretKeyStore
	lockCreate func(ctx context.Context) (release func(), err error)
}

// newBootKeyStore picks lockCreate for this process. Without
// -allow-multi-instance, claimSingleInstance already holds
// db.SingleInstanceLockKey for the process lifetime, so no other wardynd is
// booting beside this one and there is nothing to serialize — and a second held
// connection would leave a pool_max_conns=2 daemon none for the create's own
// Get and Put. With it, every create waits on db.BootKeyLockKey and fails the
// boot closed if it cannot get it.
func newBootKeyStore(secrets secretKeyStore, pool *pgxpool.Pool, multiInstance bool) bootKeyStore {
	if !multiInstance {
		return bootKeyStore{secrets, holdsSingleInstanceLock}
	}
	return bootKeyStore{secrets, func(ctx context.Context) (func(), error) {
		return db.AdvisoryLock(ctx, pool, db.BootKeyLockKey)
	}}
}

// holdsSingleInstanceLock is lockCreate for a process that holds the
// single-instance lock: that lock already excludes every other wardynd.
func holdsSingleInstanceLock(context.Context) (func(), error) { return func() {}, nil }

// loadOrCreateSecret is the shared, fail-closed bootstrap for the boot keys
// (the embedded-identity signing key, the OIDC and UI session keys, the SSH
// host key).
//
// SECURITY (boot-key destruction): the previous per-key logic treated ANY
// Get error as "key not present" and then generated + Put a fresh key,
// OVERWRITING whatever ciphertext was already there. The pg secret store
// distinguishes a TRUE not-found (it wraps pgx.ErrNoRows) from an age-decrypt
// failure (a generic error). Conflating the two meant a single transient/
// permanent decrypt error silently rotated the key, invalidating every issued
// SVID and every active session cookie. We now regenerate ONLY when the key is
// genuinely absent or present-but-invalid; on any other error we FAIL CLOSED —
// return the error and never Put, so the existing ciphertext is preserved.
//
// SECURITY (first-boot race, #754): a create happens only under lockCreate,
// and the key is read AGAIN under it, so a replica that lost the race returns
// the winner's key instead of writing its own over it.
//
//   - valid reports whether an existing raw value is usable as-is.
//   - generate produces fresh key material to persist (called only when the key
//     is absent or invalid).
func loadOrCreateSecret(
	ctx context.Context,
	secrets bootKeyStore,
	name string,
	valid func(raw []byte) bool,
	generate func() ([]byte, error),
) ([]byte, error) {
	// secretKeyStore has no For: the boot keys it bootstraps (identity signing,
	// OIDC session) are process-global, never per-principal.
	if raw, ok, err := loadBootKey(ctx, secrets, name, valid); ok || err != nil {
		return raw, err
	}
	release, lerr := secrets.lockCreate(ctx)
	if lerr != nil {
		return nil, fmt.Errorf("lock secret %q for create: %w", name, lerr)
	}
	defer release()
	if raw, ok, err := loadBootKey(ctx, secrets, name, valid); ok || err != nil {
		return raw, err
	}

	val, gerr := generate()
	if gerr != nil {
		return nil, fmt.Errorf("generate secret %q: %w", name, gerr)
	}
	if perr := secrets.Put(ctx, name, val); perr != nil {
		return nil, fmt.Errorf("persist secret %q: %w", name, perr)
	}
	return val, nil
}

// loadBootKey reports ok with the stored key when it is usable, !ok when it
// must be created, and an error when it must not be.
func loadBootKey(ctx context.Context, secrets secretKeyStore, name string, valid func([]byte) bool) (raw []byte, ok bool, err error) {
	raw, err = secrets.Get(ctx, name)
	switch {
	case err == nil:
		if valid(raw) {
			return raw, true, nil
		}
		// Present but unusable (e.g. a legacy too-short session key): create
		// over it. This is safe — the stored value cannot serve its purpose
		// anyway.
		return nil, false, nil
	case errors.Is(err, pgx.ErrNoRows):
		// TRUE not-found (first boot): create.
		return nil, false, nil
	default:
		// Decrypt failure or any other Get error: FAIL CLOSED. Do NOT generate
		// or Put — overwriting here would destroy the existing key.
		return nil, false, fmt.Errorf("load secret %q: %w", name, err)
	}
}

// loadOrCreateSigningKey returns the embedded identity ES256 key, persisting a
// freshly-generated one into the secret store on first boot. The key never
// enters a sandbox; it lives only in the broker/control-plane process memory
// and the encrypted secret column. A decrypt error fails closed (see
// loadOrCreateSecret) rather than minting a fresh key over the old one.
func loadOrCreateSigningKey(ctx context.Context, secrets bootKeyStore) (*ecdsa.PrivateKey, error) {
	raw, err := loadOrCreateSecret(ctx, secrets, secretSigningKey,
		func(b []byte) bool { return len(b) > 0 },
		func() ([]byte, error) {
			key, gerr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if gerr != nil {
				return nil, fmt.Errorf("generate signing key: %w", gerr)
			}
			pemBytes, merr := marshalECPrivateKeyPEM(key)
			if merr != nil {
				return nil, merr
			}
			slog.Info("wardynd: generated and persisted embedded identity signing key")
			return pemBytes, nil
		},
	)
	if err != nil {
		return nil, err
	}
	key, perr := parseECPrivateKeyPEM(raw)
	if perr != nil {
		return nil, fmt.Errorf("parse stored signing key: %w", perr)
	}
	return key, nil
}

// loadOrCreateSessionKey returns the 32-byte OIDC session-cookie HMAC key,
// persisting a freshly-generated one into the secret store on first boot. Like
// the signing key it never enters a sandbox; it lives only in process memory
// and the encrypted secret column. Returning the key is safe — the caller is
// the OIDC authenticator, which never logs it. A decrypt error fails closed
// (see loadOrCreateSecret) rather than rotating every session out from under
// logged-in users.
func loadOrCreateSessionKey(ctx context.Context, secrets bootKeyStore) ([]byte, error) {
	return loadOrCreateSecret(ctx, secrets, secretSessionKey,
		func(b []byte) bool { return len(b) >= 32 },
		func() ([]byte, error) {
			key := make([]byte, 32)
			if _, gerr := rand.Read(key); gerr != nil {
				return nil, fmt.Errorf("generate session key: %w", gerr)
			}
			slog.Info("wardynd: generated and persisted OIDC session key")
			return key, nil
		},
	)
}

// loadOrCreateUISessionKey returns the UI-sandbox gateway's relay-cookie HMAC
// key, persisted in the secret store and generated on first boot — the same
// loadOrCreateSecret pattern as the signing/session/SSH-host keys. It is
// SEPARATE from the OIDC session key on purpose: the two cookies live on
// different origins and authorize different things, so one key must never be
// able to forge the other's cookie.
func loadOrCreateUISessionKey(ctx context.Context, secrets bootKeyStore) ([]byte, error) {
	return loadOrCreateSecret(ctx, secrets, secretUISessionKey,
		func(b []byte) bool { return len(b) >= 32 },
		func() ([]byte, error) {
			key := make([]byte, 32)
			if _, gerr := rand.Read(key); gerr != nil {
				return nil, fmt.Errorf("generate ui session key: %w", gerr)
			}
			slog.Info("wardynd: generated and persisted UI-sandbox relay cookie key")
			return key, nil
		},
	)
}

// loadOrCreateSSHHostKey returns the SSH gateway's ed25519 host key,
// persisting a freshly-generated one into the secret store on first boot —
// the same loadOrCreateSecret pattern as the signing/session keys above,
// cloned for the one new field this key needs (ed25519 has no "is this a
// valid key of the right size" shortcut as cheap as the session key's length
// check, so validity is "does it parse", checked by the generate/persist
// round-trip itself; a corrupt stored value fails the parse below and
// loadOrCreateSecret's caller sees that as a startup error, never a silent
// re-mint over a key clients have already pinned).
func loadOrCreateSSHHostKey(ctx context.Context, secrets bootKeyStore) (ed25519.PrivateKey, error) {
	raw, err := loadOrCreateSecret(ctx, secrets, secretSSHHostKey,
		func(b []byte) bool { return len(b) > 0 },
		func() ([]byte, error) {
			_, priv, gerr := ed25519.GenerateKey(rand.Reader)
			if gerr != nil {
				return nil, fmt.Errorf("generate ssh host key: %w", gerr)
			}
			pemBytes, merr := marshalEd25519PrivateKeyPEM(priv)
			if merr != nil {
				return nil, merr
			}
			slog.Info("wardynd: generated and persisted ssh gateway host key")
			return pemBytes, nil
		},
	)
	if err != nil {
		return nil, err
	}
	key, perr := parseEd25519PrivateKeyPEM(raw)
	if perr != nil {
		return nil, fmt.Errorf("parse stored ssh host key: %w", perr)
	}
	return key, nil
}
