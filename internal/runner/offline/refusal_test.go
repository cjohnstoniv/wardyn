// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

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
