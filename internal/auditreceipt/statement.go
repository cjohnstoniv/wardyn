// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package auditreceipt

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CheckpointFormat names the statement layout below; a verifier refuses any other. Layout 1 signed no
// seq_digest, so it left seqs unbound, and it is refused by name.
const CheckpointFormat = "wardyn-audit-checkpoint/2"

// Statement is what wardynd signs for one checkpoint. The signed bytes are Bytes(): compact JSON, keys in
// exactly this field order, nothing HTML-escaped. They are stored and shipped verbatim, and ParseStatement
// accepts only that form, so the bytes a verifier reads are the bytes that were signed.
type Statement struct {
	Format string `json:"format"`
	// Kid is the signing key's id (KeyID).
	Kid string `json:"kid"`
	// SeqLo and SeqHi bound the rows covered; Rows counts them (seq can skip numbers, so Rows may be smaller
	// than the span).
	SeqLo int64 `json:"seq_lo"`
	SeqHi int64 `json:"seq_hi"`
	Rows  int64 `json:"rows"`
	// HeadHash is the row_hash at SeqHi; Digest is Fold(Prev, the rows' hashes); SeqDigest is SeqDigest(the
	// rows' seqs, their hashes), which binds each seq to its row.
	HeadHash  string `json:"head_hash"`
	Digest    string `json:"digest"`
	SeqDigest string `json:"seq_digest"`
	// Prev is StatementHash of the previous checkpoint's statement, or "" for the first.
	Prev     string `json:"prev"`
	SignedAt string `json:"signed_at"`
}

// Bytes is the statement's one encoding.
func (s Statement) Bytes() []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // strings and integers only: it cannot fail
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// ParseStatement reads a statement's exact text and refuses anything but its one encoding with valid fields.
func ParseStatement(text string) (Statement, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	var s Statement
	if err := dec.Decode(&s); err != nil {
		return Statement{}, fmt.Errorf("statement: %w", err)
	}
	// The format first, so a statement in another layout is refused by name rather than as a bad encoding.
	if s.Format != CheckpointFormat {
		return Statement{}, fmt.Errorf("statement: format %q, want %q (no other checkpoint layout is read)", s.Format, CheckpointFormat)
	}
	if string(s.Bytes()) != text {
		return Statement{}, errors.New("statement: not in its canonical encoding (key order, spacing, escapes or duplicates differ)")
	}
	switch {
	case !isLowerHex(s.Kid, 32):
		return Statement{}, errors.New("statement: kid is not 32 lower-case hex characters")
	case s.SeqLo < 0 || s.SeqHi < s.SeqLo:
		return Statement{}, fmt.Errorf("statement: seq range %d-%d is not a range", s.SeqLo, s.SeqHi)
	case s.Rows < 1 || s.Rows > s.SeqHi-s.SeqLo+1:
		return Statement{}, fmt.Errorf("statement: %d rows cannot fit seq %d-%d", s.Rows, s.SeqLo, s.SeqHi)
	case !isLowerHex(s.HeadHash, 64) || !isLowerHex(s.Digest, 64) || !isLowerHex(s.SeqDigest, 64):
		return Statement{}, errors.New("statement: head_hash, digest and seq_digest must be 64 lower-case hex characters")
	case s.Prev != "" && !isLowerHex(s.Prev, 64):
		return Statement{}, errors.New("statement: prev must be empty or 64 lower-case hex characters")
	}
	if _, err := time.Parse(time.RFC3339Nano, s.SignedAt); err != nil {
		return Statement{}, fmt.Errorf("statement: signed_at: %w", err)
	}
	return s, nil
}

// StatementHash is lowercase hex sha256 of a statement's exact text: the next checkpoint's prev.
func StatementHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// KeysFormat names the public-key file layout.
const KeysFormat = "wardyn-audit-keys/1"

// Keys is the public-key file an operator saves and later verifies against. It never holds a private key.
type Keys struct {
	Format string      `json:"format"`
	Keys   []PublicKey `json:"keys"`
}

// PublicKey is one checkpoint signing key, current or previous. Previous keys are kept for as long as their
// checkpoints must verify.
type PublicKey struct {
	Kid string `json:"kid"`
	// PublicKey is the raw 32-byte Ed25519 key (base64 in JSON).
	PublicKey    []byte     `json:"public_key"`
	PublicKeyPEM string     `json:"public_key_pem,omitempty"`
	Fingerprint  string     `json:"fingerprint,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	ReplacedAt   *time.Time `json:"replaced_at,omitempty"`
	// Retired marks a key treated as possibly stolen: every checkpoint it signed fails verification.
	Retired bool `json:"retired"`
}

// KeyID is a key's kid: the first 32 hex characters of sha256 of the raw public key.
func KeyID(pub ed25519.PublicKey) string { return Fingerprint(pub)[:32] }

// Fingerprint is the full sha256 hex of the raw public key, the form shown to people.
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}
