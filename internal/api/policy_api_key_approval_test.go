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
	changed := held
	changed.TTLSeconds = 60
	if err := authoredAPIKeyApprovalRefusal([]types.GrantSpec{changed}, []types.GrantSpec{held}); err == nil {
		t.Error("changed grant: want the refusal, got nil")
	}
	if err := authoredAPIKeyApprovalRefusal([]types.GrantSpec{held}, nil); err == nil {
		t.Error("added grant: want the refusal, got nil")
	}
}

// An inline_policy is authored at launch: a gated api_key grant in it is a 400.
func TestCreateRun_InlineGatedAPIKeyRefused(t *testing.T) {
	h := newHarness(t)
	body := `{"agent":"claude-code","repo":"acme/widgets","inline_policy":{"min_confinement_class":"CC2",` +
		`"eligible_grants":[{"kind":"api_key","requires_approval":true,"scope":{"host":"api.example.test","secret_name":"corp-key"}}]}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken, body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "api_key cannot require approval") {
		t.Fatalf("inline gated api_key: code = %d, want 400 naming the refusal; body=%s", w.Code, w.Body.String())
	}
}

// A policy_overlay over a stored or default base that already carries a gated
// api_key grant is not refused for it (the key stays out of every run, as it
// always did), unless the overlay's own eligible_grants add or change one.
func TestPolicyOverlayGatedAPIKeyOnlyRefusedWhenAuthored(t *testing.T) {
	base := types.RunPolicySpec{MinConfinementClass: types.CC2, EligibleGrants: []types.GrantSpec{gatedAPIKey(t, true)}}
	merged, err := templatePolicyOverlay(base, json.RawMessage(`{"allowed_domains":["api.example.test"]}`))
	if err != nil {
		t.Fatalf("templatePolicyOverlay: %v", err)
	}
	if err := validatePolicySpec(merged); err != nil {
		t.Errorf("overlay that leaves the legacy grant alone: validatePolicySpec = %v, want accepted", err)
	}
	other := `{"eligible_grants":[{"kind":"api_key","requires_approval":true,"scope":{"host":"other.example.test","secret_name":"corp-key"}}]}`
	merged, err = templatePolicyOverlay(base, json.RawMessage(other))
	if err != nil {
		t.Fatalf("templatePolicyOverlay: %v", err)
	}
	if err := authoredAPIKeyApprovalRefusal(merged.EligibleGrants, base.EligibleGrants); err == nil {
		t.Error("overlay that adds a gated api_key: want the refusal, got nil")
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
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "eligible_grants[0]") || !strings.Contains(out, "never injected") {
		t.Errorf("log = %q, want one WARN naming eligible_grants[0] and that the key is never injected", out)
	}
}
