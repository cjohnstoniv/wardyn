// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The person's walls over Postgres: written through PUT /me/walls, met into a create, re-asserted
// by the seal a dispatch and a revive run, withheld at the mint door, refused at an approval, and
// erased with the person.

const wallsPGBody = `{"spec":{"allowed_domains":["api.anthropic.com","github.com"],"denied_domains":["evil.example"],` +
	`"withheld_grants":[{"kind":"ssh_key"}]}}`

// putWallsPG writes body as e.user's walls through the route.
func putWallsPG(t *testing.T, e *govEnv, body string) {
	t.Helper()
	if w := e.call(e.user, http.MethodPut, "/api/v1/me/walls", body); w.Code != http.StatusOK {
		t.Fatalf("PUT /me/walls = %d %s", w.Code, w.Body)
	}
}

// TestPersonalWallsPGCreate: a member's create resolves inside their walls and records what they
// narrowed on run.create.
func TestPersonalWallsPGCreate(t *testing.T) {
	pool := throwawayPGPool(t)
	testutil.SetBoundaryNoLimits(t, pool)
	e := newGovEnvOn(t, pool)
	putWallsPG(t, e, wallsPGBody)
	w := e.call(e.user, http.MethodPost, "/api/v1/runs", `{"agent":"claude-code","task":"t"}`)
	var created createRunResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	ev := waitForRecAudit(t, e.h.audit, created.ID, "run.create", "success")
	var data struct {
		PolicySource struct {
			PersonalWalls *wallsSourceRecord `json:"personal_walls"`
		} `json:"policy_source"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	p := data.PolicySource.PersonalWalls
	if p == nil || !slices.Contains(p.Removed["allowed_domains"], "pypi.org") || !slices.Equal(p.Added["denied_domains"], []string{"evil.example"}) {
		t.Errorf("run.create policy_source.personal_walls = %+v, want pypi.org removed and evil.example added", p)
	}
}

// TestPersonalWallsPGSealAndMint: the seal a dispatch runs adds the walls' denies and withholds the
// credential kinds they keep out; walls tightened later bind the next seal (a revive) and the mint
// door; walls that cannot be read are answered unread at the mint door, never as no walls.
func TestPersonalWallsPGSealAndMint(t *testing.T) {
	pool := throwawayPGPool(t)
	testutil.SetBoundaryNoLimits(t, pool)
	e := newGovEnvOn(t, pool)
	ctx := context.Background()
	putWallsPG(t, e, wallsPGBody)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-user"}
	ssh := types.GrantSpec{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"host":"git.corp.example","key_secret_ref":"deploy-key"}`)}

	policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "pypi.org"}}
	prov := knobProvenance{}
	layers := e.srv.sealRunPolicy(ctx, run, &policy, prov)
	if !slices.Contains(policy.DeniedDomains, "evil.example") || !slices.Contains(policy.AllowedDomains, "pypi.org") {
		t.Errorf("sealed allowed %v denied %v, want the setup host kept and evil.example denied", policy.AllowedDomains, policy.DeniedDomains)
	}
	if prov["denied_domains"] != personalProvenance {
		t.Errorf("the seal recorded %+v for denied_domains, want the person's walls", prov["denied_domains"])
	}
	if w := (withholding{layers: layers}); !w.credentialWithheld(ssh) {
		t.Error("dispatch: the ssh_key grant was not withheld")
	}
	live, err := e.srv.liveWithholding(ctx, run)
	if err != nil || !live.credentialWithheld(ssh) || !live.withheldAny([]string{"evil.example"}) {
		t.Errorf("mint: %v, ssh_key withheld %v, evil.example withheld %v", err, live.credentialWithheld(ssh), live.withheldAny([]string{"evil.example"}))
	}

	// Tightened after the run started: the next seal (a revive) and the mint door hold it.
	putWallsPG(t, e, `{"spec":{"denied_domains":["evil.example","pypi.org"]}}`)
	revived := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "pypi.org"}}
	e.srv.sealRunPolicy(ctx, run, &revived, nil)
	if !slices.Contains(revived.DeniedDomains, "pypi.org") {
		t.Errorf("revive seal denies %v, want pypi.org", revived.DeniedDomains)
	}
	if live, err := e.srv.liveWithholding(ctx, run); err != nil || !live.withheldAny([]string{"pypi.org"}) || live.credentialWithheld(ssh) {
		t.Errorf("mint after the change: %v, pypi.org withheld %v, ssh_key withheld %v; want true false", err, live.withheldAny([]string{"pypi.org"}), live.credentialWithheld(ssh))
	}

	if err := e.pg.PutPrincipalPref(ctx, "sub-user", types.PersonalWallsPrefKey, json.RawMessage(`{"spec":{"bogus":true}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.liveWithholding(ctx, run); err == nil {
		t.Error("unreadable walls answered the mint door as no walls")
	}
}

// TestPersonalWallsPGApprovalRefused: no decider admits a host the run owner's walls deny.
func TestPersonalWallsPGApprovalRefused(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	putWallsPG(t, e, `{"spec":{"denied_domains":["pypi.org"]}}`)
	run, err := e.pg.CreateRun(context.Background(), types.AgentRun{ID: uuid.New(), CreatedBy: "sub-user", Agent: "claude-code",
		ConfinementClass: types.CC3, State: types.RunRunning, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ask := func(host string) uuid.UUID {
		ap, err := e.h.approvals.Request(context.Background(), types.ApprovalRequest{RunID: run.ID, Kind: types.ApprovalEgressDomain,
			RequestedScope: json.RawMessage(`{"host":"` + host + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		return ap.ID
	}
	decider := e.otherServer(func(c *Config) { c.Approvals = e.h.approvals })
	approve := func(id uuid.UUID) *httptest.ResponseRecorder {
		return doSSO(t, decider, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", e.alice, `{}`)
	}
	if w := approve(ask("PyPI.org")); w.Code != http.StatusForbidden || wireReason(t, w) != reasonPersonalWallsHost {
		t.Errorf("approving a host the walls deny: %d %s, want 403 %s", w.Code, w.Body, reasonPersonalWallsHost)
	}
	if w := approve(ask("github.com")); w.Code != http.StatusOK {
		t.Errorf("approving a host the walls do not deny: %d %s, want 200", w.Code, w.Body)
	}
}

// TestPersonalWallsPGErasure: the person's walls go with their stored preferences when they are erased.
func TestPersonalWallsPGErasure(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	putWallsPG(t, e, wallsPGBody)
	if _, err := e.pg.DeleteRunnerPoolsByOwner(ctx, "sub-user"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.GetPrincipalPref(ctx, "sub-user", types.PersonalWallsPrefKey); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("walls after erasure: %v, want none", err)
	}
}
