// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// artifactSiteCfgStore is dispatchTestStore with ONE operator egress redirect,
// so the dispatch under test really widens its egress mid-flight
// (substituteArtifactEgress) instead of handing the proxy the policy it was given.
type artifactSiteCfgStore struct{ *dispatchTestStore }

func (artifactSiteCfgStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/npm", Ecosystem: "npm"},
	}}, nil
}

// TestDispatch_AuditsEffectivePolicyEnvelope pins the authorization envelope: the
// run row cannot answer "what was this agent allowed to do?" (agent_runs.policy_id
// has no FK and no spec column, run_policies.spec is overwritten in place, and an
// inline/default policy has no row at all), so the append-only
// run.policy.effective event is the only durable record — and it is worth nothing
// if it snapshots the PRE-widening spec. Counterfactual: emit the event at the end
// of handleCreateRun (or anywhere above dispatch's widening phases) and the corp
// mirror is missing while the dropped public registry is still listed.
func TestDispatch_AuditsEffectivePolicyEnvelope(t *testing.T) {
	fr := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	srv.cfg.Store = artifactSiteCfgStore{st}
	run.Task = "" // no agent exec / completion watcher: this test is about the envelope

	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com", "registry.npmjs.org"},
			MinConfinementClass: types.CC1,
		},
	})

	ev := findAudit(audit.events, run.ID, "run.policy.effective", "success")
	if ev == nil {
		t.Fatalf("dispatch recorded no run.policy.effective envelope; events=%s", auditDump(audit.events, run.ID))
	}
	var got types.RunPolicySpec
	if err := json.Unmarshal(ev.Data, &got); err != nil {
		t.Fatalf("envelope is not a RunPolicySpec: %v (%s)", err, ev.Data)
	}
	enforced := fr.lastSpec.ProxyConfig.Policy.AllowedDomains
	if !reflect.DeepEqual(got.AllowedDomains, enforced) {
		t.Errorf("audited envelope != the policy handed to the proxy:\n audited  = %v\n enforced = %v", got.AllowedDomains, enforced)
	}
	if slices.Contains(got.AllowedDomains, "registry.npmjs.org") || !slices.Contains(got.AllowedDomains, "artifactory.corp:443") {
		t.Errorf("envelope snapshots the PRE-widening policy; want the corp mirror substituted in: %v", got.AllowedDomains)
	}
}

// TestDispatch_AuditsEffectivePolicyEnvelope_RedactsLLMInspectionValues is
// W12-A-2 (secret-leak): the run.policy.effective envelope used to
// mustJSON(policy) the FULL spec straight into the append-only audit log —
// including llm_inspection.workspace_secret_values, contradicting the
// field's own "NEVER logged" doc comment (types.LLMInspectionSpec). The
// envelope must instead carry a redaction placeholder, and the redaction must
// never corrupt what dispatch actually hands the proxy sidecar.
func TestDispatch_AuditsEffectivePolicyEnvelope_RedactsLLMInspectionValues(t *testing.T) {
	fr := &fakeRunner{}
	srv, _, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Task = ""

	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{
			MinConfinementClass: types.CC1,
			LLMInspection: &types.LLMInspectionSpec{
				Mode: "alert", DetectSecrets: true,
				WorkspaceSecretValues: []string{"hunter2-must-never-be-logged"},
			},
		},
	})

	ev := findAudit(audit.events, run.ID, "run.policy.effective", "success")
	if ev == nil {
		t.Fatalf("dispatch recorded no run.policy.effective envelope; events=%s", auditDump(audit.events, run.ID))
	}
	if strings.Contains(string(ev.Data), "hunter2-must-never-be-logged") {
		t.Fatalf("W12-A-2: the audit envelope leaked the llm_inspection secret VALUE verbatim, contradicting its own doc comment: %s", ev.Data)
	}
	var got types.RunPolicySpec
	if err := json.Unmarshal(ev.Data, &got); err != nil {
		t.Fatalf("envelope is not a RunPolicySpec: %v (%s)", err, ev.Data)
	}
	if got.LLMInspection == nil || len(got.LLMInspection.WorkspaceSecretValues) != 1 {
		t.Fatalf("expected workspace_secret_values replaced by a one-element redaction placeholder, got %+v", got.LLMInspection)
	}
	if strings.Contains(got.LLMInspection.WorkspaceSecretValues[0], "hunter2") {
		t.Errorf("the redaction placeholder itself must not echo the value, got %q", got.LLMInspection.WorkspaceSecretValues[0])
	}

	// The PROXY (what dispatch actually enforces with) must still get the
	// REAL value — the audit redaction is a SEPARATE clone, never the
	// dispatched spec itself.
	enforced := fr.lastSpec.ProxyConfig.Policy.LLMInspection
	if enforced == nil || len(enforced.WorkspaceSecretValues) != 1 || enforced.WorkspaceSecretValues[0] != "hunter2-must-never-be-logged" {
		t.Fatalf("the proxy sidecar must still receive the real value (audit redaction must not corrupt dispatch), got %+v", enforced)
	}
}

// TestDispatch_ResolvesLLMInspectionSecretNamesAtDispatch is the structural
// fix underlying W12-A-2/W12-S1-1: workspace_secret_names (what a policy
// actually authors) is resolved against the secret store ONLY at dispatch,
// onto the in-memory copy of the policy the proxy sidecar receives — never
// stored, never read back, never logged. Belt-and-braces: every resolved
// value is also registered with the run's mask registry, and the resolve
// itself is audited by NAME only.
func TestDispatch_ResolvesLLMInspectionSecretNamesAtDispatch(t *testing.T) {
	fr := &fakeRunner{}
	srv, _, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	run.Task = ""
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"prod-db-password": []byte("resolved-secret-value")}}
	srv.cfg.MaskRegistry = secretmask.NewRegistry()

	srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}), dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{
			MinConfinementClass: types.CC1,
			LLMInspection: &types.LLMInspectionSpec{
				Mode: "alert", DetectSecrets: true,
				WorkspaceSecretNames: []string{"prod-db-password"},
			},
		},
	})

	enforced := fr.lastSpec.ProxyConfig.Policy.LLMInspection
	if enforced == nil || len(enforced.WorkspaceSecretValues) != 1 || enforced.WorkspaceSecretValues[0] != "resolved-secret-value" {
		t.Fatalf("expected the resolved value handed to the proxy sidecar, got %+v", enforced)
	}

	// Belt-and-braces: the resolved value is registered in the run's mask
	// registry, so it is scrubbed from PTY capture / recordings / any other
	// audit event's Data, not merely kept out of run.policy.effective.
	found := false
	for _, v := range srv.cfg.MaskRegistry.Snapshot(run.ID) {
		if string(v) == "resolved-secret-value" {
			found = true
		}
	}
	if !found {
		t.Error("the resolved llm_inspection secret value must be registered in the run's mask registry")
	}

	// The audit trail names WHICH secret was resolved, never its value.
	ev := findAudit(audit.events, run.ID, "run.llm_inspection.secrets_resolve", "success")
	if ev == nil {
		t.Fatalf("expected a run.llm_inspection.secrets_resolve audit event; events=%s", auditDump(audit.events, run.ID))
	}
	if strings.Contains(string(ev.Data), "resolved-secret-value") {
		t.Errorf("the secrets_resolve audit event must never carry the value, got %s", ev.Data)
	}
}
