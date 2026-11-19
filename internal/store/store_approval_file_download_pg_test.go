// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The Postgres halves of a reviewed download: an approved file_download row is spent once, for that run and that
// file only, and never through another kind's spend; one review per file name waits at a time. Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const fdSum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func fdScope(name, sum string, n int64) types.FileDownloadScope {
	return types.FileDownloadScope{Name: name, Bytes: n, SHA256: sum, RequestedBy: "sub-asker"}
}

func raiseFileDownload(t *testing.T, ctx context.Context, st approvalStore, runID uuid.UUID, sc types.FileDownloadScope) (types.ApprovalRequest, error) {
	t.Helper()
	raw, _ := json.Marshal(sc)
	return approval.RequestApproval(ctx, st, types.ApprovalRequest{RunID: runID, Kind: types.ApprovalFileDownload, RequestedScope: raw})
}

func TestPG_SpendFileDownloadApproval_KindAndFileBound(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	st := approvalStore{pg}
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	other := persistRun(t, ctx, pool, newRun(types.RunRunning))
	want := fdScope("report.csv", fdSum, 17)

	ap, err := raiseFileDownload(t, ctx, st, run.ID, want)
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if spent, err := pg.SpendFileDownloadApproval(ctx, ap.ID, run.ID, want, "jti-early"); err != nil || spent {
		t.Fatalf("spent a PENDING review: %v %v", spent, err)
	}
	if _, err := pg.DecideApproval(ctx, ap.ID, types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: "sub-sec", Scope: types.ScopeOnce}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	// The tool_call spend never spends a file_download row.
	if spent, err := pg.SpendApprovalOnce(ctx, ap.ID, "jti-ado"); err != nil || spent {
		t.Fatalf("the tool_call spend spent a file_download row: %v %v", spent, err)
	}
	for name, c := range map[string]struct {
		run uuid.UUID
		sc  types.FileDownloadScope
	}{
		"another run":  {other.ID, want},
		"another name": {run.ID, fdScope("other.csv", fdSum, 17)},
		"another hash": {run.ID, fdScope("report.csv", "f"+fdSum[1:], 17)},
		"another size": {run.ID, fdScope("report.csv", fdSum, 18)},
	} {
		if spent, err := pg.SpendFileDownloadApproval(ctx, ap.ID, c.run, c.sc, "jti-"+name); err != nil || spent {
			t.Fatalf("%s: spent (%v, %v)", name, spent, err)
		}
	}
	if _, err := pg.SpendFileDownloadApproval(ctx, ap.ID, run.ID, want, ""); err == nil {
		t.Fatal("an empty jti was accepted")
	}

	const n = 16
	var wg sync.WaitGroup
	wins := make([]bool, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			spent, err := pg.SpendFileDownloadApproval(ctx, ap.ID, run.ID, want, "jti-"+strconv.Itoa(i))
			if err != nil {
				t.Errorf("spend %d: %v", i, err)
			}
			wins[i] = spent
		}(i)
	}
	close(start)
	wg.Wait()
	won := 0
	for _, w := range wins {
		if w {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d spends won, want exactly 1", won)
	}
}

// A file_download spend never spends another kind's approved row, even one whose scope names the same file.
func TestPG_SpendFileDownloadApproval_OtherKindRefused(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	st := approvalStore{pg}
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	want := fdScope("report.csv", fdSum, 17)
	raw, _ := json.Marshal(want)
	ap, err := approval.RequestApproval(ctx, st, types.ApprovalRequest{RunID: run.ID, Kind: types.ApprovalToolCall, RequestedScope: raw})
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if _, err := pg.DecideApproval(ctx, ap.ID, types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: "sub-sec", Scope: types.ScopeOnce}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if spent, err := pg.SpendFileDownloadApproval(ctx, ap.ID, run.ID, want, "jti-x"); err != nil || spent {
		t.Fatalf("a tool_call row was spent as a download: %v %v", spent, err)
	}
}

// A denied review is never spent.
func TestPG_SpendFileDownloadApproval_DeniedNeverSpent(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	want := fdScope("report.csv", fdSum, 17)
	ap, err := raiseFileDownload(t, ctx, approvalStore{pg}, run.ID, want)
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if _, err := pg.DecideApproval(ctx, ap.ID, types.ApprovalDecision{State: types.ApprovalDenied, DecidedBy: "sub-sec"}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if spent, err := pg.SpendFileDownloadApproval(ctx, ap.ID, run.ID, want, "jti-x"); err != nil || spent {
		t.Fatalf("a denied review was spent: %v %v", spent, err)
	}
}

// One review per file name waits at a time: a second ask for the same name with other bytes is refused by the
// index, an ask for the same bytes is the same row, and once the first is decided a new one may wait.
func TestPG_FileDownloadOnePendingPerName(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	st := approvalStore{pg}
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	first, err := raiseFileDownload(t, ctx, st, run.ID, fdScope("report.csv", fdSum, 17))
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if again, err := raiseFileDownload(t, ctx, st, run.ID, fdScope("report.csv", fdSum, 17)); err != nil || again.ID != first.ID {
		t.Fatalf("the same file raised another row: %v %v", again.ID, err)
	}
	if _, err := raiseFileDownload(t, ctx, st, run.ID, fdScope("report.csv", "f"+fdSum[1:], 18)); !errors.Is(err, types.ErrDuplicatePendingApproval) {
		t.Fatalf("a changed file under a pending review = %v, want ErrDuplicatePendingApproval", err)
	}
	if _, err := raiseFileDownload(t, ctx, st, run.ID, fdScope("other.csv", fdSum, 17)); err != nil {
		t.Fatalf("another name: %v", err)
	}
	if _, err := pg.DecideApproval(ctx, first.ID, types.ApprovalDecision{State: types.ApprovalDenied, DecidedBy: "sub-sec"}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if _, err := raiseFileDownload(t, ctx, st, run.ID, fdScope("report.csv", "f"+fdSum[1:], 18)); err != nil {
		t.Fatalf("a new review after the first was decided: %v", err)
	}
}
