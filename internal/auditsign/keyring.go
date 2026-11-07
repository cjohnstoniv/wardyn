// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package auditsign signs audit checkpoints: the Ed25519 key wardynd keeps in its secret store and the signer
// that folds new chained rows into a signed statement (internal/auditreceipt reads both offline).
package auditsign

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auditreceipt"
)

// keyFile is the stored value. Previous public keys are kept forever, newest first: a checkpoint signed years
// ago must still verify. Only the current key's seed is stored.
type keyFile struct {
	Current  currentKey    `json:"current"`
	Previous []previousKey `json:"previous"`
}

type currentKey struct {
	Seed      []byte    `json:"seed"`
	CreatedAt time.Time `json:"created_at"`
}

type previousKey struct {
	PublicKey  []byte    `json:"public_key"`
	Kid        string    `json:"kid"`
	CreatedAt  time.Time `json:"created_at"`
	ReplacedAt time.Time `json:"replaced_at"`
	// Retired marks a key treated as possibly stolen: verifiers fail every checkpoint it signed.
	Retired bool `json:"retired"`
}

// Key is a loaded signing key with the public halves of every key it replaced.
type Key struct {
	priv     ed25519.PrivateKey
	kid      string
	created  time.Time
	previous []previousKey
}

// NewKey makes a fresh stored value: one new key, no previous ones.
func NewKey(now time.Time) ([]byte, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("audit receipt key: %w", err)
	}
	return json.Marshal(keyFile{Current: currentKey{Seed: seed, CreatedAt: now.UTC()}, Previous: []previousKey{}})
}

// Load parses a stored value strictly. A value it refuses is never replaced by its caller: a new key over a
// stored one would hide which key signed the checkpoints already sent off-box.
func Load(raw []byte) (*Key, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f keyFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("audit receipt key: %w", err)
	}
	if len(f.Current.Seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("audit receipt key: seed is %d bytes, want %d", len(f.Current.Seed), ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(f.Current.Seed)
	k := &Key{priv: priv, kid: auditreceipt.KeyID(priv.Public().(ed25519.PublicKey)), created: f.Current.CreatedAt, previous: f.Previous}
	seen := map[string]bool{k.kid: true}
	for i, p := range f.Previous {
		if len(p.PublicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("audit receipt key: previous key %d is %d bytes, want %d", i, len(p.PublicKey), ed25519.PublicKeySize)
		}
		if p.Kid != auditreceipt.KeyID(p.PublicKey) {
			return nil, fmt.Errorf("audit receipt key: previous key %d: kid %q is not its key's", i, p.Kid)
		}
		if seen[p.Kid] {
			return nil, fmt.Errorf("audit receipt key: kid %s appears twice", p.Kid)
		}
		seen[p.Kid] = true
	}
	return k, nil
}

// Rotate returns the stored value that follows raw: a new current key, and the replaced one's public half first
// in the previous list, marked retired when retirePrevious (the recovery from a stolen key). Older keys keep
// their own flags. History is not re-signed: that would launder whatever a thief rewrote.
func Rotate(raw []byte, retirePrevious bool, now time.Time) ([]byte, error) {
	old, err := Load(raw)
	if err != nil {
		return nil, err
	}
	next, err := NewKey(now)
	if err != nil {
		return nil, err
	}
	var f keyFile
	if err := json.Unmarshal(next, &f); err != nil {
		return nil, err
	}
	f.Previous = append([]previousKey{{
		PublicKey: old.PublicKey(), Kid: old.kid, CreatedAt: old.created, ReplacedAt: now.UTC(), Retired: retirePrevious,
	}}, old.previous...)
	return json.Marshal(f)
}

// KeyID is the current key's kid.
func (k *Key) KeyID() string { return k.kid }

// PublicKey is the current key's public half.
func (k *Key) PublicKey() ed25519.PublicKey { return k.priv.Public().(ed25519.PublicKey) }

// Sign signs a statement's exact bytes with the current key.
func (k *Key) Sign(msg []byte) []byte { return ed25519.Sign(k.priv, msg) }

// Verify reports whether sig over msg is by the key named kid: the current one or any previous one, retired
// ones included (a checkpoint signed before a retire is still this install's own; verifiers judge the flag).
func (k *Key) Verify(kid string, msg, sig []byte) bool {
	if kid == k.kid {
		return ed25519.Verify(k.PublicKey(), msg, sig)
	}
	for _, p := range k.previous {
		if p.Kid == kid {
			return ed25519.Verify(p.PublicKey, msg, sig)
		}
	}
	return false
}

// Retired reports whether kid names a previous key that was retired (treated as possibly stolen).
func (k *Key) Retired(kid string) bool {
	for _, p := range k.previous {
		if p.Kid == kid && p.Retired {
			return true
		}
	}
	return false
}

// PublicKeys is the key file wardyn audit keys saves: the current key first, then every previous one, newest
// first. No private material.
func (k *Key) PublicKeys() []auditreceipt.PublicKey {
	out := []auditreceipt.PublicKey{publicKey(k.PublicKey(), k.created, nil, false)}
	for _, p := range k.previous {
		replaced := p.ReplacedAt
		out = append(out, publicKey(p.PublicKey, p.CreatedAt, &replaced, p.Retired))
	}
	return out
}

func publicKey(pub ed25519.PublicKey, created time.Time, replaced *time.Time, retired bool) auditreceipt.PublicKey {
	der, _ := x509.MarshalPKIXPublicKey(pub) // an Ed25519 key always marshals
	return auditreceipt.PublicKey{
		Kid:          auditreceipt.KeyID(pub),
		PublicKey:    pub,
		PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
		Fingerprint:  auditreceipt.Fingerprint(pub),
		CreatedAt:    created,
		ReplacedAt:   replaced,
		Retired:      retired,
	}
}

// MinInterval is the shortest signing interval; 0 turns signing off. A shorter one would sign a checkpoint for
// nearly every row on a busy install.
const MinInterval = 10 * time.Second

// CheckInterval refuses a negative interval and one between 0 and MinInterval.
func CheckInterval(d time.Duration) error {
	if d == 0 || d >= MinInterval {
		return nil
	}
	return errors.New("the audit checkpoint interval must be 0 (off) or at least 10s")
}
