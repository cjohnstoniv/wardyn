// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The request list against Postgres when an applied elevation's membership cannot be read. Guarded by
// WARDYN_TEST_PG; it runs on the elevation tree of governance_elevation_pg_test.go.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// membershipReadFails is the store with every rolled-back read transaction failing to start: the one read an
// applied elevation's view makes for its end.
type membershipReadFails struct{ store.PG }

func (membershipReadFails) DryRunGovernance(context.Context, func(store.Querier) error) error {
	return errors.New("begin: connection reset")
}

// TestRequestListUnreadableMembershipIsAnError: an applied elevation whose membership cannot be read answers
// 500, never the applied request with no end, which the console shows as ended at its approval. A membership
// held with no end is listed as such, and one that is gone is still listed, with neither.
func TestRequestListUnreadableMembershipIsAnError(t *testing.T) {
	e := newElevEnv(t)
	got := e.ask(e.req, e.a1, "2h", "")
	if w := e.call(e.adminA1, http.MethodPost, requestPath(got.Pending.ID, "approve"), ""); w.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", w.Code, w.Body)
	}
	if l := e.list(e.req); len(l.Requests) != 1 || l.Requests[0].EndsAt == nil {
		t.Fatalf("list = %+v, want the applied request with its end", l.Requests)
	}
	srv := e.otherServer(func(c *Config) { c.Store = membershipReadFails{e.pg} })
	if w := doSSO(t, srv, http.MethodGet, requestsPath, e.req, ""); w.Code != http.StatusInternalServerError {
		t.Fatalf("GET %s with the membership unreadable = %d %s, want 500", requestsPath, w.Code, w.Body)
	}
	// A unit admin's membership write makes an elevation permanent: its end is cleared.
	if _, err := e.pool.Exec(context.Background(), `UPDATE governance_memberships SET expires_at = NULL WHERE unit_id = $1 AND expires_at IS NOT NULL`, e.a1.ID); err != nil {
		t.Fatal(err)
	}
	if l := e.list(e.req); len(l.Requests) != 1 || !l.Requests[0].NoEnd || l.Requests[0].EndsAt != nil {
		t.Errorf("list with the membership held with no end = %+v, want the applied request marked no_end", l.Requests)
	}
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM governance_memberships WHERE unit_id = $1`, e.a1.ID); err != nil {
		t.Fatal(err)
	}
	if l := e.list(e.req); len(l.Requests) != 1 || l.Requests[0].State != types.GovernanceChangeApplied || l.Requests[0].EndsAt != nil || l.Requests[0].NoEnd {
		t.Errorf("list after the membership was deleted = %+v, want the applied request with no end and no no_end", l.Requests)
	}
}
