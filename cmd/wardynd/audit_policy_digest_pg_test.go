// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAuditChain_PolicyDigestSurvivesMaskingAndSealing: through the real chain, with masking on, a mask
// scope label added to the row and sealing armed, a run's rows are stamped from agent_runs.policy_digest
// and the run.policy.resolve row read back from the database recomputes to that same digest. The
// digest is taken before masking and the check reads the row after it, so this is what proves the two
// agree for a policy that holds no registered secret value.
func TestAuditChain_PolicyDigestSurvivesMaskingAndSealing(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	k, err := kek.NewLocalPurpose(id, kek.PurposeCred)
	if err != nil {
		t.Fatal(err)
	}
	pending := make([]byte, 32)
	_, _ = rand.Read(pending)
	src := newAuditSealSource(audit.SealFields)
	src.arm(newAuditSealer(subjectSealKeys{subjectkeytest.Manager(pool, k)}, pool, pending, false))

	reg := secretmask.NewRegistry()
	reg.AddGlobal(0, "test", "unrelated", time.Now(), []byte("a-registered-secret-value"))
	scope := &maskScope{}
	scope.arm(func(uuid.UUID) bool { return false }) // this process holds no manifest: every run row is labelled
	rec, _, _, _, err := buildAuditChain(t.Context(), "", "", "", pool, reg, src, serveChain{scope: scope})
	if err != nil {
		t.Fatal(err)
	}

	spec := types.RunPolicySpec{AllowedDomains: []string{"api.example.com"}, FirstUseApproval: types.FirstUseAlwaysDeny,
		// A scope is a RawMessage that keeps the writer's member order, which the database's JSONB rewrites.
		EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"secret_name":"s","host":"api.example.com"}`), TTLSeconds: 60}}}
	runID := uuid.New()
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target, policy_digest)
		 VALUES ($1,'tester','claude-code','acme/widgets','CC2','RUNNING',$2,'docker',$3)`,
		runID, "spiffe://wardyn.local/run/"+runID.String(), spec.Digest()); err != nil {
		t.Fatal(err)
	}

	resolve := func(action string, data any) uuid.UUID {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), RunID: &runID, ActorType: types.ActorSystem,
			Actor: "wardynd", Action: action, Target: runID.String(), Outcome: "success", Data: raw}
		if err := rec.Record(t.Context(), ev); err != nil {
			t.Fatal(err)
		}
		return ev.ID
	}
	envelope := struct {
		types.RunPolicySpec
		DiskMiBFilled bool `json:"disk_mib_filled"`
	}{spec, true}
	resolveID := resolve("run.policy.resolve", envelope)
	actionID := resolve("egress.allow", map[string]string{"host": "api.example.com", "policy_digest": "forged"})

	read := func(id uuid.UUID) map[string]json.RawMessage {
		var raw []byte
		if err := pool.QueryRow(t.Context(), `SELECT data FROM audit_events WHERE id=$1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("stored data %s: %v", raw, err)
		}
		return m
	}
	for name, id := range map[string]uuid.UUID{"run.policy.resolve": resolveID, "egress.allow": actionID} {
		m := read(id)
		if string(m["policy_digest"]) != `"`+spec.Digest()+`"` {
			t.Errorf("%s: stored policy_digest = %s, want %q", name, m["policy_digest"], spec.Digest())
		}
		if string(m["mask_scope"]) != `"globals_only"` {
			t.Errorf("%s: stored mask_scope = %s, want the label this test arms", name, m["mask_scope"])
		}
	}
	stored, _ := json.Marshal(read(resolveID))
	var back types.RunPolicySpec
	if err := json.Unmarshal(stored, &back); err != nil {
		t.Fatal(err)
	}
	if back.Digest() != spec.Digest() {
		t.Errorf("digest recomputed from the stored resolve row = %s, want %s", back.Digest(), spec.Digest())
	}
}
