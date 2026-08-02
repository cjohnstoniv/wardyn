// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// nonPagerAuditStore serves the global feed WITHOUT implementing store.Pager, so
// the handler takes servePage's fetch-all fallback. That branch is the trap this
// test exists for: a filter wired only into the paged query answers a filtered
// request with UNFILTERED events here — silently, which in a governance product
// is a wrong answer, not a slow one.
type nonPagerAuditStore struct {
	store.Store
	events []types.AuditEvent
}

func (s *nonPagerAuditStore) QueryRecentAuditEvents(context.Context, int) ([]types.AuditEvent, error) {
	return s.events, nil
}

func auditFilterFixture() []types.AuditEvent {
	return []types.AuditEvent{
		{ID: uuid.New(), ActorType: types.ActorHuman, Actor: "alice", Action: "secret.write", Outcome: "success"},
		{ID: uuid.New(), ActorType: types.ActorSystem, Actor: "wardynd", Action: "run.kill", Outcome: "failure"},
		{ID: uuid.New(), ActorType: types.ActorSystem, Actor: "wardynd", Action: "run.create", Outcome: "success"},
	}
}

func getAuditEvents(t *testing.T, srv *Server, query string) []types.AuditEvent {
	t.Helper()
	w := do(t, srv, http.MethodGet, "/api/v1/audit"+query, adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /audit%s: code = %d, want 200; body=%s", query, w.Code, w.Body.String())
	}
	var out []types.AuditEvent
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// TestAuditFilter_AppliesOnBothReadPaths pins the predicates on the pager path
// AND on the fetch-all fallback, which must agree — one declaration, two
// renderings (store.AuditFilter.where / .Matches).
func TestAuditFilter_AppliesOnBothReadPaths(t *testing.T) {
	h := newHarness(t)
	events := auditFilterFixture()
	for _, tc := range []struct {
		name  string
		store store.Store
	}{
		{"pager", &pagerFake{recentAudit: events}},
		{"fetch-all fallback", &nonPagerAuditStore{events: events}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(baseTestConfig(h, tc.store))

			if got := getAuditEvents(t, srv, ""); len(got) != 3 {
				t.Errorf("unfiltered: got %d events, want all 3", len(got))
			}
			if got := getAuditEvents(t, srv, "?action=run.kill"); len(got) != 1 || got[0].Action != "run.kill" {
				t.Errorf("?action=: got %+v, want only run.kill", got)
			}
			if got := getAuditEvents(t, srv, "?action_prefix=run."); len(got) != 2 {
				t.Errorf("?action_prefix=: got %d events, want the 2 run.* ones", len(got))
			}
			if got := getAuditEvents(t, srv, "?outcome=failure"); len(got) != 1 || got[0].Outcome != "failure" {
				t.Errorf("?outcome=: got %+v, want only the failure", got)
			}
			if got := getAuditEvents(t, srv, "?actor_type=human"); len(got) != 1 || got[0].Actor != "alice" {
				t.Errorf("?actor_type=: got %+v, want only the human event", got)
			}
		})
	}

	srv := New(baseTestConfig(h, &pagerFake{recentAudit: events}))
	for _, q := range []string{"?since=yesterday", "?until=2026", "?actor_type=robot"} {
		if w := do(t, srv, http.MethodGet, "/api/v1/audit"+q, adminToken, ""); w.Code != http.StatusBadRequest {
			t.Errorf("GET /audit%s: code = %d, want 400", q, w.Code)
		}
	}
}
