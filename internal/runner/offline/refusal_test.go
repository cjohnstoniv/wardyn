// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A reason with invalid bytes is cleaned whatever its length, so the org's decoded copy cannot outgrow
// MaxRefusalReason, and an emptied reason still says something.
func TestFitRefusalReasonDropsInvalidUTF8(t *testing.T) {
	r := runnerwire.ActionResult{Reason: "bad\xff\xfebytes"}
	fitRefusalReason(&r)
	if r.Reason != "badbytes" || !utf8.ValidString(r.Reason) {
		t.Errorf("reason = %q", r.Reason)
	}
	r = runnerwire.ActionResult{Reason: "\xff\xff"}
	fitRefusalReason(&r)
	if r.Reason != "refused" {
		t.Errorf("an all-invalid reason = %q, want refused", r.Reason)
	}
	r = runnerwire.ActionResult{Reason: strings.Repeat("a", runnerwire.MaxRefusalReason+10)}
	fitRefusalReason(&r)
	if len(r.Reason) > runnerwire.MaxRefusalReason {
		t.Errorf("an over-long reason kept %d bytes", len(r.Reason))
	}
}

// An escape-heavy reason encodes to six bytes a byte; the fit keeps its head
// instead of collapsing to "refused".
func TestFitRefusalReason_EscapeHeavyKeepsItsHead(t *testing.T) {
	r := runnerwire.ActionResult{Outcome: types.RunnerActionRefused, Reason: strings.Repeat("\x01", runnerwire.MaxRefusalReason)}
	fitRefusalReason(&r)
	b, err := json.Marshal(r)
	if err != nil || len(b) > runnerwire.MaxActionResultBytes {
		t.Fatalf("encoded %d bytes (err %v), want <= %d", len(b), err, runnerwire.MaxActionResultBytes)
	}
	if r.Reason == "refused" || len(r.Reason) < 100 {
		t.Fatalf("reason collapsed to %q (%d bytes)", r.Reason, len(r.Reason))
	}
}
