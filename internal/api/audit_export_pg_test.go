// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_AuditExportPagesPastOnePageAndScopesAMember runs the export's real
// SQL: the fakes are not a store.Pager, so every other export test sees the
// 501. A filtered export larger than one internal page must come back whole,
// in order, with no row twice and nothing the filter excludes; and a member's
// export must be their own run's trail and nothing else.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestPG_AuditExportPagesPastOnePageAndScopesAMember(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	const memberSub = "sub-export-member"
	mine, theirs := uuid.New(), uuid.New()
	for id, owner := range map[uuid.UUID]string{mine: memberSub, theirs: "sub-somebody-else"} {
		now := time.Now().UTC()
		if _, err := pg.CreateRun(ctx, types.AgentRun{
			ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: owner, Agent: "claude-code",
			Task: "audit export", ConfinementClass: types.CC2, State: types.RunRunning,
			SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker",
		}); err != nil {
			t.Fatalf("create run: %v", err)
		}
	}
	seed := func(n int, run *uuid.UUID, action string) {
		t.Helper()
		for i := 0; i < n; i++ {
			ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: run,
				ActorType: types.ActorHuman, Actor: "alice@corp.example", Action: action, Outcome: "success"}
			if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
	const wantBulk = auditExportPageSize + 37
	seed(wantBulk/2, nil, "export.probe")
	seed(25, nil, "export.noise") // interleaved, so the filter has to skip rows mid-page
	seed(wantBulk-wantBulk/2, nil, "export.probe")
	seed(3, &mine, "run.mine")
	seed(4, &theirs, "run.theirs")

	h := newHarness(t)
	cfg := baseTestConfig(h, pg)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	member := ssoSession(t, memberSub, "member@corp.example", oidc.RoleMember)

	export := func(cookie *http.Cookie, query string) []types.AuditEvent {
		t.Helper()
		w := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export"+query, cookie, "")
		if w.Code != http.StatusOK {
			t.Fatalf("export%s: %d %s", query, w.Code, w.Body.String())
		}
		return ndjsonLines(t, w.Body.Bytes())
	}

	bulk := export(admin, "?action=export.probe")
	if len(bulk) != wantBulk {
		t.Fatalf("export returned %d events, want %d: it stopped at a page boundary or leaked the filter", len(bulk), wantBulk)
	}
	seen := map[uuid.UUID]bool{}
	for i, ev := range bulk {
		if ev.Action != "export.probe" {
			t.Fatalf("line %d is %q: the filter was not applied on every page", i, ev.Action)
		}
		if seen[ev.ID] {
			t.Fatalf("line %d repeats %s: pages overlap", i, ev.ID)
		}
		seen[ev.ID] = true
	}
	recent, err := pg.QueryRecentAuditEventsPage(ctx, store.Page{})
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	want := store.AuditFilter{Action: "export.probe"}.Keep(recent)
	for i := range want {
		if bulk[i].ID != want[i].ID {
			t.Fatalf("line %d = %s, want %s: the export is not the newest-first feed", i, bulk[i].ID, want[i].ID)
		}
	}

	for _, c := range []struct {
		name  string
		query string
		want  int
	}{
		{"own run", "?run_id=" + mine.String(), 3},
		{"another's run", "?run_id=" + theirs.String(), 0},
		{"no run_id", "", 0},
	} {
		got := export(member, c.query)
		if len(got) != c.want {
			t.Errorf("member export, %s: %d events, want %d", c.name, len(got), c.want)
		}
		for _, ev := range got {
			if ev.RunID == nil || *ev.RunID != mine {
				t.Errorf("member export, %s: returned %s from run %v, not their own", c.name, ev.Action, ev.RunID)
			}
		}
	}
}
