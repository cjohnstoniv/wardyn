// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLeaseOutageFieldsRoundTrip(t *testing.T) {
	in := Lease{ExecutionGeneration: uuid.New(), RunID: uuid.New(), TokenRenewedAt: time.Unix(1700000000, 0).UTC(),
		EndsAt: time.Unix(1700003600, 0).UTC(), OutageAllowanceS: 1800, OutageStrict: true}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Lease
	if err := json.Unmarshal(b, &out); err != nil || out != in {
		t.Fatalf("round trip = %+v %v, want %+v", out, err, in)
	}
	def, err := json.Marshal(Lease{RunID: uuid.New()})
	if err != nil || strings.Contains(string(def), "outage") {
		t.Fatalf("a default lease marshals outage keys: %s %v", def, err)
	}
}

// TestLeaseDecodesV0102Shape: a lease from a previous release, without the outage keys, decodes with zero terms.
func TestLeaseDecodesV0102Shape(t *testing.T) {
	old := `{"execution_generation":"` + uuid.NewString() + `","run_id":"` + uuid.NewString() + `","token_renewed_at":"2026-11-01T00:00:00Z","ends_at":"2026-11-01T01:00:00Z"}`
	var l Lease
	if err := json.Unmarshal([]byte(old), &l); err != nil {
		t.Fatal(err)
	}
	if l.OutageAllowanceS != 0 || l.OutageStrict {
		t.Fatalf("an older lease decoded with outage terms: %+v", l)
	}
}
