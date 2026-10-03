// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subjectkey_test

import (
	"testing"
	"time"

	"filippo.io/age"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
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
