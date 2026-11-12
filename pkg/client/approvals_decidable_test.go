// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestListApprovalsDecidable(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/api/v1/approvals" || r.URL.Query().Get("decidable_by") != "me" {
				t.Errorf("request = %s %s, want GET /api/v1/approvals?decidable_by=me", r.Method, r.URL)
			}
			if truncated {
				w.Header().Set("X-Wardyn-Truncated", "true")
			}
			writeJSON(w, http.StatusOK, []types.ApprovalRequest{{ID: uuid.New(), State: types.ApprovalPending, RequestedBy: "sub-owner"}})
		}))
		got, gotTruncated, err := newTestClient(srv).ListApprovalsDecidable(context.Background())
		srv.Close()
		if err != nil || len(got) != 1 || got[0].RequestedBy != "sub-owner" || gotTruncated != truncated {
			t.Errorf("truncated=%v: got %+v, truncated=%v, err=%v", truncated, got, gotTruncated, err)
		}
	}
}
