// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	secretstorepg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
)

// A failed commit is recorded as reason unknown, not aborted: its reply can be
// lost after the commit landed. Every other failure is an abort. Both
// secret.rewrap writers record it so, wrapped or not.
func TestRewrapAudit_ACommitErrorIsUnknownNotAborted(t *testing.T) {
	commit := fmt.Errorf("pg secretstore: rewrap commit (2 rows, 0 principal keys): %w: %w", secretstorepg.ErrCommitOutcomeUnknown, errors.New("connection reset"))
	for _, tc := range []struct {
		err  error
		want string
	}{
		{commit, "unknown"},
		{fmt.Errorf("pg secretstore: sealing to principal keys ABORTED at a row after moving 1 rows: %w", commit), "unknown"},
		{errors.New("pg secretstore: rewrap ABORTED after 1 of 2 rows"), "aborted"},
	} {
		rec := &capturingRecorder{}
		emitRewrapAudit(t.Context(), rec, secretstorepg.RewrapResult{}, false, tc.err)
		emitRewrapPrincipalKeysAudit(t.Context(), rec, secretstorepg.SealToPrincipalKeysResult{}, tc.err)
		if len(rec.got) != 2 {
			t.Fatalf("%d audit rows for %q, want 2", len(rec.got), tc.err)
		}
		for _, ev := range rec.got {
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil || ev.Outcome != "failure" || data["reason"] != tc.want {
				t.Errorf("%s for %q: outcome %q, data %s (%v); want failure with reason %q", ev.Actor, tc.err, ev.Outcome, ev.Data, err, tc.want)
			}
		}
	}
}
