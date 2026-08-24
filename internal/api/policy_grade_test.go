// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
)

// TestGradePolicy_Validation pins the same fail-closed contract /policies'
// save path uses: an unknown field 400s via decodeStrict before
// validatePolicySpec ever runs, and an invalid field value 400s naming the
// field (validatePolicySpec's own message, wrapped verbatim).
func TestGradePolicy_Validation(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name, body, wantSubstr string
	}{
		{"invalid json", `{not json`, ""},
		{"unknown field", `{"spec":{"min_confinement_class":"CC2"},"bogus":true}`, ""},
		{"unknown min cc", `{"spec":{"min_confinement_class":"CC9"}}`, "min_confinement_class"},
		{"unknown grant kind", `{"spec":{"min_confinement_class":"CC2","eligible_grants":[{"kind":"weird"}]}}`, "eligible_grants[0]"},
		{"missing min cc", `{"spec":{}}`, "min_confinement_class"},
	}
	for _, c := range cases {
		w := do(t, h.srv, http.MethodPost, "/api/v1/policies/grade", adminToken, c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400; body=%s", c.name, w.Code, w.Body.String())
			continue
		}
		if c.wantSubstr != "" && !strings.Contains(w.Body.String(), c.wantSubstr) {
			t.Errorf("%s: body = %q, want it to name %q", c.name, w.Body.String(), c.wantSubstr)
		}
	}
}

// TestGradePolicy_Safest is the safest-shaped spec (CC3, no grants, no
// egress, auto_stop configured): every emitted item — and there is at least
// one — grades low, so the overall grade is low.
func TestGradePolicy_Safest(t *testing.T) {
	h := newHarness(t)
	body := `{"spec":{"min_confinement_class":"CC3","auto_stop_after_sec":3600}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/policies/grade", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp gradePolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if resp.OverallRisk != "low" {
		t.Errorf("overall_risk = %q, want low; risk_assessment=%+v", resp.OverallRisk, resp.RiskAssessment)
	}
	if len(resp.RiskAssessment) == 0 {
		t.Fatal("expected at least the min_confinement_class item")
	}
	for _, it := range resp.RiskAssessment {
		if it.Level != "low" {
			t.Errorf("safest spec graded a non-low item: %+v", it)
		}
	}
}

// TestGradePolicy_Weakest pairs allow-all egress with a write-capable,
// auto-mint (requires_approval=false) github_token grant — the composer's own
// independently-HIGH triggers — and asserts the overall grade is high.
func TestGradePolicy_Weakest(t *testing.T) {
	h := newHarness(t)
	body := `{"spec":{"min_confinement_class":"CC1","allow_all_egress":true,` +
		`"eligible_grants":[{"kind":"github_token","requires_approval":false,` +
		`"scope":{"repos":["acme/widgets"],"permissions":{"contents":"write"}}}]}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/policies/grade", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp gradePolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, w.Body.String())
	}
	if resp.OverallRisk != "high" {
		t.Errorf("overall_risk = %q, want high; risk_assessment=%+v", resp.OverallRisk, resp.RiskAssessment)
	}
}

// TestGradePolicy_InteractiveHintFlipsNeverReapItem pins the round-1-L3
// contract: Grade's only read of RunInput is Interactive, so the endpoint's
// optional `interactive` hint (default false) changes the never-reap item's
// level and rationale — an interactive-intended policy is not mislabeled just
// because the /policies-panel instance never supplies the hint.
func TestGradePolicy_InteractiveHintFlipsNeverReapItem(t *testing.T) {
	h := newHarness(t)
	const spec = `"spec":{"min_confinement_class":"CC2"}`

	// No hint (default false, the conservative frame): never-reap on a
	// non-interactive-declared run grades HIGH.
	w := do(t, h.srv, http.MethodPost, "/api/v1/policies/grade", adminToken, "{"+spec+"}")
	if w.Code != http.StatusOK {
		t.Fatalf("no-hint: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var noHint gradePolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &noHint); err != nil {
		t.Fatalf("decode no-hint response: %v", err)
	}
	item, ok := findRiskItem(noHint.RiskAssessment, "auto_stop_after_sec")
	if !ok || item.Level != "high" {
		t.Fatalf("no-hint auto_stop_after_sec item = %+v (ok=%v), want a HIGH item", item, ok)
	}
	if strings.Contains(item.Rationale, "interactive") {
		t.Errorf("no-hint rationale must not read as interactive-expected: %q", item.Rationale)
	}

	// interactive:true flips the item to LOW with the interactive-expected
	// rationale.
	w = do(t, h.srv, http.MethodPost, "/api/v1/policies/grade", adminToken, "{"+spec+`,"interactive":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("interactive hint: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var withHint gradePolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &withHint); err != nil {
		t.Fatalf("decode interactive response: %v", err)
	}
	item, ok = findRiskItem(withHint.RiskAssessment, "auto_stop_after_sec")
	if !ok || item.Level != "low" {
		t.Fatalf("interactive-hinted auto_stop_after_sec item = %+v (ok=%v), want a LOW item", item, ok)
	}
	if !strings.Contains(item.Rationale, "interactive") {
		t.Errorf("interactive-hinted rationale must explain the never-reap expectation: %q", item.Rationale)
	}
}

func findRiskItem(items []composer.RiskItem, field string) (composer.RiskItem, bool) {
	for _, it := range items {
		if it.Field == field {
			return it, true
		}
	}
	return composer.RiskItem{}, false
}
