// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pgReviveRunner holds one run's rendered proxy config and can replace it.
type pgReviveRunner struct {
	*fakeRunner
	mu       sync.Mutex
	cfg      []byte
	replaced int
}

func (r *pgReviveRunner) ProxyConfig(context.Context, string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg, nil
}

func (r *pgReviveRunner) EnsureProxyImage(context.Context) error { return nil }

func (r *pgReviveRunner) ReplaceProxy(_ context.Context, _ string, cfg []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg, r.replaced = cfg, r.replaced+1
	return nil
}

// TestPG_ReviveAndExtendRecheckOwnerAuthority walks the owner re-check over
// the real capability, grant, run and secret tables: a deny row on the
// owner's email refuses the revive for the owner and for an admin; an erased
// model credential refuses it; with both restored it revives; and extending
// the revived run's end is refused once its agent is withdrawn from an
// enforced kind.
func TestPG_ReviveAndExtendRecheckOwnerAuthority(t *testing.T) {
	h, sec := newRunOwnerPGHarness(t)
	ctx := context.Background()
	st := h.srv.cfg.Store
	const owner = "sub-pg-owner"

	run, err := st.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedBy: owner, Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunRunning, RunnerTarget: "docker", Task: "t"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if run.State != types.RunRunning {
		if ok, err := st.UpdateRunStateIf(ctx, run.ID, run.State, types.RunRunning); err != nil || !ok {
			t.Fatalf("UpdateRunStateIf: %v %v", ok, err)
		}
	}
	if err := st.SetSandboxRef(ctx, run.ID, "wardyn-agent-"+run.ID.String()); err != nil {
		t.Fatalf("SetSandboxRef: %v", err)
	}
	if ok, err := st.(store.RunLoser).MarkRunLost(ctx, run.ID, types.LostOutage, time.Now(), 0); err != nil || !ok {
		t.Fatalf("MarkRunLost: %v %v", ok, err)
	}
	g, err := st.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID,
		Spec: apiKeyGrantSpec("api.anthropic.com", "anthropic-api-key")})
	if err != nil {
		t.Fatalf("CreateGrant: %v", err)
	}
	cfg, err := runner.BuildProxyConfig(run.ID, runner.ProxyConfig{
		RunToken: "old", ControlPlaneURL: "http://127.0.0.1:8081",
		Policy:    types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}},
		Injection: []runner.InjectionGrant{{GrantID: g.ID, Rule: egress.InjectionRule{Host: "api.anthropic.com", Header: "x-api-key", Format: "%s"}}},
	}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	rn := &pgReviveRunner{fakeRunner: &fakeRunner{}, cfg: cfg}
	h.srv.cfg.Runner = rn
	h.srv.cfg.ControlPlaneURL = "http://127.0.0.1:8080"

	path := "/api/v1/runs/" + run.ID.String()
	session := ssoSession(t, owner, ownerEmail, oidc.RoleUser)
	revive := func(asOwner bool) int {
		if asOwner {
			return doSSO(t, h.srv, http.MethodPost, path+"/revive", session, "").Code
		}
		return do(t, h.srv, http.MethodPost, path+"/revive", adminToken, "").Code
	}
	lost := func() bool {
		r, err := st.GetRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		return r.LostAt != nil
	}

	deny, err := st.UpsertCapabilityGrant(ctx, grant(types.CapabilitySubjectUser, ownerEmail, capAgent, "claude-code", types.CapabilityDeny))
	if err != nil {
		t.Fatalf("UpsertCapabilityGrant: %v", err)
	}
	for _, asOwner := range []bool{true, false} {
		if code := revive(asOwner); code != http.StatusForbidden || !lost() || rn.replaced != 0 {
			t.Fatalf("revive (owner %v) under a deny on the owner's email = %d (lost %v, replaced %d); want 403, still lost",
				asOwner, code, lost(), rn.replaced)
		}
	}
	if err := st.DeleteCapabilityGrant(ctx, deny.ID); err != nil {
		t.Fatalf("DeleteCapabilityGrant: %v", err)
	}

	if code := revive(true); code != http.StatusConflict || !lost() {
		t.Fatalf("revive with the model credential erased = %d (lost %v); want 409, still lost", code, lost())
	}
	if err := sec.Put(ctx, "anthropic-api-key", []byte("sk-ant-test")); err != nil {
		t.Fatalf("put secret: %v", err)
	}
	if code := revive(true); code != http.StatusOK || lost() || rn.replaced != 1 {
		t.Fatalf("revive with the authority restored = %d (lost %v, replaced %d); want 200, live", code, lost(), rn.replaced)
	}

	now := time.Now().UTC().Truncate(time.Second)
	end := now.Add(24 * time.Hour)
	if ok, err := st.(store.RunLeaser).SetRunEndAndWait(ctx, run.ID, run.RunLimits, nil, 0, &end, 0); err != nil || !ok {
		t.Fatalf("SetRunEndAndWait: %v %v", ok, err)
	}
	if _, err := st.PutCapabilityEnforcement(ctx, map[string]bool{capAgent: true}); err != nil {
		t.Fatalf("PutCapabilityEnforcement: %v", err)
	}
	w := doSSO(t, h.srv, http.MethodPatch, path, session, endsAtBody(now.Add(7*24*time.Hour)))
	if w.Code != http.StatusForbidden {
		t.Fatalf("extend with the agent withdrawn = %d %s, want 403", w.Code, w.Body.String())
	}
	if r, _ := st.GetRun(ctx, run.ID); r.EndsAt == nil || !r.EndsAt.Equal(end) {
		t.Errorf("stored end = %v, want %v untouched", r.EndsAt, end)
	}
}
