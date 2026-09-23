// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// TestSecretAAD_GoldenVector pins AAD_secret byte for byte, member and operator
// form. The member vector is the one internal/secretstore/kek's golden test
// seals under (computed outside Go); the operator one pins the empty owner
// field as a zero length, not an absent field.
func TestSecretAAD_GoldenVector(t *testing.T) {
	for _, c := range []struct{ owner, want string }{
		{"alice@corp.example", "0000001077617264796e2f7365637265742f763100000012616c69636540636f72702e6578616d706c6500000011616e7468726f7069632d6170692d6b6579"},
		{"", "0000001077617264796e2f7365637265742f76310000000000000011616e7468726f7069632d6170692d6b6579"},
	} {
		if got := hex.EncodeToString(secretAAD(c.owner, "anthropic-api-key")); got != c.want {
			t.Errorf("secretAAD(%q) = %s, want %s", c.owner, got, c.want)
		}
	}
}

// TestOpen_RefusesEveryMismatch is the row-level refusal table without a
// database; the *_pg_test.go files prove the same through real SQL. Every
// refusal names the row, never carries the value, and is never a not-found.
func TestOpen_RefusesEveryMismatch(t *testing.T) {
	ctx := context.Background()
	id, _ := age.GenerateX25519Identity()
	other, _ := age.GenerateX25519Identity()
	s, _ := New(nil, id)
	foreign, _ := New(nil, other)
	const value = "sk-value-that-must-not-leak"

	good := func(owner, name string) envelope {
		w, ct, err := seal(ctx, s.kek, owner, name, []byte(value))
		if err != nil {
			t.Fatal(err)
		}
		return envelope{ownedBy: owner, name: name, version: encVersion, kekID: s.kek.ID(), wrapped: w, ct: ct}
	}
	moved := func(e envelope, owner, name string) envelope { e.ownedBy, e.name = owner, name; return e }
	forgedWrap := good("alice", "k")
	fw, fct, _ := seal(ctx, foreign.kek, "alice", "k", []byte(value))
	forgedWrap.wrapped, forgedWrap.ct = fw, fct // right kek_id, wrong key
	v0 := good("alice", "k")
	v0.version = 0
	newer := good("alice", "k")
	newer.version = 3
	otherKEK := good("alice", "k")
	otherKEK.kekID = "transit:secret/wardyn"

	cases := map[string]struct {
		e    envelope
		want string
	}{
		"moved to another owner":                      {moved(good("alice", "k"), "bob", "k"), "refused"},
		"moved to the operator":                       {moved(good("alice", "k"), "", "k"), "refused"},
		"moved to another name":                       {moved(good("alice", "k"), "alice", "k2"), "refused"},
		"forged under a foreign key but labeled ours": {forgedWrap, "refused"},
		"a newer enc_version":                         {newer, "has enc_version 3 which this wardynd does not understand; upgrade wardynd"},
		"a v0 row":                                    {v0, "an older wardynd is still writing"},
		"an unconfigured KEK":                         {otherKEK, `"transit:secret/wardyn"`},
		"value swapped, wrap intact":                  {func() envelope { e := good("alice", "k"); e.ct = good("alice", "k").ct; return e }(), "integrity"},
	}
	for label, c := range cases {
		got, err := s.open(ctx, c.e)
		if err == nil {
			t.Errorf("%s: opened (%q)", label, got)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, c.want) || !strings.Contains(msg, rowRef(c.e.ownedBy, c.e.name)) {
			t.Errorf("%s: error %q does not name the row and %q", label, msg, c.want)
		}
		if strings.Contains(msg, value) {
			t.Errorf("%s: error carries the value", label)
		}
		if errors.Is(err, secretstore.ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("%s: a refusal surfaced as not-found (rule 9)", label)
		}
	}
}
