// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"strings"
	"testing"

	"filippo.io/age"
)

// New must accept a generated *age.X25519Identity. An anonymous interface
// asserting the wrong Recipient() return type (the age.Recipient interface
// instead of the concrete *age.X25519Recipient) can never match, and breaks
// wardynd boot.
func TestNew_AcceptsX25519Identity(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	s, err := New(nil, id)
	if err != nil {
		t.Fatalf("New rejected a valid X25519 identity: %v", err)
	}
	if !strings.HasPrefix(s.kek.ID(), "local/cred:") {
		t.Fatalf("kek_id %q is not a local KEK", s.kek.ID())
	}
	// Seal/open round trip exercises the derived KEK without a DB.
	ctx := context.Background()
	wrapped, ct, err := seal(ctx, s.kek, "", "n", []byte("hello"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	pt, err := s.open(ctx, envelope{name: "n", version: encVersion, kekID: s.kek.ID(), wrapped: wrapped, ct: ct})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(pt) != "hello" {
		t.Fatalf("roundtrip mismatch: %q", pt)
	}
}
