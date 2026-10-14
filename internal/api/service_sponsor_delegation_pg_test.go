// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestServiceIdentity_PortalTokenGetsNoSponsorReach: a portal's delegated token acting for a service
// identity's sponsor reads, lists, kills and mints an entry ticket for nothing of the service's, even
// when the same person's own session does all of that (a service_attach grant held). Portals are refused
// at the service token mint too (design §6).
func TestServiceIdentity_PortalTokenGetsNoSponsorReach(t *testing.T) {
	e := newDelegationPG(t)
	ctx := context.Background()
	pg := store.NewPG(e.pool)
	e.h.srv.cfg.Approvals = attentionApprovals{ApprovalService: e.h.srv.cfg.Approvals, st: pg}
	if _, err := pg.UpsertLoginIdentity(ctx, store.LoginIdentity{
		Principal: svcSponsor, Issuer: "https://idp.example", Email: "sponsor@corp.example",
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/service-identities", e.secAdmin,
		`{"name":"`+svcName+`","sponsor":"`+svcSponsor+`","description":"ci runner"}`); w.Code != http.StatusCreated {
		t.Fatalf("create service identity: %d %s", w.Code, w.Body.String())
	}
	ageSponsorships(t, e.pool)
	body := fmt.Sprintf(`{"subject_type":"user","subject":"sponsor@corp.example","capability":"feature","value":%q,"effect":"allow"}`, featureServiceAttach)
	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/permissions/grants", e.admin, body); w.Code/100 != 2 {
		t.Fatalf("grant: %d %s", w.Code, w.Body.String())
	}
	id := uuid.New()
	run, err := pg.CreateRun(ctx, types.AgentRun{
		ID: id, CreatedBy: svcPrin, Agent: "claude-code", ConfinementClass: types.CC2, State: types.RunRunning,
		RunnerTarget: "docker", Task: "t", SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(),
		SandboxRef: "wardyn-agent-" + id.String(), Interactive: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	own := accessSession(t, svcSponsor, "sponsor@corp.example", oidc.RoleUser, []string{delegGroup})
	portal, cred := e.registerPortal(t, delegGroup)
	viaPortal := e.delegate(t, portal, cred, svcSponsor, "sponsor@corp.example", delegGroup)
	path := "/api/v1/runs/" + run.ID.String()
	ticket := path + "/attach-ticket"

	// Control: the person's own session has the reach.
	if w := doSSO(t, e.h.srv, http.MethodGet, path, own, ""); w.Code != http.StatusOK {
		t.Fatalf("control: the sponsor's session reading the run: %d %s, want 200", w.Code, w.Body.String())
	}
	if w := doSSO(t, e.h.srv, http.MethodPost, ticket, own, ""); w.Code != http.StatusOK {
		t.Fatalf("control: the sponsor's session minting a ticket: %d %s, want 200", w.Code, w.Body.String())
	}

	// The portal's token: the foreign-run 404 on every one, and no list entry.
	for _, c := range []struct{ method, p string }{
		{http.MethodGet, path}, {http.MethodGet, path + "/output"}, {http.MethodGet, path + "/events"},
		{http.MethodPost, path + "/kill"}, {http.MethodPost, ticket},
	} {
		if w := do(t, e.h.srv, c.method, c.p, viaPortal, ""); !refusedAsForeign(w) {
			t.Errorf("portal token %s %s: %d %s, want the foreign-run 404", c.method, c.p, w.Code, w.Body.String())
		}
	}
	for _, q := range []string{"", "?view=user", "?owner=me&include_killed=1"} {
		w := do(t, e.h.srv, http.MethodGet, "/api/v1/runs"+q, viaPortal, "")
		if w.Code != http.StatusOK || containsRunID(w.Body.String(), run.ID) {
			t.Errorf("portal token GET /runs%s: %d, lists the service's run = %v, want 200 without it", q, w.Code, containsRunID(w.Body.String(), run.ID))
		}
	}
	if got, err := pg.GetRun(ctx, run.ID); err != nil || got.State != types.RunRunning {
		t.Errorf("run after the portal's kill = %v (%v), want still RUNNING", got.State, err)
	}
}

func containsRunID(body string, id uuid.UUID) bool { return strings.Contains(body, id.String()) }
