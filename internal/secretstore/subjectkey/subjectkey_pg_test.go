// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subjectkey_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
)

func TestSubjectKeys_LocalContract(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	subjectkeytest.Run(t, pool, func(t *testing.T) kek.KEK {
		k, err := kek.NewLocalPurpose(id, kek.PurposeCred)
		if err != nil {
			t.Fatal(err)
		}
		return k
	})
}

// The table refuses what the package never writes, for any other writer.
func TestPrincipalKeysTableConstraints(t *testing.T) {
	gone := time.Now().UTC()
	pool := subjectkeytest.ThrowawayDB(t)
	ctx := t.Context()
	const ins = `INSERT INTO principal_keys (owner, purpose, version, domain, kek_id, wrapped_key, destroyed_at) VALUES ($1, $2, $3, 'default', 'k', $4, $5)`
	if _, err := pool.Exec(ctx, ins, "a", "cred", 1, []byte{1}, nil); err != nil {
		t.Fatalf("a live row with a wrapped key: %v", err)
	}
	for name, args := range map[string][]any{
		"a live row with no wrapped key":        {"b", "cred", 1, nil, nil},
		"a destroyed row still holding its key": {"b", "cred", 1, []byte{1}, gone},
		"an unknown purpose":                    {"b", "audit", 1, []byte{1}, nil},
		"a second live generation":              {"a", "cred", 2, []byte{1}, nil},
		"a duplicate (owner, purpose, version)": {"a", "cred", 1, nil, gone},
		"a version below 1":                     {"b", "cred", 0, []byte{1}, nil},
	} {
		if _, err := pool.Exec(ctx, ins, args...); err == nil {
			t.Errorf("the table accepted %s", name)
		}
	}
	if _, err := pool.Exec(ctx, ins, "a", "cred", 2, nil, gone); err != nil {
		t.Fatalf("a tombstone beside the live generation: %v", err)
	}
}

// A generation's handle names it to any instance, for its own purpose only;
// Destroy clears it from the table, so it names nothing after; the next
// generation gets a fresh one.
func TestSubjectKeyHandles(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	k, err := kek.NewLocalPurpose(id, kek.PurposeCred)
	if err != nil {
		t.Fatal(err)
	}
	a, b := subjectkeytest.Manager(pool, k), subjectkeytest.Manager(pool, k)
	ctx := t.Context()
	const owner, purpose = "hal@corp.test", subjectkey.PurposeAuditSeal

	h, key, err := a.CurrentHandle(ctx, owner, purpose)
	if err != nil || h == uuid.Nil {
		t.Fatalf("CurrentHandle = %s, %v", h, err)
	}
	if again, _, err := a.CurrentHandle(ctx, owner, purpose); err != nil || again != h {
		t.Fatalf("a second CurrentHandle = %s, %v; want %s", again, err, h)
	}
	if other, _, err := a.CurrentHandle(ctx, "ivy@corp.test", purpose); err != nil || other == h {
		t.Fatalf("another owner's handle = %s, %v; want a different one", other, err)
	}
	if got, err := b.KeyByHandle(ctx, purpose, h); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("KeyByHandle on another instance = %v; want the same key", err)
	}
	if _, err := b.KeyByHandle(ctx, subjectkey.PurposeCred, h); !errors.Is(err, subjectkey.ErrDataLoss) {
		t.Errorf("KeyByHandle under another purpose = %v, want ErrDataLoss", err)
	}
	if _, err := b.KeyByHandle(ctx, purpose, uuid.New()); !errors.Is(err, subjectkey.ErrDataLoss) {
		t.Errorf("KeyByHandle of a handle never issued = %v, want ErrDataLoss", err)
	}

	if _, err := a.Destroy(ctx, owner, purpose); err != nil {
		t.Fatal(err)
	}
	var holders int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM principal_keys WHERE handle = $1`, h).Scan(&holders); err != nil || holders != 0 {
		t.Errorf("%d rows still carry a destroyed generation's handle (%v), want none", holders, err)
	}
	for name, m := range map[string]*subjectkey.Manager{"warm": a, "other": b} {
		if _, err := m.KeyByHandle(ctx, purpose, h); !errors.Is(err, subjectkey.ErrDataLoss) {
			t.Errorf("%s instance: KeyByHandle after Destroy = %v, want ErrDataLoss", name, err)
		}
	}
	if next, _, err := a.CurrentHandle(ctx, owner, purpose); err != nil || next == h || next == uuid.Nil {
		t.Errorf("the next generation's handle = %s, %v; want a fresh one", next, err)
	}
}
