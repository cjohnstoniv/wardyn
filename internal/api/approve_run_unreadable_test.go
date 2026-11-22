// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// approveBoundaryStore is the walls fake, which reads only the runs it was given, with a Wardyn boundary.
type approveBoundaryStore struct {
	*wallsRunStore
	b types.Boundary
}

func (s *approveBoundaryStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, nil }

func (s *approveBoundaryStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, nil
}

// approveRestrictionStore is the walls fake with a restriction store. The gate reads the run before any
// restriction, so the embedded store, which is nil, is never called.
type approveRestrictionStore struct {
	*wallsRunStore
	store.RestrictionStore
}

// TestApproveBoundsAnswerAnUnreadableRunOneWay: whichever approve gate reads the run first (the boundary's in
// hard_limits, the restrictions', the walls'), a run that cannot be read is 503 run_unreadable with one
// sentence, and the approval is not admitted.
func TestApproveBoundsAnswerAnUnreadableRunOneWay(t *testing.T) {
	h := newHarness(t)
	uv := &uvStore{permStore: &permStore{capStore: &capStore{userTypes: utKnown}}, prefs: map[string]json.RawMessage{}}
	runs := &wallsRunStore{uvStore: uv, runs: map[uuid.UUID]types.AgentRun{}} // GetRun answers ErrNotFound for every id
	ap := types.ApprovalRequest{RunID: uuid.New(), Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`{"host":"pypi.org"}`)}
	for _, c := range []struct {
		name string
		st   store.Store
		gate func(*Server, http.ResponseWriter, *http.Request) bool
	}{
		{"the boundary in hard_limits", &approveBoundaryStore{wallsRunStore: runs, b: types.Boundary{Mode: types.BoundaryHardLimits}},
			func(s *Server, w http.ResponseWriter, r *http.Request) bool {
				_, _, ok := s.boundaryAdmitApproval(w, r, ap, types.AgentRun{}, false)
				return ok
			}},
		{"the restrictions", &approveRestrictionStore{wallsRunStore: runs},
			func(s *Server, w http.ResponseWriter, r *http.Request) bool {
				_, _, ok := s.restrictionAdmitApproval(w, r, ap, types.AgentRun{}, false)
				return ok
			}},
		{"the walls", runs,
			func(s *Server, w http.ResponseWriter, r *http.Request) bool {
				_, _, ok := s.wallsAdmitApproval(w, r, ap, types.AgentRun{}, false)
				return ok
			}},
	} {
		srv := New(baseTestConfig(h, c.st))
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/x/approve", nil).WithContext(memberCtxFor("sub-approver"))
		if c.gate(srv, w, r) || w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonRunUnreadable ||
			!strings.Contains(w.Body.String(), "could not be read to check its network preset") {
			t.Errorf("%s: %d %s, want 503 %s with the walls gate's sentence", c.name, w.Code, w.Body, reasonRunUnreadable)
		}
	}
}
