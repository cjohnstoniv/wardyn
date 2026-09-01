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

// ─── the fallback golden: empty Requirements is a byte-identical no-op ──────

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
			if _, _, code, err := srv.seedRequestWorkspace(ctx, &spec, &req); err != nil {
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

// ─── folding matrix: egress ──────────────────────────────────────────────────

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

// TestApplyWorkspaceRequirements_EgressTrustBoundary is the #12 regression:
// RequireOperatorSetEgress applies the SAME provenance gate to a scan_seeded
// egress requirement that TestApplyWorkspaceRequirements_TrustBoundary pins
// for secrets — but ONLY when the flag is set. Default (flag unset/false) is
// the pre-existing behavior: any enabled requirement folds in regardless of
// provenance (see TestApplyWorkspaceRequirements_Egress).
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

// ─── folding matrix + trust boundary: secret ────────────────────────────────

func TestApplyWorkspaceRequirements_Secret(t *testing.T) {
	wsID := uuid.New()
	wsWith := func(level string) []types.Workspace {
		return []types.Workspace{{ID: wsID, Requirements: map[string]types.WorkspaceRequirement{
			"secret:acme-key": {Level: level, Provenance: "operator_set"},
		}}}
	}
	present := func() *memSecrets { return &memSecrets{m: map[string][]byte{"acme-key": []byte("v")}} }
	absent := func() *memSecrets { return &memSecrets{} }

	t.Run("required + present mints the coupled grant", func(t *testing.T) {
		srv := New(Config{Secrets: present()})
		spec := &types.RunPolicySpec{}
		events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("required"), nil)
		if len(spec.EligibleGrants) != 1 || spec.EligibleGrants[0].Kind != types.GrantAPIKey {
			t.Fatalf("EligibleGrants = %+v, want exactly one api_key grant", spec.EligibleGrants)
		}
		var scope struct {
			Host       string `json:"host"`
			SecretName string `json:"secret_name"`
		}
		if err := json.Unmarshal(spec.EligibleGrants[0].Scope, &scope); err != nil {
			t.Fatal(err)
		}
		if scope.Host != "api.anthropic.com" || scope.SecretName != "acme-key" {
			t.Errorf("grant scope = %+v, want host=api.anthropic.com secret_name=acme-key", scope)
		}
		if !slices.Contains(spec.AllowedDomains, "api.anthropic.com") {
			t.Errorf("AllowedDomains = %v, want the coupled exact host", spec.AllowedDomains)
		}
		if len(events) != 1 || events[0].action != "run.workspace.requirement.secret" {
			t.Errorf("events = %+v, want ONE dedicated secret-grant audit entry", events)
		}
	})
	t.Run("required + absent secret never grants (degrades, does not brick)", func(t *testing.T) {
		srv := New(Config{Secrets: absent()})
		spec := &types.RunPolicySpec{}
		events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("required"), nil)
		if len(spec.EligibleGrants) != 0 || len(events) != 0 {
			t.Errorf("EligibleGrants=%+v events=%+v, want neither (absent secret must not auto-mint)", spec.EligibleGrants, events)
		}
	})
	t.Run("optional not enabled never grants even when present", func(t *testing.T) {
		srv := New(Config{Secrets: present()})
		spec := &types.RunPolicySpec{}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("optional"), nil)
		if len(spec.EligibleGrants) != 0 {
			t.Errorf("EligibleGrants = %+v, want none (optional not enabled)", spec.EligibleGrants)
		}
	})
	t.Run("optional enabled + present grants", func(t *testing.T) {
		srv := New(Config{Secrets: present()})
		spec := &types.RunPolicySpec{}
		sel := map[string]client.WorkspaceSelection{
			wsID.String(): {WorkspaceID: wsID.String(), EnabledOptional: []string{"secret:acme-key"}},
		}
		srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("optional"), sel)
		if len(spec.EligibleGrants) != 1 {
			t.Errorf("EligibleGrants = %+v, want one (optional enabled)", spec.EligibleGrants)
		}
	})
	t.Run("never double-grants a host an existing grant already covers", func(t *testing.T) {
		srv := New(Config{Secrets: present()})
		existing, _ := json.Marshal(map[string]string{"host": "api.anthropic.com", "secret_name": "other-key"})
		spec := &types.RunPolicySpec{EligibleGrants: []types.GrantSpec{{Kind: types.GrantAPIKey, Scope: existing}}}
		events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("required"), nil)
		if len(spec.EligibleGrants) != 1 || len(events) != 0 {
			t.Errorf("EligibleGrants=%+v events=%+v, want the pre-existing grant left alone and untouched", spec.EligibleGrants, events)
		}
	})
	t.Run("non-LLM agent has nothing to bind to", func(t *testing.T) {
		srv := New(Config{Secrets: present()})
		spec := &types.RunPolicySpec{}
		events := srv.applyWorkspaceRequirements(context.Background(), spec, "some-other-agent", wsWith("required"), nil)
		if len(spec.EligibleGrants) != 0 || len(events) != 0 {
			t.Errorf("EligibleGrants=%+v events=%+v, want neither (no LLM provider convention for this agent)", spec.EligibleGrants, events)
		}
	})
}

// TestApplyWorkspaceRequirements_TrustBoundary is the explicit pin for the
// security-critical rule: a scan_seeded required secret — derived from
// UNTRUSTED repo content — must NEVER auto-grant, even when the named secret
// is present in the store; the identical key as operator_set DOES.
func TestApplyWorkspaceRequirements_TrustBoundary(t *testing.T) {
	wsID := uuid.New()
	sec := &memSecrets{m: map[string][]byte{"acme-key": []byte("v")}}
	wsWith := func(provenance string) []types.Workspace {
		return []types.Workspace{{ID: wsID, Requirements: map[string]types.WorkspaceRequirement{
			"secret:acme-key": {Level: "required", Provenance: provenance},
		}}}
	}

	srv := New(Config{Secrets: sec})
	t.Run("scan_seeded required secret does NOT auto-grant", func(t *testing.T) {
		spec := &types.RunPolicySpec{}
		events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("scan_seeded"), nil)
		if len(spec.EligibleGrants) != 0 || len(events) != 0 {
			t.Fatalf("scan_seeded must NEVER auto-grant a secret (trust boundary): grants=%+v events=%+v",
				spec.EligibleGrants, events)
		}
	})
	t.Run("the SAME key as operator_set DOES auto-grant", func(t *testing.T) {
		spec := &types.RunPolicySpec{}
		events := srv.applyWorkspaceRequirements(context.Background(), spec, "claude-code", wsWith("operator_set"), nil)
		if len(spec.EligibleGrants) != 1 || len(events) != 1 {
			t.Fatalf("operator_set must auto-grant: grants=%+v events=%+v", spec.EligibleGrants, events)
		}
	})
}

// ─── folding matrix + narrow-only: write ────────────────────────────────────

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

// ─── preflight/launch agreement ──────────────────────────────────────────────

// TestWorkspaceRequirements_PreflightLaunchAgreement proves preflight cannot
// predict a rosier (or stricter) outcome than launch: the SAME workspace_id +
// required+operator_set secret requirement is folded through
// POST /runs/preflight (the real HTTP handler) and through the identical
// construction sequence handleCreateRun runs before persistRunGrants
// (seedRequestWorkspace -> referencedWorkspaces -> resolveWorkspaceSelections
// -> applyWorkspaceRequirements) — both must agree the secret grant was
// minted.
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

	// Preflight side: the real HTTP handler.
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("preflight: code=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	var pf preflightResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pf); err != nil {
		t.Fatalf("decode preflight: %v", err)
	}
	it, ok := findItem(pf.SetupItems, "secret:anthropic-api-key")
	if !ok || it.Status != "satisfied" {
		t.Fatalf("preflight secret row = %+v (ok=%v), want satisfied (the fold must have minted the grant)", it, ok)
	}

	// Launch side: the identical construction sequence handleCreateRun runs
	// before persistRunGrants.
	var req createRunRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := *req.InlinePolicy
	if _, _, code, err := h.srv.seedRequestWorkspace(ctx, &spec, &req); err != nil {
		t.Fatalf("seed: %d %v", code, err)
	}
	wsRefs := h.srv.referencedWorkspaces(ctx, spec)
	events := h.srv.applyWorkspaceRequirements(ctx, &spec, req.Agent, wsRefs, resolveWorkspaceSelections(req))
	if len(events) != 1 || events[0].action != "run.workspace.requirement.secret" {
		t.Fatalf("launch-side fold events = %+v, want ONE secret-grant entry — must AGREE with preflight's satisfied verdict", events)
	}
	if _, granted := apiKeyGrantForHost(&spec, "api.anthropic.com"); !granted {
		t.Fatal("launch-side spec must carry the SAME api_key grant preflight's checklist reported satisfied")
	}
}

// ─── Task 4: compose_setup.go checklist escalation ──────────────────────────

// TestSetupWorkspaceSecretItems_ContractRequiredAbsentEscalatesToBlockingKind
// pins the specific fix: a secret the requirements contract marks Required
// that is ALSO absent from the store must render with Kind "secret" (the
// blocking-styled kind compose-review.tsx reserves for llm_access|secret when
// status=="missing"), not the neutral advisory "workspace_secret" kind — the
// row's KIND changes, not just its status. Once the secret is present, no
// escalation is needed (and no duplicate row appears).
func TestSetupWorkspaceSecretItems_ContractRequiredAbsentEscalatesToBlockingKind(t *testing.T) {
	ws := types.Workspace{
		ID:   uuid.New(),
		Name: "acme-app",
		Requirements: map[string]types.WorkspaceRequirement{
			"secret:acme-stripe-key": {Level: "required", Provenance: "operator_set"},
		},
	}

	t.Run("absent escalates to the blocking-styled secret kind", func(t *testing.T) {
		items := setupWorkspaceSecretItems([]types.Workspace{ws}, map[string]bool{})
		it, ok := findItem(items, "secret:acme-stripe-key")
		if !ok {
			t.Fatalf("want an escalated row at id secret:acme-stripe-key, got %+v", items)
		}
		if it.Kind != "secret" {
			t.Errorf("Kind = %q, want %q (must match the review panel's destructive llm_access|secret gate)", it.Kind, "secret")
		}
		if it.Status != "missing" {
			t.Errorf("Status = %q, want missing", it.Status)
		}
		if it.Fix == nil || it.Fix.Action != "add_secret" || it.Fix.SecretName != "acme-stripe-key" {
			t.Errorf("Fix = %+v, want add_secret(acme-stripe-key)", it.Fix)
		}
		if it.Detail == "" {
			t.Error("Detail must explain the gap (honest copy: the run still launches; whatever needs the secret fails then)")
		}
		if _, dup := findItem(items, "workspace_secret:acme-stripe-key"); dup {
			t.Error("the escalated row must REPLACE the neutral workspace_secret row for this name, not duplicate it")
		}
		// RequiredBy must be a plain noun phrase: compose-review.tsx renders
		// "Required by " + this value, so a value that ALSO starts with "required
		// by" doubles up ("Required by required by workspace X's requirements
		// contract", observed live — see reconcile-workspace-first.md item 3).
		if want := "workspace acme-app's requirements contract"; it.RequiredBy != want {
			t.Errorf("RequiredBy = %q, want %q", it.RequiredBy, want)
		}
	})

	t.Run("present needs no escalation and no duplicate row", func(t *testing.T) {
		items := setupWorkspaceSecretItems([]types.Workspace{ws}, map[string]bool{"acme-stripe-key": true})
		it, ok := findItem(items, "workspace_secret:acme-stripe-key")
		if !ok || it.Kind != "workspace_secret" || it.Status != "satisfied" {
			t.Errorf("present contract secret row = %+v (ok=%v), want workspace_secret/satisfied", it, ok)
		}
		if _, dup := findItem(items, "secret:acme-stripe-key"); dup {
			t.Error("a PRESENT contract secret must not ALSO render at the escalated secret: id")
		}
		// Same non-doubling pin as the absent case above: presence changes
		// Kind/Status only, never RequiredBy's wording.
		if want := "workspace acme-app's requirements contract"; it.RequiredBy != want {
			t.Errorf("RequiredBy = %q, want %q", it.RequiredBy, want)
		}
	})

	t.Run("a scan-only (non-contract) required secret is unaffected", func(t *testing.T) {
		// Regression guard: a workspace with NO requirements contract entry for
		// this name must keep today's neutral, non-blocking behavior exactly.
		scanOnly := types.Workspace{ID: uuid.New(), Name: "legacy-app"}
		items := setupWorkspaceSecretItems([]types.Workspace{scanOnly}, map[string]bool{})
		if len(items) != 0 {
			t.Errorf("a workspace with no scanned profile and no requirements contract must add no rows, got %+v", items)
		}
	})
}
