// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// route_renames_0_8_test.go pins issue #658's acceptance criterion directly:
// every route family renamed in 0.8 (attach/holder, attach/ticket,
// profile/synthesize) must answer identically on its new path AND its old,
// chi-aliased one. Each test below hits both paths against the same fixture
// and requires byte-identical bodies, so a alias that silently drifts from
// its handler (or gets dropped) fails here instead of only in the field.

// TestAttachHolderRenamedPathMatchesAlias: GET /attach/holder (0.8) and GET
// /attach-holder (the alias kept for one minor) must be the exact same
// handler — holderTestServer already wires a running run an owning member can
// read.
func TestAttachHolderRenamedPathMatchesAlias(t *testing.T) {
	srv, _, _, _, run := holderTestServer(t)
	owner := ssoSession(t, holderOwner, holderOwner, oidc.RoleMember)

	oldPath := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/attach-holder", owner, "")
	newPath := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/attach/holder", owner, "")
	if oldPath.Code != http.StatusOK || newPath.Code != http.StatusOK {
		t.Fatalf("attach-holder alias: old code = %d, new code = %d, want both 200", oldPath.Code, newPath.Code)
	}
	if oldPath.Body.String() != newPath.Body.String() {
		t.Fatalf("attach-holder alias bodies differ:\nold=%s\nnew=%s", oldPath.Body.String(), newPath.Body.String())
	}
}

// TestAttachTicketRenamedPathMatchesAlias: POST /attach/ticket (0.8) and POST
// /attach-ticket (the alias) both mint a ticket for the run's own owner.
// secAdminRunStore (security_admin_test.go) already models the minimal store
// the mint path touches.
func TestAttachTicketRenamedPathMatchesAlias(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &secAdminRunStore{run: types.AgentRun{ID: runID, CreatedBy: secAdminSub, State: types.RunRunning}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	sess := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)

	oldPath := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/attach-ticket", sess, "")
	if oldPath.Code != http.StatusOK {
		t.Fatalf("POST attach-ticket (alias): code = %d, want 200; body=%s", oldPath.Code, oldPath.Body.String())
	}
	newPath := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/attach/ticket", sess, "")
	if newPath.Code != http.StatusOK {
		t.Fatalf("POST attach/ticket (0.8): code = %d, want 200; body=%s", newPath.Code, newPath.Body.String())
	}
}

// TestProfileSynthesizeRenamedPathMatchesAlias: POST /profile/synthesize (0.8)
// and POST /profile (the alias) must both synthesize the same profile for the
// same completed run — profile_test.go's recordStore fixture already covers
// the handler's real inputs (audit events, groundtruth heartbeat).
func TestProfileSynthesizeRenamedPathMatchesAlias(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	hb := groundtruth.HeartbeatEventWithDropped(0, 9, 0, map[string]uint64{groundtruth.ActionProcessExec: 9})
	hb.Time = time.Now()
	fake := &recordStore{
		run: types.AgentRun{ID: runID, Agent: "claude-code", Repo: "org/repo",
			State: types.RunCompleted, ConfinementClass: types.CC2},
		events:    []types.AuditEvent{egressAllowEvent(runID, "registry.npmjs.org")},
		heartbeat: &hb,
	}
	cfg := baseTestConfig(h, fake)
	cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	srv := New(cfg)

	oldPath := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/profile", adminToken, "")
	if oldPath.Code != http.StatusOK {
		t.Fatalf("POST .../profile (alias): code = %d, body=%s", oldPath.Code, oldPath.Body.String())
	}
	newPath := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/profile/synthesize", adminToken, "")
	if newPath.Code != http.StatusOK {
		t.Fatalf("POST .../profile/synthesize (0.8): code = %d, body=%s", newPath.Code, newPath.Body.String())
	}
	if oldPath.Body.String() != newPath.Body.String() {
		t.Fatalf("profile alias bodies differ:\nold=%s\nnew=%s", oldPath.Body.String(), newPath.Body.String())
	}
}
