// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package kek holds the key-encryption keys that wrap each stored
// credential's data key (DEK), and the envelope framing both layers share
// (credential-storage design §2.2, "The row (envelope v1)").
//
// A KEK never sees a credential value: it wraps and unwraps a 32-byte DEK,
// bound to the row's owner and name. Each provider is one implementation of
// KEK; the row records the provider's ID as kek_id, and a read picks the KEK by
// that id. `local` lives here; Vault Transit (package vaultkv, which holds the
// Vault client) wraps through the service's own encrypt and decrypt. WARDYN_KEK
// selects the one every write uses.
package kek

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"filippo.io/age"
)

// KEK wraps and unwraps data keys. bind carries the row's identity
// (BindOwner, BindName): a provider must refuse to unwrap under a bind other
// than the one it wrapped under — as associated data (local, Transit) or
// encryption context (AWS KMS).
type KEK interface {
	// ID is stable and non-secret; it is recorded on each row as kek_id.
	ID() string
	Wrap(ctx context.Context, dek []byte, bind map[string]string) ([]byte, error)
	Unwrap(ctx context.Context, wrapped []byte, bind map[string]string) ([]byte, error)
}

// Versioned is a KEK whose key has versions (Vault Transit): each wrap names
// the version it was made under, and `wardynd -rewrap` moves every row still
// wrapped under an older one to the latest, so the old versions can be retired.
type Versioned interface {
	KEK
	// WrapVersion is the key version wrapped was made under.
	WrapVersion(wrapped []byte) (int, error)
	// LatestVersion is the version a wrap made now would name.
	LatestVersion(ctx context.Context) (int, error)
}

// The bind keys: the KMS encryption context / Transit associated data a
// provider is handed, pinned in design §2.2.
const (
	BindOwner = "wardyn:owner"
	BindName  = "wardyn:name"
)

// Bind is the bind map for the row (owner, name). owner "" is the operator.
func Bind(owner, name string) map[string]string {
	return map[string]string{BindOwner: owner, BindName: name}
}

// DEKSize is the data key length: AES-256.
const DEKSize = 32

// Encode is the injective field encoding every AAD uses: for each field, its
// length as a big-endian uint32, then its bytes. Field 1 is the domain label,
// so an AAD for one purpose can never parse as another's.
func Encode(fields ...string) []byte {
	n := 0
	for _, f := range fields {
		n += 4 + len(f)
	}
	out := make([]byte, 0, n)
	for _, f := range fields {
		out = binary.BigEndian.AppendUint32(out, uint32(len(f)))
		out = append(out, f...)
	}
	return out
}

// Seal returns nonce(12) ‖ AES-256-GCM(key, nonce, plaintext, aad) under a
// fresh random 96-bit nonce. It is the one framing of both the value (under
// the DEK) and the local wrap (under the KEK). The nonce is drawn inside Go's
// cryptographic module (cipher.NewGCMWithRandomNonce, whose output is exactly
// this framing), so no caller ever supplies one and none can be reused — and
// it stays the approved GCM under GODEBUG=fips140=on/only, where a
// caller-supplied IV is not. One message per DEK and one wrap per local-KEK
// call keep each key far below SP 800-38D's 2^32 random-nonce bound.
func Seal(key, plaintext, aad []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nil, plaintext, aad), nil
}

// Open reverses Seal. Any mismatch — key, AAD, or a flipped byte — is one
// authentication failure, and the error carries nothing of the input.
func Open(key, sealed, aad []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.Overhead() {
		return nil, errors.New("sealed blob is shorter than a nonce and a tag")
	}
	plain, err := aead.Open(nil, nil, sealed, aad)
	if err != nil {
		return nil, errors.New("authentication failed")
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != DEKSize {
		return nil, fmt.Errorf("key is %d bytes, want %d", len(key), DEKSize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

// localInfo is the HKDF info string and the AAD domain label of a local wrap.
const localInfo = "wardyn/kek/v1"

// Local is the default KEK: a symmetric key derived from WARDYN_AGE_KEY. Being
// symmetric is what closes forgery — the age recipient is public, this key is
// not.
type Local struct {
	id  string
	key []byte
}

var _ KEK = (*Local)(nil)

// NewLocal derives the local KEK from an age identity:
// HKDF-SHA256(IKM = identity.String(), salt = empty, info = "wardyn/kek/v1").
// String() is the canonical "AGE-SECRET-KEY-1…" form ParseX25519Identity
// round-trips to; age exposes no raw scalar. The id is "local:" plus the first
// 8 bytes of SHA-256 over the public recipient, hex — so a row names which
// age key sealed it without naming the key.
func NewLocal(identity *age.X25519Identity) (*Local, error) {
	return newLocal(identity.String(), identity.Recipient().String())
}

// newLocal is NewLocal over the two strings it reads, so the golden vectors can
// pin the derivation without committing an age secret key.
func newLocal(ikm, recipient string) (*Local, error) {
	key, err := hkdf.Key(sha256.New, []byte(ikm), nil, localInfo, DEKSize)
	if err != nil {
		return nil, fmt.Errorf("derive the local KEK: %w", err)
	}
	fp := sha256.Sum256([]byte(recipient))
	return &Local{id: "local:" + hex.EncodeToString(fp[:8]), key: key}, nil
}

// ID implements KEK.
func (l *Local) ID() string { return l.id }

// Wrap implements KEK: nonce(12) ‖ AES-256-GCM(KEK, nonce, dek, AAD_kek).
func (l *Local) Wrap(_ context.Context, dek []byte, bind map[string]string) ([]byte, error) {
	aad, err := l.aad(bind)
	if err != nil {
		return nil, err
	}
	if len(dek) != DEKSize {
		return nil, fmt.Errorf("local KEK: data key is %d bytes, want %d", len(dek), DEKSize)
	}
	return Seal(l.key, dek, aad)
}

// Unwrap implements KEK. A wrap moved to another row, or made under another
// key, fails authentication.
func (l *Local) Unwrap(_ context.Context, wrapped []byte, bind map[string]string) ([]byte, error) {
	aad, err := l.aad(bind)
	if err != nil {
		return nil, err
	}
	dek, err := Open(l.key, wrapped, aad)
	if err != nil {
		return nil, fmt.Errorf("local KEK %s: unwrap: %w", l.id, err)
	}
	return dek, nil
}

func (l *Local) aad(bind map[string]string) ([]byte, error) {
	aad, err := WrapAAD(bind, l.id)
	if err != nil {
		return nil, fmt.Errorf("local KEK: %w", err)
	}
	return aad, nil
}

// WrapAAD is AAD_kek = Encode("wardyn/kek/v1", owner, name, kek_id): what every
// provider binds a wrap to, as associated data (local, Transit). A bind missing
// either key is refused rather than defaulted: a zero value here would seal a
// DEK to the wrong row.
func WrapAAD(bind map[string]string, kekID string) ([]byte, error) {
	owner, okO := bind[BindOwner]
	name, okN := bind[BindName]
	if !okO || !okN {
		return nil, errors.New("bind must carry both " + BindOwner + " and " + BindName)
	}
	return Encode(localInfo, owner, name, kekID), nil
}
