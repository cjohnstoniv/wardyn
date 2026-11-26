// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// countFilterStore is recordingRunsStore that also keeps the filter of every hidden-count read.
type countFilterStore struct {
	*recordingRunsStore
	cmu     sync.Mutex
	counted []store.RunFilter
}

func (s *countFilterStore) CountHiddenRuns(ctx context.Context, f store.RunFilter) (int, int, error) {
	s.cmu.Lock()
	s.counted = append(s.counted, f)
	s.cmu.Unlock()
	return s.recordingRunsStore.CountHiddenRuns(ctx, f)
}

var _ store.RunsFilteredPager = (*countFilterStore)(nil)

func endedAfterFixture(t *testing.T) (*Server, *countFilterStore) {
	t.Helper()
	ast := newAuthzStore()
	rec := &countFilterStore{recordingRunsStore: &recordingRunsStore{fakeRunsFilteredStore: &fakeRunsFilteredStore{ast}}}
	h := newHarness(t)
	cfg := baseTestConfig(h, rec)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Approvals = newAuthzApprovals(ast)
	return New(cfg), rec
}

// ended_after reaches the ended arm of the list read and is echoed; the hidden counts never read it.
func TestListRunsFiltered_EndedAfterReachesTheEndedArm(t *testing.T) {
	srv, rec := endedAfterFixture(t)
	sess := ssoSession(t, "sub-cut", "cut@corp.example", oidc.RoleUser)

	w := doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user&ended_after=2026-11-25T10:00:00.5Z", sess, "")
	if w.Code != http.StatusOK {
		t.Fatalf("ended_after: status = %d, body=%s", w.Code, w.Body.String())
	}
	want := time.Date(2026, 11, 25, 10, 0, 0, 5e8, time.UTC)
	listed := rec.seen()
	if len(listed) != 1 || !listed[0].EndedAfter.Equal(want) {
		t.Errorf("the list read saw %+v, want one filter with EndedAfter %v", listed, want)
	}
	rec.cmu.Lock()
	counted := append([]store.RunFilter(nil), rec.counted...)
	rec.cmu.Unlock()
	if len(counted) != 1 || !counted[0].EndedAfter.IsZero() {
		t.Errorf("the hidden-count read saw %+v, want one filter with a zero EndedAfter", counted)
	}
	if got := w.Header().Get("X-Wardyn-Ended-After"); got != "2026-11-25T10:00:00.5Z" {
		t.Errorf("X-Wardyn-Ended-After = %q, want 2026-11-25T10:00:00.5Z", got)
	}

	plain := doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user", sess, "")
	if plain.Code != http.StatusOK {
		t.Fatalf("no ended_after: status = %d, body=%s", plain.Code, plain.Body.String())
	}
	listed = rec.seen()
	if len(listed) != 2 || !listed[1].EndedAfter.IsZero() {
		t.Errorf("the read without ended_after saw %+v, want a zero EndedAfter", listed)
	}
	if _, ok := plain.Header()["X-Wardyn-Ended-After"]; ok {
		t.Error("a read without ended_after sent X-Wardyn-Ended-After")
	}
}

// A cut that is not an RFC 3339 time is refused with its own reason; an empty one is no cut.
func TestListRunsFiltered_EndedAfterRefusesAMalformedTime(t *testing.T) {
	srv, _ := endedAfterFixture(t)
	sess := ssoSession(t, "sub-cut", "cut@corp.example", oidc.RoleUser)
	for _, v := range []string{"yesterday", "2026-11-25"} {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user&ended_after="+v, sess, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("ended_after=%s: status = %d, want 400; body=%s", v, w.Code, w.Body.String())
		}
		if got := errorReason(w); got != reasonInvalidEndedAfterParam {
			t.Errorf("ended_after=%s: reason = %q, want %q", v, got, reasonInvalidEndedAfterParam)
		}
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user&ended_after=", sess, ""); w.Code != http.StatusOK {
		t.Errorf("ended_after= (empty): status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// status=needs answers live rows only, so it never reads the cut and never echoes it.
func TestListRunsFiltered_NeedsIgnoresEndedAfter(t *testing.T) {
	srv, rec := endedAfterFixture(t)
	sess := ssoSession(t, "sub-cut", "cut@corp.example", oidc.RoleUser)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/runs?view=user&status=needs&ended_after=2026-11-25T10:00:00Z", sess, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if _, ok := w.Header()["X-Wardyn-Ended-After"]; ok {
		t.Error("status=needs echoed X-Wardyn-Ended-After")
	}
	for _, f := range rec.seen() {
		if !f.EndedAfter.IsZero() {
			t.Errorf("status=needs read with EndedAfter %v", f.EndedAfter)
		}
	}
}
