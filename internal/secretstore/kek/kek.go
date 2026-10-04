// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package kek holds the key-encryption keys that wrap each stored
// credential's data key (DEK), and the shared envelope framing.
//
// A KEK never sees a credential value: it wraps and unwraps a 32-byte DEK,
// bound to the row's owner and name. Each provider implements KEK; the row
// records the provider's ID as kek_id, and a read picks the KEK by that id.
// `local` lives here, one key per purpose (NewLocalPurpose); Vault Transit
// (package vaultkv) wraps through the service's own encrypt/decrypt, and Azure
// Key Vault (package azurekv) through wrapkey/unwrapkey under a signature.
// WARDYN_KEK selects the one every write uses.
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
	"strconv"
	"strings"

	"filippo.io/age"
)

// KEK wraps and unwraps data keys. bind carries the row's identity
// (BindOwner, BindName); a provider must refuse to unwrap under a different
// bind than the one it wrapped under (F1), enforced as associated data (local,
// Transit) or a service-made signature (Key Vault), and nobody without the
// KEK may make a wrap that unwraps (F2): with an asymmetric key the public
// half alone must not suffice.
type KEK interface {
	// ID is stable and non-secret; it is recorded on each row as kek_id.
	ID() string
	Wrap(ctx context.Context, dek []byte, bind map[string]string) ([]byte, error)
	Unwrap(ctx context.Context, wrapped []byte, bind map[string]string) ([]byte, error)
}

// ErrService marks a key service's own definitive answer (deleted key,
// revoked policy, missing mount) as opposed to a local wrap refusal. Never
// transient: an unreachable service is secretstore.ErrUnavailable instead.
var ErrService = errors.New("key service error")

// ErrAccess is a key service refusing this process (401/403): definitive for rule 21, but about
// the caller, not the wrap.
var ErrAccess = errors.New("key service refuses this process's access")

// ErrKeyMissing is a key service that does not hold the key or mount a wrap names: about the
// service or this process's configuration, never the row.
var ErrKeyMissing = errors.New("the key service does not hold the key")

// Versioned is a KEK whose key has versions (Vault Transit, Key Vault): each
// wrap names the version it was made under, and `wardynd -rewrap` moves every
// row naming any other version onto the latest, so the others can be retired.
// A version is opaque; only equality counts.
type Versioned interface {
	KEK
	// WrapVersion is the key version wrapped was made under.
	WrapVersion(wrapped []byte) (string, error)
	// LatestVersion is the version a wrap made now would name.
	LatestVersion(ctx context.Context) (string, error)
}

// Behind reports whether wrapped, made under the versioned KEK k, names a
// version other than latest. Unversioned, or with latest "", it never does.
func Behind(k KEK, wrapped []byte, latest string) (bool, error) {
	v, ok := k.(Versioned)
	if !ok || latest == "" {
		return false, nil
	}
	n, err := v.WrapVersion(wrapped)
	if err != nil {
		return false, err
	}
	return n != latest, nil
}

// The kek_id prefixes of the key services. A row under one holds nothing an
// age key derives.
const (
	TransitIDPrefix  = "transit:"
	AzureKeyIDPrefix = "azurekv-key:"
)

// IsServiceID reports whether id names a key service's KEK (Transit, Key
// Vault). A kek_id that is neither local nor a known service's is not one.
func IsServiceID(id string) bool {
	return strings.HasPrefix(id, TransitIDPrefix) || strings.HasPrefix(id, AzureKeyIDPrefix)
}

// The bind keys: KMS encryption context / Transit associated data a
// provider is handed. A data-key wrap binds owner and name; a principal-key
// wrap (PrincipalBind) binds owner, purpose, version and domain instead.
const (
	BindOwner   = "wardyn:owner"
	BindName    = "wardyn:name"
	BindPurpose = "wardyn:purpose"
	BindVersion = "wardyn:version"
	BindDomain  = "wardyn:domain"
)

// Bind is the bind map for the row (owner, name). owner "" is the operator.
func Bind(owner, name string) map[string]string {
	return map[string]string{BindOwner: owner, BindName: name}
}

// PrincipalBind is the bind map for one generation of a subject's key. owner
// is never "": the operator namespace holds boot keys, which are not subject keys.
func PrincipalBind(owner, purpose string, version int, domain string) map[string]string {
	return map[string]string{BindOwner: owner, BindPurpose: purpose, BindVersion: strconv.Itoa(version), BindDomain: domain}
}

// DEKSize is the data key length: AES-256.
const DEKSize = 32

// Encode is the injective field encoding every AAD uses (length-prefixed
// fields); field 1 is the domain label, so an AAD for one purpose can never
// parse as another's.
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
// fresh random 96-bit nonce; the one framing shared by the value (under the
// DEK) and the local wrap (under the KEK). The nonce comes from Go's
// cryptographic module (cipher.NewGCMWithRandomNonce), so no caller supplies
// or can reuse one, and it stays the approved GCM under
// GODEBUG=fips140=on/only, where a caller-supplied IV is not. One message per
// DEK and one wrap per local-KEK call keep each key far below SP 800-38D's
// 2^32 random-nonce bound.
func Seal(key, plaintext, aad []byte) ([]byte, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nil, plaintext, aad), nil
}

// Open reverses Seal. Any mismatch (key, AAD, or a flipped byte) is one
// authentication failure; the error carries nothing of the input.
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

// localInfo is the AAD domain label of every local wrap, and the HKDF info
// of the pre-split KEK (NewLocal).
const localInfo = "wardyn/kek/v1"

// The purposes a local KEK serves. A platform KEK wraps the boot keys
// (secretstore.PlatformNames); a credential KEK wraps every other row.
const (
	PurposePlatform = "platform"
	PurposeCred     = "cred"
)

// Local is the default KEK: a symmetric key derived from WARDYN_AGE_KEY (or,
// for the platform purpose, WARDYN_PLATFORM_KEY_FILE). Being symmetric closes
// forgery — the age recipient is public, this key is not.
type Local struct {
	id  string
	key []byte
}

var _ KEK = (*Local)(nil)

// NewLocal derives the pre-split local KEK, which wrote every row before the
// purpose split and now only reads them:
// HKDF-SHA256(IKM = identity.String(), salt = empty, info = "wardyn/kek/v1").
// The id is "local:" plus the first 8 bytes of SHA-256 over the public
// recipient, hex — so a row names which age key sealed it without naming the
// key.
//
// GODEBUG=fips140=only forbids the identity's X25519 recipient; age silently
// leaves it empty rather than erroring, which would collide every identity
// onto the same kek_id, so a recipient that fails to parse is refused instead.
func NewLocal(identity *age.X25519Identity) (*Local, error) {
	recipient, err := localRecipient(identity)
	if err != nil {
		return nil, err
	}
	return newLocal(identity.String(), recipient, "local:", localInfo)
}

// NewLocalPurpose derives the local KEK of one purpose: HKDF info
// "wardyn/kek/v1/<purpose>" and id "local/<purpose>:<fingerprint>". Two
// purposes from one identity are unrelated keys, and AAD_kek names the
// purpose, so a wrap made for one never opens as the other.
func NewLocalPurpose(identity *age.X25519Identity, purpose string) (*Local, error) {
	if purpose != PurposePlatform && purpose != PurposeCred {
		return nil, fmt.Errorf("local KEK: unknown purpose %q", purpose)
	}
	recipient, err := localRecipient(identity)
	if err != nil {
		return nil, err
	}
	return newLocal(identity.String(), recipient, "local/"+purpose+":", localInfo+"/"+purpose)
}

// localRecipient is the identity's public recipient, refused when it fails to
// parse (the fips140=only case) since an empty one would give every age key
// the same id.
func localRecipient(identity *age.X25519Identity) (string, error) {
	recipient := identity.Recipient().String()
	if _, err := age.ParseX25519Recipient(recipient); err != nil {
		return "", errors.New("local KEK: the age identity has no public recipient (X25519 failed; GODEBUG=fips140=only forbids it) — the local key cannot run in FIPS 140-only mode; use a key service (WARDYN_KEK=transit or azurekv), alone or with a store mode, which needs no WARDYN_AGE_KEY")
	}
	return recipient, nil
}

// newLocal takes the derivation's inputs as plain strings so golden vectors
// can pin it without committing an age secret key.
func newLocal(ikm, recipient, idPrefix, info string) (*Local, error) {
	key, err := hkdf.Key(sha256.New, []byte(ikm), nil, info, DEKSize)
	if err != nil {
		return nil, fmt.Errorf("derive the local KEK: %w", err)
	}
	fp := sha256.Sum256([]byte(recipient))
	return &Local{id: idPrefix + hex.EncodeToString(fp[:8]), key: key}, nil
}

func (l *Local) ID() string { return l.id }

// Wrap: nonce(12) ‖ AES-256-GCM(KEK, nonce, dek, AAD_kek).
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

// Unwrap: a wrap moved to another row, or made under another key, fails
// authentication.
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

// pkWrapLabel is the AAD domain label of a principal-key wrap. It differs from
// localInfo, so a principal-key wrap can never verify as a data-key wrap.
const pkWrapLabel = "wardyn/pk-wrap/v1"

// WrapAAD is what every provider binds a wrap to: AAD_kek =
// Encode("wardyn/kek/v1", owner, name, kek_id) for a data key, and for a
// principal key Encode("wardyn/pk-wrap/v1", owner, purpose, version, domain,
// kek_id). A bind missing a key of its shape, or mixing the two shapes, is
// refused rather than defaulted, since a zero value here would seal a key to
// the wrong row.
func WrapAAD(bind map[string]string, kekID string) ([]byte, error) {
	owner, okO := bind[BindOwner]
	name, okN := bind[BindName]
	purpose, okP := bind[BindPurpose]
	version, okV := bind[BindVersion]
	domain, okD := bind[BindDomain]
	if okP || okV || okD {
		if !okO || !okP || !okV || !okD || okN {
			return nil, errors.New("a principal-key bind must carry " + BindOwner + ", " + BindPurpose + ", " + BindVersion + " and " + BindDomain + ", and no " + BindName)
		}
		return Encode(pkWrapLabel, owner, purpose, version, domain, kekID), nil
	}
	if !okO || !okN {
		return nil, errors.New("bind must carry both " + BindOwner + " and " + BindName)
	}
	return Encode(localInfo, owner, name, kekID), nil
}
