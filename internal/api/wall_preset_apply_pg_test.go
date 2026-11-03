// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestWallPresetPGSealAndMint: a run launched with a stored organisation preset over Postgres keeps
// the frozen copy in agent_runs.wall_preset; the seal a dispatch and a revive run, and the mint door,
// read that copy, so deleting the preset afterwards withholds nothing less; and no decider admits a
// host the copy denies.
func TestWallPresetPGSealAndMint(t *testing.T) {
	pool := throwawayPGPool(t)
	testutil.SetBoundaryNoLimits(t, pool)
	e := newGovEnvOn(t, pool)
	ctx := context.Background()
	on := true
	stored, err := e.pg.CreateWallPreset(ctx, types.WallPreset{ID: uuid.NewString(), Scope: types.WallPresetScopeOrg, Name: "Org tight",
		UpdatedBy: "sub-admin", Spec: types.PersonalWallsSpec{DeniedDomains: wallsList("evil.example"),
			WithheldGrants: []types.GrantSpec{{Kind: types.GrantSSHKey}}, PushRules: &types.PersonalWallsPushRules{DenyAll: &on}}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	w := e.call(e.user, http.MethodPost, "/api/v1/runs", `{"agent":"claude-code","task":"t","wall_preset":"`+stored.ID+`"}`)
	var created createRunResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	run, err := e.pg.GetRun(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p := run.WallPreset; p == nil || p.ID != stored.ID || p.Version != 1 || p.Scope != types.WallPresetScopeOrg ||
		!p.Spec.DeniesAllPushes() || p.Spec.DeniedDomains == nil || !slices.Equal(*p.Spec.DeniedDomains, []string{"evil.example"}) {
		t.Fatalf("agent_runs.wall_preset read back as %+v, want the frozen Org tight copy", run.WallPreset)
	}
	if _, err := e.pg.DeleteWallPreset(ctx, uuid.MustParse(stored.ID), nil); err != nil {
		t.Fatal(err)
	}
	if run, err = e.pg.GetRun(ctx, created.ID); err != nil || run.WallPreset == nil || run.WallPreset.ID != stored.ID {
		t.Fatalf("after the preset's delete the run reads %+v (%v), want its copy kept", run.WallPreset, err)
	}

	ssh := types.GrantSpec{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"host":"git.corp.example","key_secret_ref":"deploy-key"}`)}
	policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "evil.example"}}
	layers := e.srv.sealRunPolicy(ctx, run, &policy, knobProvenance{})
	if !slices.Contains(policy.DeniedDomains, "evil.example") {
		t.Errorf("sealed denied %v, want the preset's evil.example", policy.DeniedDomains)
	}
	if !slices.ContainsFunc(layers, func(l sealLayer) bool { return l.denyAllPushes }) {
		t.Error("no seal layer refuses every push; a revive would not re-assert the preset's deny_all")
	}
	if w := (withholding{layers: layers}); !w.credentialWithheld(ssh) {
		t.Error("dispatch: the preset's withheld ssh_key was not withheld")
	}
	live, err := e.srv.liveWithholding(ctx, run)
	if err != nil || !live.credentialWithheld(ssh) || !live.withheldAny([]string{"evil.example"}) {
		t.Errorf("mint: %v, ssh_key withheld %v, evil.example withheld %v", err, live.credentialWithheld(ssh), live.withheldAny([]string{"evil.example"}))
	}

	ap, err := e.h.approvals.Request(ctx, types.ApprovalRequest{RunID: run.ID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":"Evil.example"}`)})
	if err != nil {
		t.Fatal(err)
	}
	decider := e.otherServer(func(c *Config) { c.Approvals = e.h.approvals })
	if w := doSSO(t, decider, http.MethodPost, "/api/v1/approvals/"+ap.ID.String()+"/approve", e.alice, `{}`); w.Code != http.StatusForbidden ||
		wireReason(t, w) != reasonWallPresetHost {
		t.Errorf("approving a host the preset denies: %d %s, want 403 %s", w.Code, w.Body, reasonWallPresetHost)
	}
}
