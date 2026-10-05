// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// miscCovFixedKeys is a subject-key service that has one 32-byte key for every subject.
type miscCovFixedKeys struct{ key []byte }

func (k miscCovFixedKeys) Current(context.Context, string, string) (uuid.UUID, []byte, error) {
	return uuid.Nil, append([]byte(nil), k.key...), nil
}

func (k miscCovFixedKeys) Key(context.Context, string, uuid.UUID) ([]byte, error) {
	return append([]byte(nil), k.key...), nil
}

// With a subject key to seal under, a row with a personal field reaches the recorder below sealed (never
// pending, never in the clear), and the Sealer that wrote it opens it again.
func TestMiscCovSealingRecorderPassesASealedRowDownWhenTheKeyIsHad(t *testing.T) {
	inner := &captureRecorder{}
	src := newAuditSealSource(audit.SealFields)
	sealer := &audit.Sealer{Keys: miscCovFixedKeys{key: make([]byte, 32)}}
	src.arm(sealer)

	row := decideRow("a private reason")
	if err := (sealingRecorder{inner: inner, src: src}).Record(t.Context(), row); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(inner.evs) != 1 {
		t.Fatalf("the recorder below got %d rows, want 1", len(inner.evs))
	}
	stored := inner.evs[0]
	if strings.Contains(string(stored.Data), "a private reason") {
		t.Errorf("the personal field reached the recorder in the clear: %s", stored.Data)
	}
	if audit.IsPending(stored) {
		t.Error("a row sealed under a real key was marked pending")
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(stored.Data, &data); err != nil {
		t.Fatal(err)
	}
	if string(data["reason"]) == `"a private reason"` || len(data["reason"]) == 0 {
		t.Errorf("reason = %s, want it sealed", data["reason"])
	}

	opened, err := src.unsealer().Unseal(t.Context(), []types.AuditEvent{stored})
	if err != nil || len(opened) != 1 || !strings.Contains(string(opened[0].Data), "a private reason") {
		t.Fatalf("unsealed = %+v, %v; want the reason back", opened, err)
	}
}
