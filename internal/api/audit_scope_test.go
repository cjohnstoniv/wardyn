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

// auditScopeStore is a pager that can also answer GetRun, which the member gate
// needs to decide ownership. pagerFake alone cannot: it embeds a nil
// store.Store, so the ownership read would panic rather than answer.
type auditScopeStore struct {
	pagerFake
	runs map[uuid.UUID]types.AgentRun
}

func (s *auditScopeStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	r, ok := s.runs[id]
	if !ok {
		return types.AgentRun{}, store.ErrNotFound
	}
	return r, nil
}

const auditMemberSub = "sub-audit-member"

// TestAuditMemberScope_QueryAndExportAgree pins the member scoping that
// handleQueryAudit and handleExportAudit now share through auditScope, and
// pins it as ONE property: for every request shape, the capped JSON read and
// the uncapped NDJSON export must return the SAME events. Two hand-copied
// gates could drift into a member seeing through the export what the query
// withholds; one gate cannot, and this test is what says so out loud.
//
// The collapse is deliberately an empty 200 and never a 404: /audit is a
// collection endpoint, so "a run you do not own", "a run that does not exist"
// and "no run_id at all" must be indistinguishable — a 404 on one of them
// would be an existence oracle over other people's runs. A MALFORMED run_id is
// the one thing that still 400s for everybody: it is an input-shape error, not
// an authz answer, which is why the parse happens before the role is read.
func TestAuditMemberScope_QueryAndExportAgree(t *testing.T) {
	h := newHarness(t)
	mine, theirs := uuid.New(), uuid.New()
	st := &auditScopeStore{
		runs: map[uuid.UUID]types.AgentRun{
			mine:   {ID: mine, CreatedBy: auditMemberSub},
			theirs: {ID: theirs, CreatedBy: "sub-somebody-else"},
		},
	}
	st.auditByRun = map[uuid.UUID][]types.AuditEvent{
		mine:   makeEvents(3, &mine),
		theirs: makeEvents(4, &theirs),
	}
	st.recentAudit = makeEvents(9, nil)

	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	member := ssoSession(t, auditMemberSub, "member@corp.example", oidc.RoleMember)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	cases := []struct {
		name       string
		query      string
		cookie     *http.Cookie
		wantCode   int
		wantEvents int
	}{
		{"member, no run_id, collapses to empty", "", member, http.StatusOK, 0},
		{"member, own run, sees that trail", "?run_id=" + mine.String(), member, http.StatusOK, 3},
		{"member, another's run, empty not 404", "?run_id=" + theirs.String(), member, http.StatusOK, 0},
		{"member, unknown run, empty not 404", "?run_id=" + uuid.New().String(), member, http.StatusOK, 0},
		{"member, own run, filter still applies", "?run_id=" + mine.String() + "&action=test.event", member, http.StatusOK, 3},
		{"member, own run, non-matching filter", "?run_id=" + mine.String() + "&action=nothing.matches", member, http.StatusOK, 0},
		{"member, malformed run_id, 400 not empty", "?run_id=not-a-uuid", member, http.StatusBadRequest, 0},
		{"security operator, malformed run_id, 400 too", "?run_id=not-a-uuid", admin, http.StatusBadRequest, 0},
		{"security operator, no run_id, global feed", "", admin, http.StatusOK, 9},
		{"security operator, another's run, sees it", "?run_id=" + theirs.String(), admin, http.StatusOK, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, "/api/v1/audit"+c.query, c.cookie, "")
			if w.Code != c.wantCode {
				t.Fatalf("GET /audit%s: code = %d, want %d (body=%s)", c.query, w.Code, c.wantCode, w.Body.String())
			}
			x := doSSO(t, srv, http.MethodGet, "/api/v1/audit/export"+c.query, c.cookie, "")
			if x.Code != c.wantCode {
				t.Fatalf("GET /audit/export%s: code = %d, want %d (body=%s)", c.query, x.Code, c.wantCode, x.Body.String())
			}
			if c.wantCode != http.StatusOK {
				return
			}
			var got []types.AuditEvent
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode /audit body: %v (body=%s)", err, w.Body.String())
			}
			if len(got) != c.wantEvents {
				t.Errorf("/audit returned %d events, want %d", len(got), c.wantEvents)
			}
			// The export answers in NDJSON even when it answers with nothing —
			// an empty export is a typed 200, not a bare one.
			if ct := x.Header().Get("Content-Type"); ct != "application/x-ndjson" {
				t.Errorf("/audit/export Content-Type = %q, want application/x-ndjson", ct)
			}
			lines := ndjsonLines(t, x.Body.Bytes())
			if len(lines) != c.wantEvents {
				t.Errorf("/audit/export returned %d events, want %d", len(lines), c.wantEvents)
			}
			if len(lines) != len(got) {
				t.Errorf("query and export disagree: /audit %d events, /audit/export %d", len(got), len(lines))
			}
		})
	}
}
