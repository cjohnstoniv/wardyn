// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The org engine of a local run scans with the run's dispatch-time workspace-secret corpus, kept in
// the run's sealed org-side config, while the laptop copy carries neither the names nor the values.
func TestViaOrg_EngineKeepsTheDeclaredInspectionCorpus(t *testing.T) {
	srv, _, _, run := localDispatchFixture(t, types.PlacementLocal)
	ctx := context.Background()
	spec := localOrgNetworkSpec(run.ID)
	spec.ProxyConfig.ControlPlaneURL = "http://127.0.0.1:9"
	spec.ProxyConfig.Policy.LLMInspection = &types.LLMInspectionSpec{Mode: "block", DetectSecrets: true,
		WorkspaceSecretNames: []string{"ws-deploy-key"}, WorkspaceSecretValues: []string{"WS-DECLARED-VALUE-31c7"}}
	if !srv.classifyLocalDispatch(ctx, run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), types.SiteConfig{}, &spec, nil, llmTransport{}, false) {
		t.Fatal("an all-own plan was refused")
	}
	if li := spec.ProxyConfig.Policy.LLMInspection; li == nil || len(li.WorkspaceSecretNames)+len(li.WorkspaceSecretValues) != 0 {
		t.Fatalf("the laptop copy carries the corpus: %+v", li)
	}
	cfg, err := srv.viaOrgEngineConfig(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if li := cfg.Policy.LLMInspection; li == nil || !slices.Contains(li.WorkspaceSecretValues, "WS-DECLARED-VALUE-31c7") {
		t.Fatalf("the org engine's inspection lost the declared corpus: %+v", li)
	}
}

// The org engine drops the k8s runner's sidecar settings (self-lapse, the decision sink, the
// in-cluster hard deny) if a run's stored config ever carries them: they describe a proxy beside a
// runner, not the organisation's engine.
func TestViaOrg_EngineDropsTheRunnerSidecarSettings(t *testing.T) {
	srv, _, _, run := localDispatchFixture(t, types.PlacementLocal)
	ctx := context.Background()
	spec := localOrgNetworkSpec(run.ID)
	spec.ProxyConfig.ControlPlaneURL = "http://127.0.0.1:9"
	if !srv.classifyLocalDispatch(ctx, run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), types.SiteConfig{}, &spec, nil, llmTransport{}, false) {
		t.Fatal("an all-own plan was refused")
	}
	raw, err := srv.loadRunProxyConfig(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	delete(stored, "decision_spool_dir")
	stored["self_lapse"] = true
	stored["decision_sink"] = map[string]any{"url": "tls://runnerd.wardyn.svc:9443", "spki_sha256": strings.Repeat("ab", 32), "key": strings.Repeat("cd", 32)}
	stored["hard_deny"] = map[string]any{"suffixes": []string{"cluster.local"}}
	if err := srv.storeRunProxyConfig(ctx, run.ID, mustJSON(stored)); err != nil {
		t.Fatal(err)
	}
	cfg, err := srv.viaOrgEngineConfig(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SelfLapse || cfg.DecisionSink != nil || cfg.HardDeny != nil {
		t.Fatalf("the org engine kept the runner's sidecar settings: self_lapse %v sink %+v hard_deny %+v", cfg.SelfLapse, cfg.DecisionSink, cfg.HardDeny)
	}
}
