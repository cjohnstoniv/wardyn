// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A requirement row that is not an operator's own (reachable only with the operator-set gate off) is
// repository content: its private-range literal host widens the run only where the ceiling already
// covers it. An operator_set row, and a name, are never clamped.
func TestEgressBound_ClampsANonOperatorSetPrivateLiteral(t *testing.T) {
	ws := types.Workspace{}
	scan := types.WorkspaceRequirement{Level: "required", Provenance: "scan_seeded"}
	op := types.WorkspaceRequirement{Level: "required", Provenance: "operator_set"}
	var none *egressBound
	if !none.admits(scan, "10.0.0.5", ws) {
		t.Error("no bound: a row is admitted as before")
	}
	b := &egressBound{}
	if b.admits(scan, "10.0.0.5", ws) {
		t.Error("a scan-seeded private literal outside the ceiling was admitted")
	}
	if !b.admits(op, "10.0.0.5", ws) || !b.admits(scan, "registry.example.com", ws) {
		t.Error("an operator_set row or a name was clamped")
	}
	if !(&egressBound{trusted: []string{"10.0.0.5"}}).admits(scan, "10.0.0.5", ws) {
		t.Error("a private literal the ceiling names exactly was refused")
	}
}

// The confined replay holds a scan-seeded private literal to the run's ceiling when the gate is off.
func TestConfinedEgressDomains_ClampsAScanSeededPrivateLiteral(t *testing.T) {
	srv := &Server{cfg: Config{RequireOperatorSetEgress: false}}
	ws := types.Workspace{Requirements: map[string]types.WorkspaceRequirement{
		"egress:10.0.0.5": {Level: "required", Provenance: "scan_seeded"},
		"egress:ok.test":  {Level: "required", Provenance: "scan_seeded"},
	}}
	got := srv.confinedEgressDomains(ws, nil)
	if slices.Contains(got, "10.0.0.5") || !slices.Contains(got, "ok.test") {
		t.Fatalf("domains = %v, want ok.test and not the private literal", got)
	}
	if got := srv.confinedEgressDomains(ws, []string{"10.0.0.5"}); !slices.Contains(got, "10.0.0.5") {
		t.Fatalf("domains = %v, want the literal the ceiling names", got)
	}
}

// scanSeededPrivateWorkspace is a scanned workspace whose scanner-seeded required egress rows name a
// private literal and a plain name.
func scanSeededPrivateWorkspace(src types.WorkspaceSource) types.Workspace {
	return types.Workspace{ID: uuid.New(), Name: "app", Status: types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{src},
		Requirements: map[string]types.WorkspaceRequirement{
			"egress:10.0.0.5": {Level: "required", Provenance: "scan_seeded"},
			"egress:ok.test":  {Level: "required", Provenance: "scan_seeded"},
		}}
}

// The launch fold holds a workspace's requirement rows to the run's ceiling: with the operator-set gate
// off, a scan-seeded private literal the ceiling does not cover stays out of the run's spec, and the
// preflight says which host was dropped and where an admin can allow it.
func TestRunFold_RequirementRowsAreHeldToTheCeiling(t *testing.T) {
	ws := scanSeededPrivateWorkspace(types.WorkspaceSource{Type: types.WorkspaceSourceTypeRepo, Source: "https://github.com/acme/app.git", Target: "/home/agent/app"})
	cookie := ssoSession(t, "member", "m@example.com", oidc.RoleUser)
	body := `{"agent":"claude-code","task":"t","workspace_id":"` + ws.ID.String() + `"}`
	srv := providerRunFixture(t, types.SiteConfig{}, &capStore{}, &ws)
	srv.cfg.RequireOperatorSetEgress = false
	var pv policyPreviewResponse
	w := doSSO(t, srv, http.MethodPost, previewDoor, cookie, body)
	if err := json.Unmarshal(w.Body.Bytes(), &pv); err != nil || w.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", w.Code, w.Body.String())
	}
	if slices.Contains(pv.Spec.AllowedDomains, "10.0.0.5") {
		t.Errorf("preview spec allowed_domains = %v, want the scan-seeded private literal held back", pv.Spec.AllowedDomains)
	}
	w = doSSO(t, srv, http.MethodPost, preflightDoor, cookie, body)
	want := `dropped egress requirement \"10.0.0.5\" of workspace \"app\": a private address not in operator allowlist; allow it in the workspace's approved egress`
	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("preflight = %d %s, want the drop warning %s", w.Code, w.Body.String(), want)
	}
}

// A confined record-run replay is held to the launching operator's ceiling end to end: the policy the
// session dispatches does not carry a scan-seeded private literal back in through the requirement fold.
func TestLaunchRecordRun_ConfinedReplayHoldsRequirementRowsToTheCeiling(t *testing.T) {
	h := newHarness(t)
	ws := scanSeededPrivateWorkspace(types.WorkspaceSource{Type: types.WorkspaceSourceTypeLocalDir, Path: t.TempDir()})
	fake := newRecordLLMModeStore(ws)
	cfg := baseTestConfig(h, fake)
	fr := &fakeRunner{}
	cfg.Runner = fr
	cfg.Broker = h.broker
	cfg.RequireOperatorSetEgress = false
	srv := New(cfg)
	run, _, err := srv.launchRecordRun(context.Background(), "alice@example.com", ws, "build", "build", true)
	if err != nil {
		t.Fatalf("launchRecordRun: %v", err)
	}
	fr.mu.Lock()
	got, calls := fr.lastSpec.ProxyConfig.Policy.AllowedDomains, fr.createCalls
	fr.mu.Unlock()
	if calls != 1 {
		t.Fatalf("CreateSandbox calls = %d (run %s: %s), want the confined session dispatched once", calls, run.State, run.FailureHint)
	}
	if slices.Contains(got, "10.0.0.5") {
		t.Errorf("confined replay allowed_domains = %v, want the scan-seeded private literal held back", got)
	}
}
