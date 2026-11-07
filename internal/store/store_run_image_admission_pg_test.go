// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.image_admission: the image admission verdict create recorded for the
// run's member image. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func admissionVerdictFixture() *types.ImageAdmissionVerdict {
	d := "sha256:" + strings.Repeat("b", 64)
	return &types.ImageAdmissionVerdict{Mode: types.ImageAdmissionEnforce, Verdict: types.ImageAdmissionAdmitted, Digest: d,
		PinnedRef: "registry.example.com/team/app:1@" + d, Signer: "corp-ci", Identity: "https://ci.example/build@refs/heads/main",
		Issuer: "https://token.actions.githubusercontent.com", Attestations: []string{"https://slsa.dev/provenance/v1"}}
}

// TestPG_RunImageAdmission_RoundTrips: a verdict and no verdict (NULL: admission off, no member image, or a run
// from before the column) each survive create and read.
func TestPG_RunImageAdmission_RoundTrips(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	notApplicable := &types.ImageAdmissionVerdict{Mode: types.ImageAdmissionEnforce, Verdict: types.ImageAdmissionNotApplicable}
	for _, v := range []*types.ImageAdmissionVerdict{admissionVerdictFixture(), notApplicable, nil} {
		r := newRun(types.RunRunning)
		r.ImageAdmission = v
		created := persistRun(t, ctx, pool, r)
		got, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range []types.AgentRun{created, got} {
			if !reflect.DeepEqual(run.ImageAdmission, v) {
				t.Errorf("verdict %+v read back as %+v", v, run.ImageAdmission)
			}
		}
	}
}

// TestPG_RunImageAdmission_SetWritesIt: a launch's verdict for the convention image, written after create,
// reads back with its ref; nil clears it.
func TestPG_RunImageAdmission_SetWritesIt(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunPending)
	persistRun(t, ctx, pool, r)
	v := admissionVerdictFixture()
	v.Ref = "registry.example.com/team/agent:1"
	for _, want := range []*types.ImageAdmissionVerdict{v, nil} {
		if err := pg.SetRunImageAdmission(ctx, r.ID, want); err != nil {
			t.Fatal(err)
		}
		got, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.ImageAdmission, want) {
			t.Errorf("verdict %+v read back as %+v", want, got.ImageAdmission)
		}
	}
}

// TestPG_RunImageAdmission_StartRewritesIt: a queued run joins with the verdict its create reached, and the
// start, which checks the image afresh, writes the verdict it reached, including none.
func TestPG_RunImageAdmission_StartRewritesIt(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 2)
	now := time.Now().UTC()
	queuedV := admissionVerdictFixture()
	startV := admissionVerdictFixture()
	startV.Digest, startV.PinnedRef = "sha256:"+strings.Repeat("c", 64), "registry.example.com/team/app:1@sha256:"+strings.Repeat("c", 64)
	rewritten, cleared := poolRun(p, types.RunQueued, "alice"), poolRun(p, types.RunQueued, "bob")
	for _, r := range []types.AgentRun{rewritten, cleared} {
		r.ImageAdmission = queuedV
		queued, err := pg.CreateQueuedRun(ctx, r, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0)
		if err != nil || !reflect.DeepEqual(queued.ImageAdmission, queuedV) {
			t.Fatalf("queued run verdict = %+v, %v; want %+v", queued.ImageAdmission, err, queuedV)
		}
	}
	if promoted, _, err := pg.PromoteRunQueue(ctx, pass(p), admitAll); err != nil || len(promoted) != 2 {
		t.Fatalf("promote = %+v, %v; want both waiting runs", promoted, err)
	}
	for _, tc := range []struct {
		run types.AgentRun
		v   *types.ImageAdmissionVerdict
	}{{rewritten, startV}, {cleared, nil}} {
		started, ok, err := pg.StartQueuedRun(ctx, tc.run.ID, store.QueueStart{SPIFFEID: "spiffe://x/" + tc.run.ID.String(),
			ConfinementClass: types.CC2, ImageAdmission: tc.v})
		if err != nil || !ok || !reflect.DeepEqual(started.ImageAdmission, tc.v) {
			t.Fatalf("start with verdict %+v = %+v ok=%v err=%v", tc.v, started.ImageAdmission, ok, err)
		}
	}
}
