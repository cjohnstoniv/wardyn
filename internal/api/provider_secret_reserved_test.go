// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// providerSecretNames is every per-person model-provider credential name an
// env_secret or llm_inspection grant must never read: the three suffixes that
// exist, plus one that does not yet, since the reservation is by prefix (#1035).
var providerSecretNames = []string{
	providerSecretPrefix + "0c1f7a2e-key",
	providerSecretPrefix + "0c1f7a2e-oauth",
	providerSecretPrefix + "0c1f7a2e-sso",
	providerSecretPrefix + "0c1f7a2e-future",
}

func envSecretInline(secretName string) string {
	return `"min_confinement_class":"CC1","eligible_grants":[{"kind":"env_secret","scope":{"name":"CORP_TOKEN","secret_name":"` + secretName + `"}}]`
}

func llmInspectionInline(secretName string) string {
	return `"min_confinement_class":"CC1","llm_inspection":{"mode":"alert","detect_secrets":true,"workspace_secret_names":["` + secretName + `"]}`
}

// TestProviderSecretNames_RefusedAtWrite: each suffix, both lanes, both doors a
// run policy is written through — create (POST /runs) and Review (POST
// /runs/preflight) — and an ordinary name still passes both.
func TestProviderSecretNames_RefusedAtWrite(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Store = createRunUnconfiguredStore{}
	h.srv.router = h.srv.routes()

	lanes := map[string]func(string) string{"env_secret": envSecretInline, "llm_inspection": llmInspectionInline}
	doors := map[string]string{"create": "/api/v1/runs", "review": "/api/v1/runs/preflight"}
	body := func(inline string) string {
		return `{"agent":"claude-code","repo":"ephemeral","task":"echo hi","task_mode":"exec","inline_policy":{` + inline + `}}`
	}
	for lane, inline := range lanes {
		for door, path := range doors {
			for _, name := range providerSecretNames {
				w := do(t, h.srv, http.MethodPost, path, adminToken, body(inline(name)))
				if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), name) {
					t.Errorf("%s via %s naming %q: code=%d body=%s, want a 400 naming it", lane, door, name, w.Code, w.Body.String())
				}
			}
			// The ordinary name gets past validation: Review answers 200, and
			// create reaches CreateRun (createRunUnconfiguredStore's 500 sentinel).
			w := do(t, h.srv, http.MethodPost, path, adminToken, body(inline("corp-api-token")))
			want := map[string]int{"create": http.StatusInternalServerError, "review": http.StatusOK}[door]
			if w.Code != want {
				t.Errorf("%s via %s naming an ordinary secret: code=%d body=%s, want %d", lane, door, w.Code, w.Body.String(), want)
			}
		}
	}
}

// TestProviderSecretNames_RefusedAtDispatch is the defence-in-depth half: a
// spec that never met the write-time validators (a stored row written before
// it, or WARDYN_DEFAULT_POLICY) still resolves no provider name — neither the
// member's own row nor the operator's, which the owner-then-operator read
// would otherwise fall back to.
func TestProviderSecretNames_RefusedAtDispatch(t *testing.T) {
	const member = "sub-member@corp.example"
	sec := &memSecrets{m: map[string][]byte{"corp-api-token": []byte("ordinary-value-0123456789")}, owned: map[string]map[string][]byte{}}
	// The member holds only their own -key row; every other name falls back to
	// the operator's row, which is the read this reservation must also stop.
	for _, name := range providerSecretNames {
		sec.m[name] = []byte("OPERATOR-MODEL-KEY-" + name)
	}
	if err := sec.For(member).Put(context.Background(), providerSecretNames[0], []byte("MEMBER-MODEL-KEY")); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.srv.cfg.Secrets = sec
	run := types.AgentRun{ID: uuid.New(), CreatedBy: member}

	var grants []types.GrantSpec
	for i, name := range providerSecretNames {
		grants = append(grants, envSecretGrant("PROVIDER_"+string(rune('A'+i)), name))
	}
	grants = append(grants, envSecretGrant("CORP_TOKEN", "corp-api-token"))
	env := map[string]string{}
	resolved := h.srv.resolveEnvSecretGrants(context.Background(), run, types.RunPolicySpec{EligibleGrants: grants}, env)
	if len(resolved) != 1 || resolved[0] != "CORP_TOKEN" || len(env) != 1 || env["CORP_TOKEN"] != "ordinary-value-0123456789" {
		t.Fatalf("env_secret resolved %v into env %v, want only CORP_TOKEN", resolved, env)
	}
	refused := 0
	for _, ev := range h.audit.events {
		if ev.Action == "run.env_secret.resolve" && ev.Outcome == "failure" &&
			strings.Contains(string(ev.Data), "reserved platform-internal secret name") {
			refused++
		}
	}
	if refused != len(providerSecretNames) {
		t.Errorf("audited reserved-name refusals = %d, want %d", refused, len(providerSecretNames))
	}

	spec := &types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{
		Mode: "alert", DetectSecrets: true, WorkspaceSecretNames: append(append([]string(nil), providerSecretNames...), "corp-api-token"),
	}}
	h.srv.resolveLLMInspectionSecrets(context.Background(), run, spec)
	if got := spec.LLMInspection.WorkspaceSecretValues; len(got) != 1 || got[0] != "ordinary-value-0123456789" {
		t.Fatalf("llm_inspection resolved %v, want only the ordinary secret's value", got)
	}
}
