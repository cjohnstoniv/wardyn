// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// the decision's durable write-backs outlive the request

// cancelOnDecide wraps the fixture's approval service so the client
// "disconnects" the instant Decide() commits — the real shape of the defect:
// the browser tab that clicked Always is closed (or the operator navigates)
// between the store transaction and the handler's write-backs, and net/http
// cancels r.Context() the moment the socket goes.
type cancelOnDecide struct {
	ApprovalService
	cancel func()
}

func (c cancelOnDecide) Decide(ctx context.Context, id uuid.UUID, at types.ActorType, d types.ApprovalDecision) (types.ApprovalRequest, error) {
	ap, err := c.ApprovalService.Decide(ctx, id, at, d)
	c.cancel()
	return ap, err
}

// ctxHonouringStore is the fixture store with the ONE method the write-back
// depends on taught to respect ctx — a real pgx pool returns
// context.Canceled here, an in-memory fake would happily write through a dead
// context and hide the whole defect.
type ctxHonouringStore struct {
	*authzStore
}

func (s ctxHonouringStore) AddWorkspaceEgressDecision(ctx context.Context, id uuid.UUID, host string, allow bool, maxApproved int) (types.Workspace, error) {
	if err := ctx.Err(); err != nil {
		return types.Workspace{}, err
	}
	return s.authzStore.AddWorkspaceEgressDecision(ctx, id, host, allow, maxApproved)
}

// ctxHonouringRecorder drops an audit event written on a dead context, the way
// a Postgres INSERT on a cancelled context does. The write-backs' audit rows
// are half of what was lost: a fail-SILENT-BUT-AUDITED contract that fails
// silent AND unaudited is just fail-silent.
type ctxHonouringRecorder struct{ inner *recRecorder }

func (r ctxHonouringRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.inner.Record(ctx, ev)
}

// TestDecideWriteBacksSurviveAClientDisconnect pins `always` is the one
// decision scope that promises something OUTLIVING the run, and the promise was
// kept on r.Context() — so an operator who clicked Always and closed the tab got
// a 200, a green console, and no durable grant. The audit rows that were
// supposed to report the miss rode the same dead context, so nothing anywhere
// said so.
//
// The negative control for this is TestDecideScope_AlwaysPersistsToTheWorkspace
// (the connected client), which must stay green untouched.
func TestDecideWriteBacksSurviveAClientDisconnect(t *testing.T) {
	const host = "registry.npmjs.org"

	f := newScopeFixture(t)
	rec := &recRecorder{}
	f.srv.cfg.Audit = ctxHonouringRecorder{inner: rec}
	f.srv.cfg.Store = ctxHonouringStore{authzStore: f.store}

	wsID := f.seedWorkspace(t, nil, nil)
	id := f.seedEgress(t, host)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.srv.cfg.Approvals = cancelOnDecide{ApprovalService: f.approval, cancel: cancel}

	admin := ssoSession(t, "sub-admin-disconnect", "admin@corp.example", oidc.RoleAdmin)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve",
		strings.NewReader(decideBody(t, types.ScopeAlways, nil))).WithContext(ctx)
	req.AddCookie(admin)
	w := httptest.NewRecorder()
	panicFails(t, f.srv.Handler()).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("approve always: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	approved, _ := f.egressLists(t, wsID)
	if !slices.Contains(approved, host) {
		t.Errorf("approved_egress = %v, want it to contain %q — the permanent grant was dropped "+
			"because the write-back ran on the cancelled request context", approved, host)
	}
	var sawEgressRow bool
	for _, ev := range rec.events {
		if ev.Action == "workspace.egress.approve" {
			sawEgressRow = true
		}
	}
	if !sawEgressRow {
		t.Error("no workspace.egress.approve audit row: the write-back's audit ran on the dead request context too")
	}
}
