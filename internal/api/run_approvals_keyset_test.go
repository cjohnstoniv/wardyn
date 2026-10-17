// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keysetApprovals is a store whose by-run keyset read serves a fixed newest-first list, and records
// each call: the run, the kinds, the key it resumed from and the page size it asked for.
type keysetApprovals struct {
	store.Store
	rows  []types.ApprovalRequest
	calls []keysetCall
}

type keysetCall struct {
	run     uuid.UUID
	kinds   []types.ApprovalKind
	afterID uuid.UUID
	limit   int
}

func (k *keysetApprovals) ListApprovalsKeysetByRun(_ context.Context, runID uuid.UUID, _ types.ApprovalState, kinds []types.ApprovalKind,
	_ time.Time, afterID uuid.UUID, limit int,
) ([]types.ApprovalRequest, error) {
	k.calls = append(k.calls, keysetCall{runID, kinds, afterID, limit})
	start := 0
	if afterID != uuid.Nil {
		for i, r := range k.rows {
			if r.ID == afterID {
				start = i + 1
			}
		}
	}
	end := min(start+limit, len(k.rows))
	return k.rows[start:end], nil
}

// FUG-L699 N4: the per-run approvals read pages by keyset, asks only for the kinds its callers read,
// and returns every row once, across a page boundary and on an exact multiple of the page size.
func TestRunApprovalsReadsByKeysetInBoundedPages(t *testing.T) {
	for _, n := range []int{0, 1, runApprovalsPage - 1, runApprovalsPage, runApprovalsPage + 1, 2*runApprovalsPage + 7} {
		run := uuid.New()
		st := &keysetApprovals{}
		for range n {
			st.rows = append(st.rows, types.ApprovalRequest{ID: uuid.New(), RunID: run, Kind: types.ApprovalCredentialReauth})
		}
		s := &Server{cfg: Config{Store: st}}

		got, err := s.runApprovals(t.Context(), run, "")
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if len(got) != n {
			t.Fatalf("n=%d: read %d rows", n, len(got))
		}
		seen := map[uuid.UUID]bool{}
		for _, r := range got {
			if seen[r.ID] {
				t.Fatalf("n=%d: row %s read twice", n, r.ID)
			}
			seen[r.ID] = true
		}
		wantCalls := n/runApprovalsPage + 1
		if len(st.calls) != wantCalls {
			t.Errorf("n=%d: %d round trips, want %d", n, len(st.calls), wantCalls)
		}
		for i, c := range st.calls {
			if c.limit != runApprovalsPage || c.run != run || len(c.kinds) != 2 || (i == 0) != (c.afterID == uuid.Nil) {
				t.Errorf("n=%d call %d = %+v, want a bounded page of this run's two kinds, resumed after the last row", n, i, c)
			}
		}
	}
}
