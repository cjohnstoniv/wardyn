// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// scopedApprovals is fakeApprovals plus the ownership-scoped pager the real
// backend implements, with the SAME JOIN predicate expressed in Go: an approval
// belongs to the human who created its run. Modelling the join rather than
// stubbing the answer is load-bearing — a double that returned a hand-picked
// list would pass whether the handler scoped or not.
type scopedApprovals struct {
	*fakeApprovals
	runCreator map[uuid.UUID]string
}

func (s *scopedApprovals) ListApprovalsPageByRunCreator(_ context.Context, createdBy string, state types.ApprovalState, p store.Page) ([]types.ApprovalRequest, error) {
	var out []types.ApprovalRequest
	for _, ap := range s.fakeApprovals.byID {
		if s.runCreator[ap.RunID] != createdBy {
			continue
		}
		if state != "" && ap.State != state {
			continue
		}
		out = append(out, ap)
	}
	if p.Offset >= len(out) {
		return nil, nil
	}
	out = out[p.Offset:]
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}

// ListApprovalsPage is the UNSCOPED pager the real adapter also promotes. It is
// here so that removing the ownership scoping produces the defect's own shape —
// a member served the whole fleet's queue, 200 OK — rather than an incidental
// 500 from a double that could not answer the fall-through at all.
func (s *scopedApprovals) ListApprovalsPage(_ context.Context, state types.ApprovalState, p store.Page) ([]types.ApprovalRequest, error) {
	var out []types.ApprovalRequest
	for _, ap := range s.fakeApprovals.byID {
		if state == "" || ap.State == state {
			out = append(out, ap)
		}
	}
	if p.Offset >= len(out) {
		return nil, nil
	}
	out = out[p.Offset:]
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}

// TestMemberApprovalListIsOwnershipScoped is F125's missing counterfactual.
//
// handleListApprovals narrows a member's UNSCOPED GET /approvals to approvals on
// runs they created, and fails CLOSED with 500 when the backend cannot do it.
// Neither arm was exercised anywhere in internal/api, internal/store or
// test/apie2e: deleting the scoping entirely — serving every member the whole
// fleet's approval queue — left the suite green. This is the test that reddens.
func TestMemberApprovalListIsOwnershipScoped(t *testing.T) {
	const mine, theirs = "sub-owner", "sub-someone-else"

	seed := func(t *testing.T) (*scopedApprovals, uuid.UUID, uuid.UUID) {
		t.Helper()
		fa := newFakeApprovals()
		ownedRun, foreignRun := uuid.New(), uuid.New()
		for _, runID := range []uuid.UUID{ownedRun, foreignRun} {
			if _, err := fa.Request(context.Background(), types.ApprovalRequest{
				ID: uuid.New(), RunID: runID, Kind: types.ApprovalEgressDomain,
				RequestedScope: json.RawMessage(`{"host":"example.com"}`),
			}); err != nil {
				t.Fatal(err)
			}
		}
		return &scopedApprovals{
			fakeApprovals: fa,
			runCreator:    map[uuid.UUID]string{ownedRun: mine, foreignRun: theirs},
		}, ownedRun, foreignRun
	}

	t.Run("a member sees only approvals on runs they created", func(t *testing.T) {
		ap, ownedRun, foreignRun := seed(t)
		h := newHarness(t)
		cfg := baseTestConfig(h, r3PlainStore{})
		cfg.OIDC = &oidc.Authenticator{}
		cfg.Approvals = ap
		srv := New(cfg)

		w := doSSO(t, srv, http.MethodGet, "/api/v1/approvals",
			ssoSession(t, mine, "owner@corp.example", oidc.RoleMember), "")
		if w.Code != http.StatusOK {
			t.Fatalf("member GET /approvals = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var got []types.ApprovalRequest
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, w.Body.String())
		}
		if len(got) != 1 || got[0].RunID != ownedRun {
			t.Fatalf("member saw %d approvals %v, want exactly the one on their own run %s "+
				"(the foreign run %s must not appear — an unscoped list serves every member the whole fleet's queue)",
				len(got), got, ownedRun, foreignRun)
		}
	})

	// The fail-CLOSED arm: a backend that cannot scope must refuse, never fall
	// back to the unscoped list. fakeApprovals alone does NOT implement
	// ApprovalsByRunCreatorPager, which is exactly the shape being pinned.
	t.Run("a backend that cannot scope refuses rather than widening", func(t *testing.T) {
		h := newHarness(t)
		cfg := baseTestConfig(h, r3PlainStore{})
		cfg.OIDC = &oidc.Authenticator{}
		cfg.Approvals = newFakeApprovals()
		srv := New(cfg)

		w := doSSO(t, srv, http.MethodGet, "/api/v1/approvals",
			ssoSession(t, mine, "owner@corp.example", oidc.RoleMember), "")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("member GET /approvals on an unscopable backend = %d, want 500; body=%s", w.Code, w.Body.String())
		}
	})

	// The control: the security tier DOES see the org-wide queue, so a test that
	// merely made every list empty would not pass this arm.
	t.Run("the security tier still sees the org-wide queue", func(t *testing.T) {
		ap, _, _ := seed(t)
		h := newHarness(t)
		cfg := baseTestConfig(h, r3PlainStore{})
		cfg.OIDC = &oidc.Authenticator{}
		cfg.Approvals = ap
		srv := New(cfg)

		w := doSSO(t, srv, http.MethodGet, "/api/v1/approvals",
			ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin), "")
		if w.Code != http.StatusOK {
			t.Fatalf("security_admin GET /approvals = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		var got []types.ApprovalRequest
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("security_admin saw %d approvals, want both", len(got))
		}
	})
}
