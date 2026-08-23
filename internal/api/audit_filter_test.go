// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
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
			// D6: ?actor= — the per-principal "everything developer X did" filter,
			// on BOTH read paths.
			if got := getAuditEvents(t, srv, "?actor=alice"); len(got) != 1 || got[0].Actor != "alice" {
				t.Errorf("?actor=: got %+v, want only alice's event", got)
			}
			if got := getAuditEvents(t, srv, "?actor=nobody"); len(got) != 0 {
				t.Errorf("?actor= unknown principal: got %d events, want 0", len(got))
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

// TestAuditExport_NDJSON covers the D6 streaming export: /audit/export streams
// every matching event as newline-delimited JSON, applies ?actor= like the
// paginated read, and (on a non-Pager backend) fails with 501 rather than a
// silently-capped read.
func TestAuditExport_NDJSON(t *testing.T) {
	h := newHarness(t)
	events := auditFilterFixture()
	srv := New(baseTestConfig(h, &pagerFake{recentAudit: events}))

	// Full export: one JSON object per line, all three events.
	w := do(t, srv, http.MethodGet, "/api/v1/audit/export", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("export: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
	lines := ndjsonLines(t, w.Body.Bytes())
	if len(lines) != 3 {
		t.Fatalf("export lines = %d, want 3\n%s", len(lines), w.Body.String())
	}

	// Filtered export: ?actor=alice yields only alice's event.
	w = do(t, srv, http.MethodGet, "/api/v1/audit/export?actor=alice", adminToken, "")
	lines = ndjsonLines(t, w.Body.Bytes())
	if len(lines) != 1 || lines[0].Actor != "alice" {
		t.Fatalf("actor-filtered export = %+v, want only alice", lines)
	}

	// A non-Pager backend refuses rather than silently returning a capped read.
	srv = New(baseTestConfig(h, &nonPagerAuditStore{events: events}))
	if w := do(t, srv, http.MethodGet, "/api/v1/audit/export", adminToken, ""); w.Code != http.StatusNotImplemented {
		t.Fatalf("export on non-pager backend: code = %d, want 501", w.Code)
	}
}

func ndjsonLines(t *testing.T, body []byte) []types.AuditEvent {
	t.Helper()
	var out []types.AuditEvent
	dec := json.NewDecoder(bytes.NewReader(body))
	for dec.More() {
		var ev types.AuditEvent
		if err := dec.Decode(&ev); err != nil {
			t.Fatalf("decode ndjson: %v", err)
		}
		out = append(out, ev)
	}
	return out
}
