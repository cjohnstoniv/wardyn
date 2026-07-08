// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"testing"

	"filippo.io/age"
)

// Regression: New must accept a generated *age.X25519Identity. The original
// implementation asserted an anonymous interface with the WRONG Recipient()
// return type (the age.Recipient interface instead of the concrete
// *age.X25519Recipient), which can never match and broke wardynd boot.
func TestNew_AcceptsX25519Identity(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	s, err := New(nil, id)
	if err != nil {
		t.Fatalf("New rejected a valid X25519 identity: %v", err)
	}
	if s.recipient == nil {
		t.Fatal("recipient not derived")
	}
	// Encrypt/decrypt roundtrip exercises the derived recipient without a DB.
	ct, err := s.encrypt([]byte("hello"))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	pt, err := s.decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(pt) != "hello" {
		t.Fatalf("roundtrip mismatch: %q", pt)
	}
}
