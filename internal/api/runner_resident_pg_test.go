// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func syncAuditRows(a *syncAudit, action string) []types.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []types.AuditEvent
	for _, ev := range a.events {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// A changed posture is stored and writes runner.posture.record with the
// runner_asserted source; a repeat of the same posture writes nothing.
func TestPosture_EventRecordedAndAudited(t *testing.T) {
	_, cfg, rn, _, _, _ := runnerExitAPIFixture(t)
	srv := New(cfg)
	ctx := t.Context()
	p := types.RunnerPosture{MDMManaged: true, DiskEncrypted: true, OS: "darwin", OSVersion: "15.2", Evidence: types.PostureEvidenceMDMFile}
	ev := runnerwire.Event{Kind: runnerwire.EventPosture, Data: mustJSON(p)}
	for range 2 {
		if err := srv.EventFromRunner(ctx, rn.ID, ev); err != nil {
			t.Fatal(err)
		}
	}
	rows := syncAuditRows(cfg.Audit.(*syncAudit), placement.ActionRunnerPostureRecord)
	if len(rows) != 1 || rows[0].Target != rn.ID.String() {
		t.Fatalf("runner.posture.record rows = %+v, want one for the change and none for the repeat", rows)
	}
	var data struct {
		Posture types.RunnerPosture `json:"posture"`
		Source  string              `json:"posture_source"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil || data.Posture != p || data.Source != types.RunnerPostureSourceRunnerAsserted {
		t.Fatalf("audit data = %s (%v)", rows[0].Data, err)
	}
	stored, err := cfg.Store.(store.PG).GetRunner(ctx, rn.ID)
	if err != nil || stored.Posture != p || stored.PostureSource != types.RunnerPostureSourceRunnerAsserted {
		t.Fatalf("stored posture = %+v %q (%v)", stored.Posture, stored.PostureSource, err)
	}
	for name, bad := range map[string]json.RawMessage{
		"no evidence":   mustJSON(types.RunnerPosture{OS: "linux"}),
		"no os":         mustJSON(types.RunnerPosture{Evidence: types.PostureEvidenceOSProbe}),
		"unknown field": json.RawMessage(`{"os":"linux","evidence":"os_probe","attested":true}`),
	} {
		if err := srv.EventFromRunner(ctx, rn.ID, runnerwire.Event{Kind: runnerwire.EventPosture, Data: bad}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The runner's resident_erase for a run bound to it writes
// runner.resident.erase naming the grant and the outcome; a grant that is not
// the run's runner_resident grant, or another runner's run, is refused.
func TestResident_ErasedAtRunEndAndAudited(t *testing.T) {
	_, cfg, rn, run, _, _ := runnerExitAPIFixture(t)
	srv := New(cfg)
	ctx := t.Context()
	grant, err := cfg.Store.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Delivery: types.GrantDeliveryResident,
		Spec: types.GrantSpec{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"TOKEN","secret_name":"ci-token"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	erase := func(runnerID, grantID uuid.UUID, outcome string) error {
		return srv.EventFromRunner(ctx, runnerID, runnerwire.Event{Kind: runnerwire.EventResidentErase, RunID: &run.ID,
			Data: mustJSON(runnerwire.ResidentErase{GrantID: grantID, Outcome: outcome})})
	}
	if err := erase(rn.ID, grant.ID, runnerwire.ResidentErased); err != nil {
		t.Fatal(err)
	}
	if err := erase(rn.ID, uuid.New(), runnerwire.ResidentErased); err == nil {
		t.Fatal("an unknown grant was accepted")
	}
	if err := erase(uuid.New(), grant.ID, runnerwire.ResidentErased); err == nil {
		t.Fatal("another runner's run was accepted")
	}
	rows := syncAuditRows(cfg.Audit.(*syncAudit), placement.ActionRunnerResidentErase)
	if len(rows) != 1 || rows[0].Target != grant.ID.String() || rows[0].Outcome != "success" || rows[0].RunID == nil || *rows[0].RunID != run.ID {
		t.Fatalf("runner.resident.erase rows = %+v", rows)
	}
}
