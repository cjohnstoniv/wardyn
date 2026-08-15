// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/groundtruth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSynthesizeProfile_StampsEbpfGroundtruthCaveat is
// W20-W20-groundtruth-mapper-4's other call site: a synthesized profile
// (POST /api/v1/runs/{id}/profile) must carry the SAME one-line eBPF sensor
// coverage note reconcileRecordRun stamps onto RecordTaskResult.Caveats —
// before this fix, ebpf_groundtruth's per-kind state existed only on the
// admin-only /healthz endpoint, nowhere Record Mode's own review surfaces
// (this one included) would show it.
func TestSynthesizeProfile_StampsEbpfGroundtruthCaveat(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	hb := groundtruth.HeartbeatEventWithDropped(0, 9, map[string]uint64{groundtruth.ActionProcessExec: 9}) // partial: 2 kinds never arrived
	hb.Time = time.Now()
	fake := &recordStore{
		run: types.AgentRun{ID: runID, Agent: "claude-code", Repo: "org/repo",
			State: types.RunCompleted, ConfinementClass: types.CC2},
		events:    []types.AuditEvent{egressAllowEvent(runID, "registry.npmjs.org")},
		heartbeat: &hb,
	}
	cfg := baseTestConfig(h, fake)
	cfg.DefaultPolicy = types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/profile", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST .../profile code = %d, body=%s", w.Code, w.Body.String())
	}
	var resp profileResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, warn := range resp.Warnings {
		if strings.Contains(warn, "kernel ground truth") && strings.Contains(warn, "partial") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want one naming the partial eBPF sensor coverage", resp.Warnings)
	}
}
