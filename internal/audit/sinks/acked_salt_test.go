// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAckedSink_EncodesNoHashSalt: the acknowledged sink's batch, built from rows the store reads for it
// (types.FederatedAuditEvent), has no hash_format, hash_salt or claim_salt key.
func TestAckedSink_EncodesNoHashSalt(t *testing.T) {
	rows := []types.FederatedAuditEvent{{AuditEvent: types.AuditEvent{ID: uuid.New(), Action: "run.create", Outcome: "success",
		ActorType: types.ActorSystem, RowHash: strings.Repeat("a", 64), PrevHash: strings.Repeat("b", 64)}, Seq: 1}}
	b, err := encodeAckedBatch(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"hash_salt", "claim_salt", "hash_format"} {
		if strings.Contains(string(b), key) {
			t.Errorf("an acknowledged-sink line carries %q: %s", key, b)
		}
	}
}
