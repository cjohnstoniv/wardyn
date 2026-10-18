// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Record refuses an outcome the audit table cannot store before it touches the
// pool, and counts it: the backstop behind each producer's own tests.
func TestRecorder_RefusesUnstorableOutcomes(t *testing.T) {
	for _, outcome := range []string{"warn", "", "error", "Denied"} {
		before := RefusedAuditOutcomes()
		err := Recorder{}.Record(context.Background(), types.AuditEvent{Action: "run.artifact.redirect", Outcome: outcome})
		if !errors.Is(err, ErrAuditOutcome) {
			t.Errorf("outcome %q: err = %v, want ErrAuditOutcome", outcome, err)
		}
		if got := RefusedAuditOutcomes(); got != before+1 {
			t.Errorf("outcome %q: counter %d → %d, want one more", outcome, before, got)
		}
	}
}
