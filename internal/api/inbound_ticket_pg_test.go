// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const appTicketOwner = "ticket-owner"

// inboundTicketRun stores a RUNNING run of appTicketOwner whose effective policy declares "api" inbound with
// callers and "web" not inbound.
func (e svcEnv) inboundTicketRun(t *testing.T, callers ...string) types.AgentRun {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	run, err := e.st.CreateRun(ctx, types.AgentRun{
		ID: id, CreatedBy: appTicketOwner, Agent: "claude-code", ConfinementClass: types.CC2, State: types.RunRunning,
		RunnerTarget: "docker", Task: "t", SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(),
		SandboxRef: "wardyn-agent-" + id.String(), Interactive: true,
	})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	data, err := json.Marshal(types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: []types.UIApp{
		{Name: "api", Port: 8080, Inbound: true, Callers: callers}, {Name: "web", Port: 3000}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (store.Recorder{Pool: e.pool}).Record(ctx, types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &run.ID,
		ActorType: types.ActorSystem, Actor: "wardynd", Action: "run.policy.resolve", Target: run.ID.String(), Outcome: "success", Data: data}); err != nil {
		t.Fatalf("seed the run's effective policy: %v", err)
	}
	return run
}

func (e svcEnv) appTicket(t *testing.T, as *http.Cookie, run uuid.UUID, app string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, e.h.srv, http.MethodPost, "/api/v1/runs/"+run.String()+"/apps/"+app+"/ticket", as, "")
}

// TestAppTicket_CallerMintAndRedeem: in host mode the owner and each caller the app names (by subject, by a
// group of their sign-in) get a ticket; the stored ticket carries the matched entry and the app, and the
// terminal WebSocket refuses a caller's ticket even for its run.
func TestAppTicket_CallerMintAndRedeem(t *testing.T) {
	e := newSvcEnv(t)
	e.h.srv.cfg.UIOriginTemplate = "https://{run}.ui.example.com"
	e.h.srv.cfg.Runner = &holderTestRunner{}
	run := e.inboundTicketRun(t, "user:ana", "group:payments")
	ctx := context.Background()

	for _, tc := range []struct {
		name, caller string
		as           *http.Cookie
	}{
		{"the owner", "", accessSession(t, appTicketOwner, "owner@corp.example", oidc.RoleUser, []string{})},
		{"a caller by subject", "user:ana", accessSession(t, "ana", "ana@corp.example", oidc.RoleUser, []string{})},
		{"a caller by group", "group:payments", accessSession(t, "gus", "gus@corp.example", oidc.RoleUser, []string{"Payments"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := ticketOf(t, e.appTicket(t, tc.as, run.ID, "api"))
			ta, v, err := e.h.srv.redeemAttachTicket(ctx, tok, run.ID)
			if err != nil || v != verdictOK {
				t.Fatalf("redeem = %v %q, want ok", err, v)
			}
			if ta.caller != tc.caller || ta.app != "api" || ta.role != oidc.RoleUser {
				t.Errorf("redeemed ticket caller %q app %q role %q, want %q, api, member", ta.caller, ta.app, ta.role, tc.caller)
			}
		})
	}

	ana := accessSession(t, "ana", "ana@corp.example", oidc.RoleUser, []string{})
	tok := ticketOf(t, e.appTicket(t, ana, run.ID, "api"))
	w := do(t, e.h.srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, "", "")
	if w.Code != http.StatusForbidden || errorReason(w) != reasonAttachTicketNotYourRun {
		t.Errorf("a caller's ticket at the terminal: %d %s, want 403 %s", w.Code, w.Body.String(), reasonAttachTicketNotYourRun)
	}
}

// TestAppTicket_RefusedAsAForeignRun: a person the app does not name, a super admin, an app that is not
// inbound and an app the run does not declare all get the foreign-run 404, with an authz.denied row.
func TestAppTicket_RefusedAsAForeignRun(t *testing.T) {
	e := newSvcEnv(t)
	e.h.srv.cfg.UIOriginTemplate = "https://{run}.ui.example.com"
	run := e.inboundTicketRun(t, "user:ana")
	owner := accessSession(t, appTicketOwner, "owner@corp.example", oidc.RoleUser, []string{})
	for _, tc := range []struct {
		name, app string
		as        *http.Cookie
	}{
		{"a person who is not a caller", "api", e.other},
		{"a super admin", "api", e.super},
		{"the owner, on an app that is not inbound", "web", owner},
		{"the owner, on an app the run does not declare", "nope", owner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(e.h.audit.snapshot())
			if w := e.appTicket(t, tc.as, run.ID, tc.app); !refusedAsForeign(w) {
				t.Fatalf("mint: %d %s, want 404 %s", w.Code, w.Body.String(), reasonRunNotFound)
			}
			denied := false
			for _, ev := range e.h.audit.snapshot()[before:] {
				denied = denied || (ev.Action == authz.AuditAction && strings.Contains(string(ev.Data), string(authz.ReasonAttachTicketForeignRun)))
			}
			if !denied {
				t.Errorf("no authz.denied row of reason %s", authz.ReasonAttachTicketForeignRun)
			}
		})
	}
	if w := e.appTicket(t, owner, uuid.New(), "api"); !refusedAsForeign(w) {
		t.Errorf("a run that does not exist: %d %s, want 404", w.Code, w.Body.String())
	}
}

// TestAppTicket_PathModeNeedsHostMode: with apps served from one shared host a named caller is refused 403
// inbound_needs_host_mode, audited as inbound.deny; the owner still gets a ticket.
func TestAppTicket_PathModeNeedsHostMode(t *testing.T) {
	e := newSvcEnv(t)
	run := e.inboundTicketRun(t, "user:ana")
	ana := accessSession(t, "ana", "ana@corp.example", oidc.RoleUser, []string{})
	w := e.appTicket(t, ana, run.ID, "api")
	if w.Code != http.StatusForbidden || errorReason(w) != reasonInboundNeedsHostMode || !strings.Contains(w.Body.String(), "per-run hosts") {
		t.Fatalf("a caller in path mode: %d %s, want 403 %s", w.Code, w.Body.String(), reasonInboundNeedsHostMode)
	}
	audited := false
	for _, ev := range e.h.audit.snapshot() {
		audited = audited || (ev.Action == "inbound.deny" && ev.Actor == "ana" && strings.Contains(string(ev.Data), `"reason":"needs_host_mode"`))
	}
	if !audited {
		t.Errorf("no inbound.deny row of reason needs_host_mode for ana")
	}
	owner := accessSession(t, appTicketOwner, "owner@corp.example", oidc.RoleUser, []string{})
	ticketOf(t, e.appTicket(t, owner, run.ID, "api"))
}
