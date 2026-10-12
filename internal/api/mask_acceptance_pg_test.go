// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The #1512 acceptance pins that the masking-manifest tests did not already
// carry: a run that used no secrets, kill and revoke with no masking
// continuity, and the evidence label on a recording upload. WARDYN_TEST_PG
// guarded (throwawayPGPool), like mask_manifest_pg_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run that truly used no secret, injection or minted token is known empty: its
// manifest completes with zero values, covers the run, and its upload is stored
// byte for byte.
func TestPG_MaskManifest_ARunWithNoSecretsIsCoveredAndUploadsNormally(t *testing.T) {
	l := newMaskLab(t)
	rp := l.replica()
	run := l.run()
	rp.dispatch(t, run)

	if !rp.srv.maskCovered(context.Background(), run.ID) {
		t.Fatal("a completed zero-value manifest does not cover the run")
	}
	body := "plain output, nothing to hide\n"
	if w := l.upload(rp, run, body); w.Code != http.StatusNoContent {
		t.Fatalf("upload of a run with no secrets: %d %s, want 204", w.Code, w.Body.String())
	}
	if got := string(rp.rec.saved); got != body {
		t.Errorf("stored %q, want the body byte-identical %q", got, body)
	}
}

// Kill and revoke need no masking continuity: on a run whose manifest is
// incomplete, or that has none, with Postgres-backed masking on, the kill lands,
// the run is KILLED, and both revocations run.
func TestPG_KillAndRevokeNeedNoMaskingContinuity(t *testing.T) {
	l := newMaskLab(t)
	rp := l.replica()
	rp.srv.cfg.Broker = l.h.broker
	ctx := context.Background()

	partial, legacy := l.run(), l.run()
	if err := rp.srv.cfg.MaskManifests.Start(ctx, partial.ID, maskOwner); err != nil {
		t.Fatal(err)
	}
	for name, run := range map[string]types.AgentRun{"incomplete": partial, "legacy": legacy} {
		t.Run(name, func(t *testing.T) {
			if rp.srv.maskCovered(ctx, run.ID) {
				t.Fatal("the fixture run is covered; the premise (no masking continuity) is gone")
			}
			tok := mintRunTokenAs(t, l.h, run.ID, maskOwner)
			w := do(t, rp.srv, http.MethodPost, "/api/v1/runs/"+run.ID.String()+"/kill", adminToken, "")
			if w.Code != http.StatusAccepted {
				t.Fatalf("kill: %d %s, want 202 (no mask_state_unavailable on this path)", w.Code, w.Body.String())
			}
			got, err := rp.srv.cfg.Store.GetRun(ctx, run.ID)
			if err != nil || got.State != types.RunKilled {
				t.Fatalf("run after kill = %q, %v, want KILLED", got.State, err)
			}
			if _, err := l.h.idp.Verify(ctx, tok, internalAudience); err == nil {
				t.Error("the run token still verifies after the kill; the identity revoke did not run")
			}
			if n := len(l.h.broker.revoked); n == 0 || l.h.broker.revoked[n-1] != run.ID {
				t.Errorf("broker revoke did not run for %s: %v", run.ID, l.h.broker.revoked)
			}
		})
	}
}

// A recording uploaded for a run whose evidence is runner_asserted is audited as
// such, on the outcome row and on the part-limit failure row; a substrate run's
// rows carry no such key.
func TestPG_RecordingUploadOfARunnerAssertedRunIsLabelled(t *testing.T) {
	l := newMaskLab(t)
	rp := l.replica()
	asserted, substrate := l.run(), l.run()
	for run, src := range map[uuid.UUID]types.RunEvidenceSource{asserted.ID: types.RunEvidenceRunnerAsserted, substrate.ID: types.RunEvidenceSubstrate} {
		if _, err := l.pool.Exec(context.Background(), `UPDATE agent_runs SET evidence_source=$2 WHERE id=$1`, run, string(src)); err != nil {
			t.Fatal(err)
		}
	}
	rp.dispatch(t, asserted)
	rp.dispatch(t, substrate)

	rows := func(run types.AgentRun) (outcome, partLimit map[string]any) {
		tok := mintRunTokenAs(t, l.h, run.ID, maskOwner)
		path := "/api/v1/internal/recordings/" + run.ID.String()
		if w := do(t, rp.srv, http.MethodPut, path, tok, "cast\n"); w.Code != http.StatusNoContent {
			t.Fatalf("upload: %d %s", w.Code, w.Body.String())
		}
		over := path + "/parts/" + strconv.Itoa(types.RecordingMaxParts+1)
		if w := do(t, rp.srv, http.MethodPut, over, tok, "cast\n"); w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("part over the limit: %d %s, want 413", w.Code, w.Body.String())
		}
		for _, ev := range l.rec.snapshot() {
			if ev.Action != "recording.upload" || ev.RunID == nil || *ev.RunID != run.ID {
				continue
			}
			d := map[string]any{}
			if len(ev.Data) > 0 {
				if err := json.Unmarshal(ev.Data, &d); err != nil {
					t.Fatal(err)
				}
			}
			if ev.Outcome == "success" {
				outcome = d
			} else {
				partLimit = d
			}
		}
		if outcome == nil || partLimit == nil {
			t.Fatalf("missing recording.upload rows for %s: %s", run.ID, auditDump(l.rec.snapshot(), run.ID))
		}
		return outcome, partLimit
	}

	outcome, partLimit := rows(asserted)
	for name, d := range map[string]map[string]any{"outcome": outcome, "part-limit": partLimit} {
		if d["evidence_source"] != string(types.RunEvidenceRunnerAsserted) {
			t.Errorf("runner_asserted run's %s row = %v, want evidence_source runner_asserted", name, d)
		}
	}
	outcome, partLimit = rows(substrate)
	for name, d := range map[string]map[string]any{"outcome": outcome, "part-limit": partLimit} {
		if _, has := d["evidence_source"]; has {
			t.Errorf("substrate run's %s row = %v, want no evidence_source key", name, d)
		}
	}
}
