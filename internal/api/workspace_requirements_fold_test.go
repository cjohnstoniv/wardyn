// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// This file covers Task 2 (the fold), Task 3 (per-run selections threaded
// through create + preflight) and the Task 4 compose_setup.go escalation —
// applyWorkspaceRequirements is the load-bearing function under test
// throughout (defined in runs_create.go, beside applyWorkspaceCreds).

// the fallback golden: empty Requirements is a byte-identical no-op

// TestApplyWorkspaceRequirements_EmptyRequirementsIsNoOp is the back-compat
// proof for every existing (pre-contract, or simply unconfigured) workspace:
// with NO requirements declared, applyWorkspaceRequirements must not touch the
// resolved spec's grants, AllowedDomains, or any mount's ReadOnly AT ALL — the
// snapshot below is a byte-for-byte JSON comparison, not a field-by-field
// approximation. Covers a local-dir, a repo, and a multi-source workspace.
func TestApplyWorkspaceRequirements_EmptyRequirementsIsNoOp(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name string
		ws   types.Workspace
	}{
		{"local-dir", types.Workspace{ID: uuid.New(), Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/app"},
		}}},
		{"repo", types.Workspace{ID: uuid.New(), Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeRepo, Source: "acme/widgets"},
		}}},
		{"multi-source", types.Workspace{ID: uuid.New(), Sources: []types.WorkspaceSource{
			{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/one", Writable: true},
			{Type: types.WorkspaceSourceTypeRepo, Source: "acme/two"},
			{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/scratch"},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			srv := New(baseTestConfig(h, &workspaceStoreFake{ws: tc.ws}))
			spec := types.RunPolicySpec{MinConfinementClass: types.CC2, AllowedDomains: []string{"api.anthropic.com"}}
			req := createRunRequest{Agent: "claude-code", WorkspaceID: &tc.ws.ID}
			if _, _, code, _, err := srv.seedRequestWorkspace(ctx, &spec, &req); err != nil {
				t.Fatalf("seed: %d %v", code, err)
			}
			wsRefs := srv.referencedWorkspaces(ctx, spec)
			if len(wsRefs) == 0 {
				t.Fatalf("workspace did not resolve via referencedWorkspaces — test setup is broken")
			}
			before, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			events := srv.applyWorkspaceRequirements(ctx, &spec, req.Agent, wsRefs, resolveWorkspaceSelections(req))
			after, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Errorf("empty Requirements must be a byte-identical no-op:\nbefore=%s\nafter =%s", before, after)
			}
			if len(events) != 0 {
				t.Errorf("no audit events expected from an empty requirements contract, got %+v", events)
			}
		})
	}
}

// folding matrix: egress

func TestApplyWorkspaceRequirements_Egress(t *testing.T) {
	srv := New(Config{})
	wsID := uuid.New()
	wsWith := func(level string) []types.Workspace {
		return []types.Workspace{{ID: wsID, Requirements: map[string]types.WorkspaceRequirement{
			"egress:api.stripe.com": {Level: level, Provenance: "operator_set"},
		}}}
	}

	t.Run("required folds in unconditionally", func(t *testing.T) {
		spec := &types.RunPolicySpec{}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("required"), nil)
		if !slices.Contains(spec.AllowedDomains, "api.stripe.com") {
			t.Errorf("AllowedDomains = %v, want api.stripe.com folded in (required)", spec.AllowedDomains)
		}
	})
	t.Run("optional not enabled stays out", func(t *testing.T) {
		spec := &types.RunPolicySpec{}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("optional"), nil)
		if slices.Contains(spec.AllowedDomains, "api.stripe.com") {
			t.Errorf("optional egress must NOT fold in without an explicit per-run enable, got %v", spec.AllowedDomains)
		}
	})
	t.Run("optional enabled via selection folds in", func(t *testing.T) {
		spec := &types.RunPolicySpec{}
		sel := map[string]client.WorkspaceSelection{
			wsID.String(): {WorkspaceID: wsID.String(), EnabledOptional: []string{"egress:api.stripe.com"}},
		}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("optional"), sel)
		if !slices.Contains(spec.AllowedDomains, "api.stripe.com") {
			t.Errorf("enabled optional egress must fold in, got %v", spec.AllowedDomains)
		}
	})
}

// TestApplyWorkspaceRequirements_EgressTrustBoundary:
// RequireOperatorSetEgress applies the SAME provenance gate to a scan_seeded
// egress requirement that TestApplyWorkspaceRequirements_TrustBoundary pins
// for secrets — but only when the flag is set. With the flag unset/false, any
// enabled requirement folds in regardless of provenance (see
// TestApplyWorkspaceRequirements_Egress).
func TestApplyWorkspaceRequirements_EgressTrustBoundary(t *testing.T) {
	wsID := uuid.New()
	wsWith := func(provenance string) []types.Workspace {
		return []types.Workspace{{ID: wsID, Requirements: map[string]types.WorkspaceRequirement{
			"egress:api.stripe.com": {Level: "required", Provenance: provenance},
		}}}
	}

	t.Run("flag off: scan_seeded still folds in (today's behavior, unchanged)", func(t *testing.T) {
		srv := New(Config{})
		spec := &types.RunPolicySpec{}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("scan_seeded"), nil)
		if !slices.Contains(spec.AllowedDomains, "api.stripe.com") {
			t.Errorf("AllowedDomains = %v, want api.stripe.com folded in (gate is off by default)", spec.AllowedDomains)
		}
	})
	t.Run("flag on: scan_seeded is skipped (trust boundary)", func(t *testing.T) {
		srv := New(Config{RequireOperatorSetEgress: true})
		spec := &types.RunPolicySpec{}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("scan_seeded"), nil)
		if slices.Contains(spec.AllowedDomains, "api.stripe.com") {
			t.Errorf("AllowedDomains = %v, want api.stripe.com NOT folded in (scan_seeded, gate on)", spec.AllowedDomains)
		}
	})
	t.Run("flag on: the SAME key as operator_set DOES fold in", func(t *testing.T) {
		srv := New(Config{RequireOperatorSetEgress: true})
		spec := &types.RunPolicySpec{}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("operator_set"), nil)
		if !slices.Contains(spec.AllowedDomains, "api.stripe.com") {
			t.Errorf("AllowedDomains = %v, want api.stripe.com folded in (operator_set, gate on)", spec.AllowedDomains)
		}
	})
}

// folding matrix + trust boundary: secret

// TestApplyWorkspaceRequirements_SecretNeverGrants pins #547: a secret:
// requirement's grant was always scoped to the run's agent's MODEL host, so it
// handed a run a stored secret — the operator's, on a member's run — as its
// model key. Model access is a model provider now: an operator_set secret
// requirement grants nothing and records an audited skip naming the host it
// would have landed on.
func TestApplyWorkspaceRequirements_SecretNeverGrants(t *testing.T) {
	wsID := uuid.New()
	wsWith := func(level string) []types.Workspace {
		return []types.Workspace{{ID: wsID, Requirements: map[string]types.WorkspaceRequirement{
			"secret:acme-key": {Level: level, Provenance: "operator_set"},
		}}}
	}
	srv := New(Config{Secrets: &memSecrets{m: map[string][]byte{"acme-key": []byte("v")}}})
	wantSkip := func(t *testing.T, events []requirementAuditEntry) {
		t.Helper()
		if len(events) != 1 || events[0].action != "run.requirement.skip" || events[0].outcome != "denied" ||
			events[0].target != "acme-key" || events[0].data["reason"] != reasonRequirementModelHost ||
			events[0].data["host"] != "api.anthropic.com" {
			t.Fatalf("events = %+v, want ONE run.requirement.skip (denied, model_host, api.anthropic.com)", events)
		}
	}

	t.Run("required + present grants nothing, opens nothing, and records the skip", func(t *testing.T) {
		spec := &types.RunPolicySpec{}
		events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("required"), nil)
		if len(spec.EligibleGrants) != 0 || len(spec.AllowedDomains) != 0 {
			t.Fatalf("grants=%+v domains=%v, want neither", spec.EligibleGrants, spec.AllowedDomains)
		}
		wantSkip(t, events)
	})
	t.Run("optional enabled is skipped the same way", func(t *testing.T) {
		spec := &types.RunPolicySpec{}
		sel := map[string]client.WorkspaceSelection{
			wsID.String(): {WorkspaceID: wsID.String(), EnabledOptional: []string{"secret:acme-key"}},
		}
		events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("optional"), sel)
		if len(spec.EligibleGrants) != 0 {
			t.Fatalf("grants = %+v, want none", spec.EligibleGrants)
		}
		wantSkip(t, events)
	})
	t.Run("optional not enabled records nothing", func(t *testing.T) {
		spec := &types.RunPolicySpec{}
		if events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("optional"), nil); len(events) != 0 || len(spec.EligibleGrants) != 0 {
			t.Errorf("events=%+v grants=%+v, want neither", events, spec.EligibleGrants)
		}
	})
	t.Run("a non-model agent has nothing to bind to and records nothing", func(t *testing.T) {
		spec := &types.RunPolicySpec{}
		if events := srv.applyWorkspaceRequirements(context.Background(), spec, "some-other-agent", wsWith("required"), nil); len(events) != 0 || len(spec.EligibleGrants) != 0 {
			t.Errorf("events=%+v grants=%+v, want neither", events, spec.EligibleGrants)
		}
	})
}

// TestApplyWorkspaceRequirements_IntegrationSkipsModelHosts: an integration
// requirement's header credential is never injected on a host that serves a
// model — the host opens, the credential is skipped and audited — while its
// other hosts are credentialed as before.
func TestApplyWorkspaceRequirements_IntegrationSkipsModelHosts(t *testing.T) {
	integ := feedIntegration()
	integ.Egress = []string{"artifactory.corp.internal", "api.anthropic.com"}
	srv := runIntegrationSrv(t, []types.Integration{integ}, map[string][]byte{"artifactory-token": []byte("tok")})
	ws := []types.Workspace{{ID: uuid.New(), Requirements: map[string]types.WorkspaceRequirement{
		"integration:" + integ.ID: {Level: "required", Provenance: "operator_set"},
	}}}
	spec := &types.RunPolicySpec{}
	events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", ws, nil)
	if _, ok := apiKeyGrantForHost(spec, "api.anthropic.com"); ok {
		t.Fatalf("the integration's credential was granted on the model host: %+v", spec.EligibleGrants)
	}
	if _, ok := apiKeyGrantForHost(spec, "artifactory.corp.internal"); !ok {
		t.Errorf("the non-model host lost its grant: %+v", spec.EligibleGrants)
	}
	if !slices.Contains(spec.AllowedDomains, "api.anthropic.com") {
		t.Errorf("AllowedDomains = %v, want the model host still opened", spec.AllowedDomains)
	}
	var skipped bool
	for _, ev := range events {
		if ev.action == "run.requirement.skip" && ev.data["reason"] == reasonRequirementModelHost &&
			slices.Equal(ev.data["hosts"].([]string), []string{"api.anthropic.com"}) {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("events = %+v, want a run.requirement.skip naming api.anthropic.com", events)
	}
}

// TestApplyWorkspaceRequirements_TrustBoundary is the explicit pin for the
// security-critical rule: a scan_seeded required secret — derived from
// UNTRUSTED repo content — must NEVER auto-grant, even when the named secret
// is present in the store. Since #547 the identical key as operator_set does
// not grant either; it is recorded as an audited skip.
func TestApplyWorkspaceRequirements_TrustBoundary(t *testing.T) {
	wsID := uuid.New()
	sec := &memSecrets{m: map[string][]byte{"acme-key": []byte("v")}}
	wsWith := func(provenance string) []types.Workspace {
		return []types.Workspace{{ID: wsID, Requirements: map[string]types.WorkspaceRequirement{
			"secret:acme-key": {Level: "required", Provenance: provenance},
		}}}
	}

	srv := New(Config{Secrets: sec})
	t.Run("scan_seeded required secret records nothing at all", func(t *testing.T) {
		spec := &types.RunPolicySpec{}
		events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("scan_seeded"), nil)
		if len(spec.EligibleGrants) != 0 || len(events) != 0 {
			t.Fatalf("scan_seeded must never auto-grant a secret (trust boundary): grants=%+v events=%+v",
				spec.EligibleGrants, events)
		}
	})
	t.Run("the SAME key as operator_set is an audited skip, never a grant", func(t *testing.T) {
		spec := &types.RunPolicySpec{}
		events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("operator_set"), nil)
		if len(spec.EligibleGrants) != 0 || len(events) != 1 || events[0].action != "run.requirement.skip" {
			t.Fatalf("operator_set must be skipped, not granted: grants=%+v events=%+v", spec.EligibleGrants, events)
		}
	})
}

// folding matrix + narrow-only: write

// TestApplyWorkspaceRequirements_WriteNarrowing covers the write:<path> rules
// end to end, including the two explicit narrow-only invariants: a run may
// DROP a Required write (force it read-only) but may never ADD write access
// to an Optional entry it did not enable via enabled_optional.
func TestApplyWorkspaceRequirements_WriteNarrowing(t *testing.T) {
	wsID := uuid.New()
	const path = "/srv/app"
	mkSpec := func() *types.RunPolicySpec {
		ro := true // seedRequestWorkspace's own safe default before any fold runs
		return &types.RunPolicySpec{WorkspaceMounts: []types.WorkspaceMount{{Source: path, Target: "/home/agent/work", ReadOnly: &ro}}}
	}
	wsWith := func(level string) []types.Workspace {
		return []types.Workspace{{ID: wsID, Requirements: map[string]types.WorkspaceRequirement{
			"write:" + path: {Level: level, Provenance: "operator_set"},
		}}}
	}
	mountRO := func(spec *types.RunPolicySpec) bool {
		if spec.WorkspaceMounts[0].ReadOnly == nil {
			t.Fatal("mount ReadOnly must always be resolved to an explicit pointer")
		}
		return *spec.WorkspaceMounts[0].ReadOnly
	}
	srv := New(Config{})

	t.Run("required defaults to read-write", func(t *testing.T) {
		spec := mkSpec()
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("required"), nil)
		if mountRO(spec) {
			t.Error("a Required write must default to read-write")
		}
	})
	t.Run("a run may DROP a Required write via read_only=true", func(t *testing.T) {
		spec := mkSpec()
		narrow := true
		sel := map[string]client.WorkspaceSelection{wsID.String(): {WorkspaceID: wsID.String(), ReadOnly: &narrow}}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("required"), sel)
		if !mountRO(spec) {
			t.Error("read_only=true must NARROW a Required write down to read-only")
		}
	})
	t.Run("optional not enabled defaults to read-only", func(t *testing.T) {
		spec := mkSpec()
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("optional"), nil)
		if !mountRO(spec) {
			t.Error("an Optional write not enabled must stay read-only")
		}
	})
	t.Run("optional enabled becomes read-write", func(t *testing.T) {
		spec := mkSpec()
		sel := map[string]client.WorkspaceSelection{wsID.String(): {WorkspaceID: wsID.String(), EnabledOptional: []string{"write:" + path}}}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("optional"), sel)
		if mountRO(spec) {
			t.Error("an enabled Optional write must become read-write")
		}
	})
	t.Run("a run may NOT ADD an Optional write it never enabled via read_only=false alone", func(t *testing.T) {
		spec := mkSpec()
		widen := false
		sel := map[string]client.WorkspaceSelection{wsID.String(): {WorkspaceID: wsID.String(), ReadOnly: &widen}}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("optional"), sel)
		if !mountRO(spec) {
			t.Error("read_only=false must NOT widen an optional write the run never enabled via enabled_optional")
		}
	})
	t.Run("a path not actually mounted this run is a no-op", func(t *testing.T) {
		spec := &types.RunPolicySpec{} // no mounts at all
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("required"), nil)
		if len(spec.WorkspaceMounts) != 0 {
			t.Errorf("WorkspaceMounts = %+v, want untouched (nothing to narrow)", spec.WorkspaceMounts)
		}
	})
}

// preflight/launch agreement

// TestWorkspaceRequirements_PreflightLaunchAgreement: Review and launch must
// agree about a required operator_set secret requirement — the SAME
// workspace_id is folded through POST /runs/preflight (the real HTTP handler)
// and through the construction sequence handleCreateRun runs before
// persistRunGrants (seedRequestWorkspace -> referencedWorkspaces ->
// resolveWorkspaceSelections -> applyWorkspaceRequirements). Neither may put
// the named secret on the model host (#547): preflight reports no grant row
// for it, and launch mints none and records the skip.
func TestWorkspaceRequirements_PreflightLaunchAgreement(t *testing.T) {
	h, _ := newSecretsHarness(t) // memSecrets seeded with "anthropic-api-key"
	wsID := uuid.New()
	ws := types.Workspace{
		ID:      wsID,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: "/srv/app"}},
		Status:  types.WorkspaceScanned,
		Requirements: map[string]types.WorkspaceRequirement{
			"secret:anthropic-api-key": {Level: "required", Provenance: "operator_set"},
		},
	}
	h.srv.cfg.Store = &workspaceStoreFake{ws: ws}

	body := `{"agent":"claude-code","workspace_id":"` + wsID.String() + `",` +
		`"inline_policy":{"min_confinement_class":"CC2"}}`

	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight: code=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	var pf preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pf); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}
	if it, ok := findItem(pf.SetupItems, "llm_access:claude-code"); ok && it.Status == "satisfied" {
		t.Fatalf("preflight llm_access = %+v, want not satisfied — the requirement no longer grants the model key", it)
	}

	var req createRunRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := *req.InlinePolicy
	if _, _, code, _, err := h.srv.seedRequestWorkspace(ctx, &spec, &req); err != nil {
		t.Fatalf("seed: %d %v", code, err)
	}
	wsRefs := h.srv.referencedWorkspaces(ctx, spec)
	events := h.srv.applyWorkspaceRequirements(ctx, &spec, req.Agent, wsRefs, resolveWorkspaceSelections(req))
	if len(events) != 1 || events[0].action != "run.requirement.skip" {
		t.Fatalf("launch-side fold events = %+v, want ONE run.requirement.skip", events)
	}
	if _, granted := apiKeyGrantForHost(&spec, "api.anthropic.com"); granted {
		t.Fatal("launch-side spec carries an api_key grant on the model host from the requirement")
	}
}

// Task 4: compose_setup.go checklist escalation

// TestSetupWorkspaceSecretItems_ContractRequiredIsNeverBlocking: a secret the
// requirements contract marks Required used to escalate to the blocking-styled
// "secret" kind when absent, because the requirement minted a model-host grant
// from it at launch. It mints nothing now (#547), so storing it changes nothing
// about the run: the row stays the neutral workspace_secret kind either way.
func TestSetupWorkspaceSecretItems_ContractRequiredIsNeverBlocking(t *testing.T) {
	ws := types.Workspace{
		ID:   uuid.New(),
		Name: "acme-app",
		Requirements: map[string]types.WorkspaceRequirement{
			"secret:acme-stripe-key": {Level: "required", Provenance: "operator_set"},
		},
	}
	for _, present := range []bool{false, true} {
		items := setupWorkspaceSecretItems([]types.Workspace{ws}, map[string]bool{"acme-stripe-key": present})
		if _, blocking := findItem(items, "secret:acme-stripe-key"); blocking {
			t.Errorf("present=%v: a blocking secret row appeared: %+v", present, items)
		}
		it, ok := findItem(items, "workspace_secret:acme-stripe-key")
		if !ok || it.Kind != "workspace_secret" {
			t.Fatalf("present=%v: row = %+v (ok=%v), want the neutral workspace_secret row", present, it, ok)
		}
		if want := "workspace acme-app's requirements contract"; it.RequiredBy != want {
			t.Errorf("RequiredBy = %q, want %q", it.RequiredBy, want)
		}
	}
}
