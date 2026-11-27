// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The policy view's secrets_as_value against a real Postgres: the rows placement writes are the rows the view reads
// back, through the run's filtered audit read. Guarded by WARDYN_TEST_PG (throwawayPGPool); skipped cleanly when unset.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// asValuePGRun is a running run on a real database with its policy recorded, and the server that reads it.
func asValuePGRun(t *testing.T, policy types.RunPolicySpec) (*Server, types.AgentRun) {
	t.Helper()
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)
	h := newHarness(t)
	h.srv.cfg.Store = pg
	h.srv.cfg.Audit = store.Recorder{Pool: pool} // the audit table itself, CHECK included
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.EnvSecretPlaceholders = true
	h.srv.router = h.srv.routes()
	now := time.Now().UTC()
	id := uuid.New()
	run, err := pg.CreateRun(t.Context(), types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "sub-as-value", Agent: "claude-code", Task: "as value",
		ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.test/agent-run/" + id.String(), RunnerTarget: "docker",
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	h.srv.recordAudit(t.Context(), h.srv.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.policy.resolve", run.ID.String(), "success",
		mustJSON(effectivePolicyDatum{RunPolicySpec: policy})))
	return h.srv, run
}

func getAsValueView(t *testing.T, srv *Server, runID uuid.UUID) runPolicyResponse {
	t.Helper()
	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+runID.String()+"/policy", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET policy = %d %s", w.Code, w.Body.String())
	}
	var resp runPolicyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return resp
}

// placeOne runs placement for one env_secret grant with value, the way dispatch does, and returns the sandbox's variable.
func placeOne(t *testing.T, srv *Server, run types.AgentRun, policy types.RunPolicySpec, value string) string {
	t.Helper()
	env := map[string]string{"BREAK_TOKEN": value}
	var injections []runner.InjectionGrant
	srv.placeEnvSecrets(t.Context(), run, policy, dispatchParams{}, nil, []string{"BREAK_TOKEN"}, env, &injections)
	return env["BREAK_TOKEN"]
}

func asValuePolicy(t *testing.T) types.RunPolicySpec {
	t.Helper()
	return types.RunPolicySpec{AllowedDomains: []string{placeholderHost}, EligibleGrants: []types.GrantSpec{hostScoped(t, envSecretGrant("BREAK_TOKEN", "break-token"))}}
}

func TestRunPolicy_SecretsAsValue_PG(t *testing.T) {
	policy := asValuePolicy(t)
	srv, run := asValuePGRun(t, policy)
	if got := placeOne(t, srv, run, policy, "two-line\nvalue-0001"); got != "two-line\nvalue-0001" {
		t.Fatalf("the value was changed: %q", got)
	}
	resp := getAsValueView(t, srv, run.ID)
	if len(resp.SecretsAsValue) != 1 {
		t.Fatalf("secrets_as_value = %+v, want one entry", resp.SecretsAsValue)
	}
	e := resp.SecretsAsValue[0]
	if e.Cause != placeholderCauseMultiline || e.Kind != "env_secret" || e.SecretName != "break-token" || e.Host != placeholderHost ||
		e.Reason != placeholderCauseSentence[placeholderCauseMultiline] || resp.SecretsAsValueTruncated {
		t.Errorf("entry %+v (truncated %v), want the multi-line cause on %s", e, resp.SecretsAsValueTruncated, placeholderHost)
	}
}

// The positive control: a one-line value on an exact host is placed, and its success row is no entry. It fails if the
// view lists every row instead of the failures.
func TestRunPolicy_NoSecretsAsValueWhenPlaced_PG(t *testing.T) {
	policy := asValuePolicy(t)
	srv, run := asValuePGRun(t, policy)
	if got := placeOne(t, srv, run, policy, "one-line-value-0001"); !strings.HasPrefix(got, secretPlaceholderPrefix) {
		t.Fatalf("the value was not placed: %q", got)
	}
	if resp := getAsValueView(t, srv, run.ID); len(resp.SecretsAsValue) != 0 || resp.SecretsAsValueTruncated {
		t.Errorf("secrets_as_value = %+v, want none for a placed secret", resp.SecretsAsValue)
	}
}

func TestRunPolicy_SecretsAsValueTruncatedAt50_PG(t *testing.T) {
	for _, tc := range []struct {
		n         int
		truncated bool
	}{{50, false}, {51, true}} {
		t.Run(fmt.Sprintf("%d rows", tc.n), func(t *testing.T) {
			srv, run := asValuePGRun(t, types.RunPolicySpec{AllowedDomains: []string{placeholderHost}})
			for i := 0; i < tc.n; i++ {
				srv.recordPlaceholder(t.Context(), run, types.GrantEnvSecret, fmt.Sprintf("secret-%02d", i), placeholderHost, placeholderCauseNotAllowed, false)
			}
			resp := getAsValueView(t, srv, run.ID)
			if len(resp.SecretsAsValue) != 50 || resp.SecretsAsValueTruncated != tc.truncated {
				t.Errorf("%d rows: %d entries, truncated %v; want 50 and %v", tc.n, len(resp.SecretsAsValue), resp.SecretsAsValueTruncated, tc.truncated)
			}
		})
	}
}
