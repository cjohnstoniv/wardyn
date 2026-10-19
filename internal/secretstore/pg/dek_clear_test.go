// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"bytes"
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// retainKEK keeps a reference to every data key it is handed or hands out, so a
// test can see whether the caller cleared it after use.
type retainKEK struct{ deks [][]byte }

func (*retainKEK) ID() string { return "retain" }

func (k *retainKEK) Wrap(_ context.Context, dek []byte, _ map[string]string) ([]byte, error) {
	k.deks = append(k.deks, dek)
	return bytes.Clone(dek), nil
}

func (k *retainKEK) Unwrap(context.Context, []byte, map[string]string) ([]byte, error) {
	dek := bytes.Repeat([]byte{0x5a}, kek.DEKSize)
	k.deks = append(k.deks, dek)
	return dek, nil
}

func (k *retainKEK) requireCleared(t *testing.T) {
	t.Helper()
	if len(k.deks) == 0 {
		t.Fatal("no data key passed through the KEK")
	}
	for i, dek := range k.deks {
		if !bytes.Equal(dek, make([]byte, len(dek))) {
			t.Errorf("data key %d is still in memory after the call returned", i)
		}
	}
}

// TestSealAndRewrap_ClearTheDataKey pins pg.go's promise that a DEK is in memory
// only during the call: seal and rewrap zero it before they return.
func TestSealAndRewrap_ClearTheDataKey(t *testing.T) {
	ctx := context.Background()
	k := &retainKEK{}
	if _, _, err := seal(ctx, k, "alice@corp.example", "n", []byte("v")); err != nil {
		t.Fatalf("seal: %v", err)
	}
	k.requireCleared(t)

	from, to := &retainKEK{}, &retainKEK{}
	e := envelope{ownedBy: "alice@corp.example", name: "n", version: encVersion, wrapped: []byte("w")}
	if _, err := rewrap(ctx, func(envelope) (kek.KEK, error) { return from, nil }, to, e); err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	from.requireCleared(t)
	to.requireCleared(t)
}
