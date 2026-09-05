// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// r3bTruncatedServer answers every GET with a one-element JSON array and the
// server's X-Wardyn-Truncated marker — the shape servePage (internal/api/
// runs_policy.go) writes when a further page exists.
func r3bTruncatedServer(t *testing.T, truncated bool) *client.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if truncated {
			w.Header().Set("X-Wardyn-Truncated", "true")
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": uuid.New().String()}})
	}))
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "tok")
}

// TestR3BListFamiliesSurfaceTruncation is F265's pin.
//
// The package doc names X-Wardyn-Truncated as THE pagination contract for "the
// list endpoints and the audit trail", but only AuditEventsPage honoured it:
// ListRuns / ListApprovals / ListPolicies / ListWorkspaces threw the header
// away, so a page the server flagged truncated was byte-identical to a complete
// one — the CLI printed it with exit 0, nothing on stderr and no marker in
// --json. Every list family now has a *Page variant returning the bool, and the
// plain forms stay as thin wrappers so existing callers are unchanged.
func TestR3BListFamiliesSurfaceTruncation(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func(c *client.Client) (int, bool, error)
	}{
		{"ListRunsPage", func(c *client.Client) (int, bool, error) {
			out, tr, err := c.ListRunsPage(ctx)
			return len(out), tr, err
		}},
		{"ListApprovalsPage", func(c *client.Client) (int, bool, error) {
			out, tr, err := c.ListApprovalsPage(ctx, "", uuid.Nil)
			return len(out), tr, err
		}},
		{"ListPoliciesPage", func(c *client.Client) (int, bool, error) {
			out, tr, err := c.ListPoliciesPage(ctx)
			return len(out), tr, err
		}},
		{"ListWorkspacesPage", func(c *client.Client) (int, bool, error) {
			out, tr, err := c.ListWorkspacesPage(ctx)
			return len(out), tr, err
		}},
	} {
		t.Run(tc.name+" reports a truncated page", func(t *testing.T) {
			n, truncated, err := tc.call(r3bTruncatedServer(t, true))
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if n != 1 {
				t.Fatalf("%s returned %d rows, want 1 — the fixture served one", tc.name, n)
			}
			if !truncated {
				t.Errorf("%s reported truncated=false for a page the server flagged X-Wardyn-Truncated: true — "+
					"a consumer cannot tell \"this is everything\" from \"this is page 1 of more\"", tc.name)
			}
		})
		// The control: without the header the answer must be false, so the pin
		// cannot pass by hard-coding true.
		t.Run(tc.name+" reports a complete page", func(t *testing.T) {
			_, truncated, err := tc.call(r3bTruncatedServer(t, false))
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if truncated {
				t.Errorf("%s reported truncated=true with no X-Wardyn-Truncated header", tc.name)
			}
		})
	}

	// The plain forms stay: existing callers keep compiling and keep working,
	// which is the whole reason the signal arrives as a sibling method rather
	// than a signature change.
	t.Run("the plain wrappers still return the page", func(t *testing.T) {
		c := r3bTruncatedServer(t, true)
		if runs, err := c.ListRuns(ctx); err != nil || len(runs) != 1 {
			t.Errorf("ListRuns = %d rows, %v; want 1, nil", len(runs), err)
		}
		if aps, err := c.ListApprovals(ctx, "", uuid.Nil); err != nil || len(aps) != 1 {
			t.Errorf("ListApprovals = %d rows, %v; want 1, nil", len(aps), err)
		}
		if pol, err := c.ListPolicies(ctx); err != nil || len(pol) != 1 {
			t.Errorf("ListPolicies = %d rows, %v; want 1, nil", len(pol), err)
		}
		if ws, err := c.ListWorkspaces(ctx); err != nil || len(ws) != 1 {
			t.Errorf("ListWorkspaces = %d rows, %v; want 1, nil", len(ws), err)
		}
	})

	// AuditEventsPage is the family that always honoured the header; asserting
	// it here keeps the four new ones measured against the one that was right.
	t.Run("AuditEventsPage is unchanged", func(t *testing.T) {
		var events []types.AuditEvent
		events, truncated, err := r3bTruncatedServer(t, true).AuditEventsPage(ctx, uuid.New(), client.AuditFilter{})
		if err != nil {
			t.Fatalf("AuditEventsPage: %v", err)
		}
		if !truncated || len(events) != 1 {
			t.Errorf("AuditEventsPage = %d events, truncated=%v; want 1, true", len(events), truncated)
		}
	})
}
