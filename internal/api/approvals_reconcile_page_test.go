// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ListDecidedAlwaysEgressPage is the fake's copy of store.PG's projection: the
// same filter, order and keyset, and no reason on the rows it returns.
func (a *authzApprovals) ListDecidedAlwaysEgressPage(_ context.Context, state types.ApprovalState, afterAt time.Time, afterID uuid.UUID, limit int) ([]types.ApprovalRequest, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	newerFirst := func(x, y types.ApprovalRequest) int {
		if c := y.RequestedAt.Compare(x.RequestedAt); c != 0 {
			return c
		}
		return bytes.Compare(y.ID[:], x.ID[:])
	}
	var out []types.ApprovalRequest
	for _, ap := range a.byID {
		if ap.State != state || ap.Kind != types.ApprovalEgressDomain || ap.DecisionScope != types.ScopeAlways {
			continue
		}
		if afterID != uuid.Nil && newerFirst(ap, types.ApprovalRequest{ID: afterID, RequestedAt: afterAt}) <= 0 {
			continue
		}
		out = append(out, types.ApprovalRequest{
			ID: ap.ID, RunID: ap.RunID, Kind: ap.Kind, RequestedScope: ap.RequestedScope, State: ap.State,
			RequestedAt: ap.RequestedAt, DecidedAt: ap.DecidedAt, DecisionScope: ap.DecisionScope,
		})
	}
	slices.SortFunc(out, newerFirst)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// countingApprovals counts the heal's reads: every whole-row List, and every
// projection page, which it caps at pageCap rows to force a multi-page walk.
type countingApprovals struct {
	*authzApprovals
	pageCap     int
	listCalls   int
	pageCalls   int
	rowsCounted int
}

func (c *countingApprovals) List(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	c.listCalls++
	return c.authzApprovals.List(ctx, state)
}

func (c *countingApprovals) ListDecidedAlwaysEgressPage(ctx context.Context, state types.ApprovalState, afterAt time.Time, afterID uuid.UUID, limit int) ([]types.ApprovalRequest, error) {
	c.pageCalls++
	if c.pageCap > 0 && limit > c.pageCap {
		limit = c.pageCap
	}
	aps, err := c.authzApprovals.ListDecidedAlwaysEgressPage(ctx, state, afterAt, afterID, limit)
	c.rowsCounted += len(aps)
	return aps, err
}

func countReads(f *scopeFixture, pageCap int) *countingApprovals {
	c := &countingApprovals{authzApprovals: f.approval, pageCap: pageCap}
	f.srv.cfg.Approvals = c
	return c
}

// rcSeedAt seeds a decided egress approval with its own request time, decision
// time, scope and reason.
func rcSeedAt(f *scopeFixture, host string, state types.ApprovalState, scope types.ApprovalScope, requestedAt, decidedAt time.Time, reason string) {
	id := uuid.New()
	at := decidedAt
	f.approval.mu.Lock()
	f.approval.byID[id] = types.ApprovalRequest{
		ID: id, RunID: f.runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":` + strconv.Quote(host) + `}`),
		State:          state, DecisionScope: scope, DecidedAt: &at, RequestedAt: requestedAt, Reason: reason,
	}
	f.approval.mu.Unlock()
}

// TestReconcileReadsOnlyTheAlwaysProjection: the heal never lists decided rows
// in full. Run-scoped denials with large reasons sit beside one approve·always;
// the heal replays that one verdict through the projection, never calls List,
// and reads none of the run-scoped rows.
func TestReconcileReadsOnlyTheAlwaysProjection(t *testing.T) {
	f := newScopeFixture(t)
	ws := f.seedWorkspace(t, nil, nil)
	now := time.Now().UTC()
	large := strings.Repeat("r", 64<<10)
	for i := range 20 {
		rcSeedAt(f, "h"+strconv.Itoa(i)+".example.com", types.ApprovalDenied, types.ScopeRun, now.Add(-time.Hour), now, large)
	}
	rcSeedAt(f, "registry.npmjs.org", types.ApprovalApproved, types.ScopeAlways, now.Add(-time.Hour), now, "ok")
	reads := countReads(f, 0)

	n, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if reads.listCalls != 0 {
		t.Fatalf("the heal listed decided approvals in full %d times; it must read only the projection", reads.listCalls)
	}
	if n != 1 || reads.rowsCounted != 1 {
		t.Fatalf("reconciled %d, projection rows read %d; want 1 and 1", n, reads.rowsCounted)
	}
	if approved, _ := f.egressLists(t, ws); !slices.Contains(approved, "registry.npmjs.org") {
		t.Fatalf("approved = %v, want the approve·always host", approved)
	}
}

// TestReconcileNewestWinsAcrossPages walks the projection two rows at a time.
// A host's newest verdict was REQUESTED first, so it is on a later page than the
// older verdict it overrides, and the deny direction is read after every approve
// page; the operator's list edit still drops verdicts older than it.
func TestReconcileNewestWinsAcrossPages(t *testing.T) {
	t0 := time.Now().UTC().Add(-24 * time.Hour)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }

	f := newScopeFixture(t)
	ws := f.seedWorkspace(t, nil, nil)
	edited := at(5)
	f.store.mu.Lock()
	w := f.store.workspaces[ws]
	w.EgressEditedAt = &edited
	f.store.workspaces[ws] = w
	f.store.mu.Unlock()

	// a.example.com: an older deny requested late, the newest approve requested first.
	rcSeedAt(f, "a.example.com", types.ApprovalDenied, types.ScopeAlways, at(10), at(11), "")
	rcSeedAt(f, "a.example.com", types.ApprovalApproved, types.ScopeAlways, at(0), at(12), "")
	// b.example.com: an older approve requested late, the newest deny requested first.
	rcSeedAt(f, "b.example.com", types.ApprovalApproved, types.ScopeAlways, at(9), at(10), "")
	rcSeedAt(f, "b.example.com", types.ApprovalDenied, types.ScopeAlways, at(0), at(13), "")
	// Filler approve·always verdicts so the walk takes several pages.
	for i := range 4 {
		rcSeedAt(f, "f"+strconv.Itoa(i)+".example.com", types.ApprovalApproved, types.ScopeAlways, at(8-i), at(9), "")
	}
	// Decided before the operator's edit at hour 5: skipped in both directions.
	rcSeedAt(f, "old-allow.example.com", types.ApprovalApproved, types.ScopeAlways, at(1), at(4), "")
	rcSeedAt(f, "old-deny.example.com", types.ApprovalDenied, types.ScopeAlways, at(1), at(4), "")
	reads := countReads(f, 2)

	if _, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// 7 approve·always rows in pages of 2 (2,2,2,1, then empty) and 3 deny·always
	// rows (2,1, then empty): 8 reads. A cursor taken from anywhere but the last
	// row of a page re-reads rows and takes more.
	if reads.pageCalls != 8 || reads.listCalls != 0 {
		t.Fatalf("page reads = %d, List calls = %d; want exactly 8 projection pages and no List", reads.pageCalls, reads.listCalls)
	}
	approved, denied := f.egressLists(t, ws)
	if !slices.Contains(approved, "a.example.com") || slices.Contains(denied, "a.example.com") {
		t.Errorf("a.example.com: approved=%v denied=%v, want approved (newest verdict)", approved, denied)
	}
	if !slices.Contains(denied, "b.example.com") || slices.Contains(approved, "b.example.com") {
		t.Errorf("b.example.com: approved=%v denied=%v, want denied (newest verdict)", approved, denied)
	}
	for i := range 4 {
		if h := "f" + strconv.Itoa(i) + ".example.com"; !slices.Contains(approved, h) {
			t.Errorf("filler %s missing from approved=%v", h, approved)
		}
	}
	if slices.Contains(approved, "old-allow.example.com") || slices.Contains(denied, "old-deny.example.com") {
		t.Errorf("a verdict older than the list edit was replayed: approved=%v denied=%v", approved, denied)
	}
}

// noPagerApprovals is an approval backend without the decided always-egress
// projection: embedding the interface promotes only ApprovalService's methods.
type noPagerApprovals struct {
	ApprovalService
	listCalls int
}

func (n *noPagerApprovals) List(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	n.listCalls++
	return n.ApprovalService.List(ctx, state)
}

// TestReconcileWithoutTheProjectionErrors: a backend without the projection
// makes the heal fail, and it never falls back to listing decided rows in full.
func TestReconcileWithoutTheProjectionErrors(t *testing.T) {
	f := newScopeFixture(t)
	ws := f.seedWorkspace(t, nil, nil)
	now := time.Now().UTC()
	rcSeedAt(f, "registry.npmjs.org", types.ApprovalApproved, types.ScopeAlways, now.Add(-time.Hour), now, "")
	backend := &noPagerApprovals{ApprovalService: f.approval}
	f.srv.cfg.Approvals = backend

	n, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context())
	if err == nil || n != 0 {
		t.Fatalf("reconcile = (%d, %v), want an error and nothing reconciled", n, err)
	}
	if backend.listCalls != 0 {
		t.Fatalf("the heal listed decided approvals in full %d times", backend.listCalls)
	}
	if approved, denied := f.egressLists(t, ws); len(approved) != 0 || len(denied) != 0 {
		t.Fatalf("approved=%v denied=%v, want both empty", approved, denied)
	}
}
