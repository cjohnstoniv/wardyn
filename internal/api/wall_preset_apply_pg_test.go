// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
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

// queueRacePreset is a stored organisation preset that refuses every push, and the edit that a race
// lands between the queue's check and the read it guards: version 2, the push refusal removed.
func queueRacePreset(t *testing.T, e *queueEnv) (types.WallPreset, func()) {
	t.Helper()
	on := true
	p, err := e.pg.CreateWallPreset(context.Background(), types.WallPreset{ID: uuid.NewString(), Scope: types.WallPresetScopeOrg,
		Name: "No pushes", UpdatedBy: "sub-admin", Spec: types.PersonalWallsSpec{DeniedDomains: wallsList("evil.example"),
			PushRules: &types.PersonalWallsPushRules{DenyAll: &on}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	return p, func() {
		once.Do(func() {
			widened := p
			widened.Spec = types.PersonalWallsSpec{DeniedDomains: wallsList("evil.example")}
			if _, err := e.pg.UpdateWallPreset(context.Background(), widened, 1, nil); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestPG_QueueWallPresetEditedAfterTheEnqueueFold: the preset is edited after the create's fold froze
// version 1 and before the queue row is written. The row records the version the run was reviewed
// under, never a second read, so the start sees the edit and ends the run queue_policy_changed.
func TestPG_QueueWallPresetEditedAfterTheEnqueueFold(t *testing.T) {
	e := newQueueEnv(t)
	p, edit := queueRacePreset(t, e)
	e.srv.queueRace = func(point string) {
		if point == queueRaceEnqueueAfterFold {
			edit()
		}
	}
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	queued := e.queuedLaunch(e.person, pool, `,"wall_preset":"`+p.ID+`"`)
	entry, err := e.pg.GetRunQueueEntry(context.Background(), queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Version int `json:"wall_preset_version"`
	}
	if err := json.Unmarshal(entry.PolicyRef, &ref); err != nil || ref.Version != 1 {
		t.Errorf("policy_ref = %s (%v), want wall_preset_version 1, the version the fold froze", entry.PolicyRef, err)
	}
	if r := e.run(queued.ID); r.WallPreset == nil || r.WallPreset.Version != 1 || !r.WallPreset.Spec.DeniesAllPushes() {
		t.Errorf("the queued run's copy = %+v, want version 1 refusing every push", r.WallPreset)
	}
	e.free(occupant)
	got := e.waitFailed(queued.ID)
	if got.StatusDetail != "queue: queue_policy_changed" {
		t.Errorf("a run whose preset was edited before its queue row = %s %q, want queue_policy_changed", got.State, got.StatusDetail)
	}
}

// TestPG_QueueWallPresetEditedAfterTheStartPrecheck: the preset is edited after the start's check of
// the reference passed and before its fold reads the preset again. The start compares what the fold
// froze with the reference before anything is minted or written, so the run ends queue_policy_changed
// and never starts with pushes permitted.
func TestPG_QueueWallPresetEditedAfterTheStartPrecheck(t *testing.T) {
	e := newQueueEnv(t)
	p, edit := queueRacePreset(t, e)
	e.srv.queueRace = func(point string) {
		if point == queueRaceStartAfterPrecheck {
			edit()
		}
	}
	pool := e.cappedPool("Busy", 1)
	occupant := e.occupy(pool)
	queued := e.queuedLaunch(e.person, pool, `,"wall_preset":"`+p.ID+`"`)
	e.free(occupant)
	got := e.waitFailed(queued.ID)
	if got.StatusDetail != "queue: queue_policy_changed" || got.SPIFFEID != "" {
		t.Errorf("a run whose preset was edited between the start's check and its fold = %s %q (identity %q), want queue_policy_changed and nothing minted",
			got.State, got.StatusDetail, got.SPIFFEID)
	}
	if got.WallPreset == nil || got.WallPreset.Version != 1 || !got.WallPreset.Spec.DeniesAllPushes() {
		t.Errorf("the run's copy = %+v, want the reviewed version 1 kept, never the edit", got.WallPreset)
	}
}

// TestPG_QueueWallPresetChangedWhileWaiting: a queued run whose wall preset is edited, deleted, or
// no longer open to its owner while it waits ends queue_policy_changed at its start; it never starts
// under the edit, nor without a preset.
func TestPG_QueueWallPresetChangedWhileWaiting(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, e *queueEnv, p types.WallPreset, run uuid.UUID)
		want   types.WallPresetChange
	}{
		{"edited", func(t *testing.T, e *queueEnv, p types.WallPreset, _ uuid.UUID) {
			p.Spec = types.PersonalWallsSpec{DeniedDomains: wallsList("evil.example")}
			if _, err := e.pg.UpdateWallPreset(context.Background(), p, 1, nil); err != nil {
				t.Fatal(err)
			}
		}, types.WallPresetChange{Version: 2}},
		{"deleted", func(t *testing.T, e *queueEnv, p types.WallPreset, _ uuid.UUID) {
			if _, err := e.pg.DeleteWallPreset(context.Background(), uuid.MustParse(p.ID), nil); err != nil {
				t.Fatal(err)
			}
		}, types.WallPresetChange{Gone: true}},
		{"no longer open to the owner", func(_ *testing.T, e *queueEnv, _ types.WallPreset, run uuid.UUID) {
			e.exec(`UPDATE run_queue SET authority = jsonb_set(authority, '{groups}', '[]'::jsonb) WHERE run_id = $1`, run)
		}, types.WallPresetChange{Gone: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newQueueEnv(t)
			on := true
			p, err := e.pg.CreateWallPreset(context.Background(), types.WallPreset{ID: uuid.NewString(), Scope: types.WallPresetScopeGroup,
				GroupID: "eng", Name: "Eng no pushes", UpdatedBy: "sub-admin",
				Spec: types.PersonalWallsSpec{DeniedDomains: wallsList("evil.example"), PushRules: &types.PersonalWallsPushRules{DenyAll: &on}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			member := ssoSessionWith(t, e.personP, types.UserTypeStandard, "eng")
			pool := e.cappedPool("Busy", 1)
			occupant := e.occupy(pool)
			queued := e.queuedLaunch(member, pool, `,"wall_preset":"`+p.ID+`"`)
			tc.change(t, e, p, queued.ID)
			e.free(occupant)
			got := e.waitFailed(queued.ID)
			if got.StatusDetail != "queue: queue_policy_changed" || got.SPIFFEID != "" {
				t.Errorf("%s: %s %q (identity %q), want queue_policy_changed and nothing minted", tc.name, got.State, got.StatusDetail, got.SPIFFEID)
			}
			if got.WallPreset == nil || got.WallPreset.Version != 1 || !got.WallPreset.Spec.DeniesAllPushes() {
				t.Errorf("%s: the run's copy = %+v, want the reviewed version 1", tc.name, got.WallPreset)
			} else if c := got.WallPreset.Changed; c == nil || *c != tc.want {
				t.Errorf("%s: the run's wall preset change = %+v, want %+v", tc.name, c, tc.want)
			}
		})
	}
}
