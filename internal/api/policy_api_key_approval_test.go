// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const apiKeyApprovalRefusal = "eligible_grants[0]: api_key cannot require approval"

func gatedAPIKey(t *testing.T, gated bool) types.GrantSpec {
	t.Helper()
	return types.GrantSpec{Kind: types.GrantAPIKey, RequiresApproval: gated, Scope: apiKeyScope(t, "api.example.test", "corp-key")}
}

// An api_key grant marked requires_approval is refused where a person or a file
// authors a policy, as env_secret and file_secret are: every door that injects a
// key drops an approval-gated grant, so no run could ever use it. The same grant
// without the flag is accepted. A policy saved before the rule still loads: the
// run-path validators (strict, lenient, per grant) accept the flag.
func TestAPIKeyRequiresApprovalRefusedAtSave(t *testing.T) {
	for _, gated := range []bool{false, true} {
		g := gatedAPIKey(t, gated)
		spec := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{g}}
		err := validateAuthoredPolicySpec(spec)
		if gated && (err == nil || !strings.Contains(err.Error(), apiKeyApprovalRefusal)) {
			t.Errorf("authored requires_approval=true: err = %v, want the api_key approval refusal", err)
		}
		if !gated && err != nil {
			t.Errorf("authored requires_approval=false: err = %v, want accepted", err)
		}
		for name, err := range map[string]error{
			"validatePolicySpec":        validatePolicySpec(spec),
			"validatePolicySpecLenient": validatePolicySpecLenient(spec),
			"validateEligibleGrantMode": validateEligibleGrantMode(0, g, true),
		} {
			if err != nil {
				t.Errorf("requires_approval=%v, %s: err = %v, want accepted (a saved policy keeps loading)", gated, name, err)
			}
		}
	}
}

// A grant an earlier write already held is left alone by a write that does not
// change it, and refused once the write adds or changes it.
func TestAPIKeyApprovalHeldGrantLeftAloneUntilChanged(t *testing.T) {
	held := gatedAPIKey(t, true)
	restated := held
	restated.Scope = json.RawMessage(`{ "host": "api.example.test", "header": "Authorization", "secret_name": "corp-key" }`)
	if err := authoredAPIKeyApprovalRefusal([]types.GrantSpec{restated}, []types.GrantSpec{held}); err != nil {
		t.Errorf("unchanged grant (scope spelled differently): err = %v, want accepted", err)
	}
	reordered := held
	reordered.Scope = json.RawMessage(`{"secret_name":"corp-key","host":"api.example.test","header":"Authorization"}`)
	if err := authoredAPIKeyApprovalRefusal([]types.GrantSpec{reordered}, []types.GrantSpec{held}); err != nil {
		t.Errorf("unchanged grant (scope keys in another order): err = %v, want accepted", err)
	}
	changed := held
	changed.TTLSeconds = 60
	if err := authoredAPIKeyApprovalRefusal([]types.GrantSpec{changed}, []types.GrantSpec{held}); err == nil {
		t.Error("changed grant: want the refusal, got nil")
	}
	if err := authoredAPIKeyApprovalRefusal([]types.GrantSpec{held}, nil); err == nil {
		t.Error("added grant: want the refusal, got nil")
	}
}

// A launch never refuses a gated api_key grant. A default policy and a member's
// ceiling saved before the rule hold one; a policy_overlay that adds or changes
// the grant still launches, and the key is left out of the run (the grant is
// never on the proxy's list: TestPersistRunGrantsLeavesApprovalGatedKeyOffTheProxy).
func TestPolicyOverlayGatedAPIKeyLaunches(t *testing.T) {
	srv, _ := ovOverlayFixture(t)
	legacy := &srv.cfg.DefaultPolicy.EligibleGrants[0] // shared with the member's ceiling
	legacy.RequiresApproval = true
	body := ovOverlayBody(`{"eligible_grants":[{"kind":"api_key","requires_approval":true,"ttl_seconds":60,` +
		`"scope":{"host":"api.anthropic.com","secret_name":"` + govCorpSecret + `"}}]}`)
	_, created, _ := ovDoorSpecs(t, srv, body)
	for _, g := range created.EligibleGrants {
		if g.Kind == types.GrantAPIKey && !g.RequiresApproval {
			t.Errorf("created grant %+v, want the approval kept on", g)
		}
	}
}

// A ceiling saved before the rule may hold a gated api_key grant. The clamp
// forces approval on a member's grant under it, and the run still launches: the
// key is left out of the run (TestPersistRunGrantsLeavesApprovalGatedKeyOffTheProxy).
func TestAPIKeyClampedToApprovalStillValidates(t *testing.T) {
	ceiling := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{gatedAPIKey(t, true)}}
	proposed := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{gatedAPIKey(t, false)}}
	clamped, _ := composer.Clamp(proposed, ceiling, types.GovernanceLimits{})
	if len(clamped.EligibleGrants) != 1 || !clamped.EligibleGrants[0].RequiresApproval {
		t.Fatalf("clamp = %+v, want the one api_key grant forced to require approval", clamped.EligibleGrants)
	}
	spec := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: clamped.EligibleGrants}
	if err := validatePolicySpec(spec); err != nil {
		t.Errorf("validatePolicySpec(clamped) = %v, want accepted", err)
	}
}

// The default policy file is read at boot: a gated api_key grant in it loads, and
// one warning names the grant, so an upgrade never stops the daemon.
func TestLoadPolicySpecGatedAPIKeyLoadsWithWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	content := `{"min_confinement_class":"CC2","eligible_grants":[` +
		`{"kind":"api_key","requires_approval":true,"scope":{"host":"api.example.test","secret_name":"corp-key"}}]}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	spec, err := LoadPolicySpec(path)
	if err != nil {
		t.Fatalf("LoadPolicySpec: %v", err)
	}
	if len(spec.EligibleGrants) != 1 || !spec.EligibleGrants[0].RequiresApproval {
		t.Errorf("eligible grants = %+v, want the gated api_key grant kept", spec.EligibleGrants)
	}
	out := logs.String()
	if strings.Count(out, "an api_key grant in the policy file requires approval") != 1 || !strings.Contains(out, "eligible_grants[0]") ||
		!strings.Contains(out, "never injected") || !strings.Contains(out, "level=WARN") {
		t.Errorf("log = %q, want one WARN naming eligible_grants[0] and that the key is never injected", out)
	}
}

// A governance overlay write over a base that holds a gated api_key grant (a
// deployment default saved before the rule) leaves the grant alone, even with
// its scope keys in another order, and refuses an overlay that changes it.
func TestCheckOverlayGatedAPIKeyFromTheBase(t *testing.T) {
	srv, _, _ := govEscapeFixture(t, &capStore{})
	srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantAPIKey, RequiresApproval: true, TTLSeconds: 300,
		Scope: apiKeyScope(t, "api.example.test", "corp-key")}}
	overlay := func(g types.GrantSpec) *unitWriteError {
		p := types.GovernanceUnit{Name: "p", Overlay: &types.CeilingOverlay{EligibleGrants: &[]types.GrantSpec{g}}}
		return srv.checkOverlay(p, []types.GovernanceUnit{p})
	}
	unchanged := types.GrantSpec{Kind: types.GrantAPIKey, RequiresApproval: true, TTLSeconds: 300,
		Scope: json.RawMessage(`{"secret_name":"corp-key","host":"api.example.test","header":"Authorization"}`)}
	if ref := overlay(unchanged); ref != nil {
		t.Errorf("unchanged grant, keys reordered: %v, want accepted", ref)
	}
	changed := unchanged
	changed.TTLSeconds = 60
	if ref := overlay(changed); ref == nil || !strings.Contains(ref.Error(), "api_key cannot require approval") {
		t.Errorf("changed grant: %v, want the api_key approval refusal", ref)
	}
}

// A preset save refuses a gated api_key grant in its inline_policy or its
// policy_overlay, and leaves alone one the stored preset already carries.
func TestPresetSaveGatedAPIKey(t *testing.T) {
	f := newMemPresetFixture(t)
	grant := types.GrantSpec{Kind: types.GrantAPIKey, RequiresApproval: true, Scope: apiKeyScope(t, "api.example.test", "corp-key")}
	inline := client.PresetRequest{Request: client.CreateRunRequest{Agent: "claude-code",
		InlinePolicy: &types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{grant}}}}
	overlay := client.PresetRequest{Request: client.CreateRunRequest{Agent: "claude-code",
		PolicyOverlay: json.RawMessage(`{"eligible_grants":[{"kind":"api_key","requires_approval":true,` +
			`"scope":{"host":"api.example.test","secret_name":"corp-key"}}]}`)}}
	for name, req := range map[string]client.PresetRequest{"inline_policy": inline, "policy_overlay": overlay} {
		w := f.put(t, "new-"+name[:6], req)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "api_key cannot require approval") {
			t.Errorf("%s: code = %d, want 400 naming the refusal; body=%s", name, w.Code, w.Body.String())
		}
	}
	raw, err := json.Marshal(inline.Request)
	if err != nil {
		t.Fatal(err)
	}
	f.store.rows = map[string]types.LaunchPreset{"ops": {Name: "ops", Version: 1, Request: raw}}
	if w := f.put(t, "ops", inline); w.Code != http.StatusOK {
		t.Errorf("a save that leaves the stored gated grant alone: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// A template save refuses a gated api_key grant in its inline_policy when the
// save adds or changes it, and leaves alone one the revision it replaces held.
func TestTemplateSaveGatedAPIKey(t *testing.T) {
	owner := testTemplateOwner("person", "alice", "")
	grant := types.GrantSpec{Kind: types.GrantAPIKey, RequiresApproval: true, Scope: apiKeyScope(t, "api.example.test", "corp-key")}
	doc := structDoc(t, `{"inline_policy":{"eligible_grants":[{"kind":"api_key","requires_approval":true,`+
		`"scope":{"secret_name":"corp-key","host":"api.example.test","header":"Authorization"}}]}}`)
	save := func(held []types.GrantSpec) []client.TemplateDiagnostic {
		_, diags := templateForSave(client.TemplateSaveRequest{Name: "t", Document: doc}, owner, types.TemplateKindTemplate, held)
		return diags
	}
	if diags := save(nil); len(diags) == 0 || !strings.Contains(diags[0].Message, "api_key cannot require approval") {
		t.Errorf("a new gated api_key: diagnostics %v, want the refusal", diags)
	}
	if diags := save([]types.GrantSpec{grant}); len(diags) != 0 {
		t.Errorf("the stored revision held it (scope keys reordered): diagnostics %v, want accepted", diagReasons(diags))
	}
}
