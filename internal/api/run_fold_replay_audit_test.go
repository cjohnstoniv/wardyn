// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestStepPolicy_AReplayedStartWritesNoSecondPolicyAuditRow: the create of a queued run already staged
// policy.inline.apply once; the promoted run's start folds the same request again and must not add a
// second row (which would carry no run id and be read as a second authorisation).
func TestStepPolicy_AReplayedStartWritesNoSecondPolicyAuditRow(t *testing.T) {
	h := newHarness(t)
	fold := func(ctx context.Context, promoted bool) int {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/v1/runs", nil).WithContext(ctx)
		f := &runFold{
			mode: foldCreate, w: httptest.NewRecorder(), r: r, promoted: promoted,
			req: &createRunRequest{Agent: "claude-code", Repo: "acme/widgets", InlinePolicy: &types.RunPolicySpec{MinConfinementClass: types.CC2}},
		}
		if !h.srv.stepPolicy(f, &foldRecorder{}) {
			t.Fatalf("stepPolicy refused: %s", f.w.(*httptest.ResponseRecorder).Body.String())
		}
		// Create writes the staged rows with the run's insert; a promoted start stages none.
		n := 0
		for _, ev := range f.source.applyAudits {
			if ev.Action == "policy.inline.apply" {
				n++
			}
		}
		return n
	}
	if n := fold(context.Background(), false); n != 1 {
		t.Fatalf("a create fold staged %d policy.inline.apply rows, want 1", n)
	}
	if n := fold(withPromotedStart(context.Background(), uuid.New(), nil, "", false), true); n != 0 {
		t.Fatalf("a promoted start's fold staged %d policy.inline.apply rows, want 0", n)
	}
}
