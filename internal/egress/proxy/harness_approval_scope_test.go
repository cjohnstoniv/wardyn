// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestToolApproval_ScopeNamesTheAskingAgent: a held call's approval names the agent whose
// proxy-issued credential it carried, never one the body claims; a call with no credential,
// and a run without per-agent credentials, name none, so their scope is what it was.
func TestToolApproval_ScopeNamesTheAskingAgent(t *testing.T) {
	var scopes []map[string]any
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RequestedScope map[string]any `json:"requested_scope"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		scopes = append(scopes, body.RequestedScope)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: uuid.New(), RunID: uuid.New(), Kind: types.ApprovalToolCall, State: types.ApprovalPending})
	}))
	t.Cleanup(cp.Close)
	p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)
	p.harnessRules = []types.HarnessToolRules{{Harness: "claude-code"}, {Harness: "codex-cli"}}
	p.harnessCreds = []types.HarnessCredential{
		{Harness: "claude-code", SHA256: credDigest(claudeCred)},
		{Harness: "codex-cli", SHA256: credDigest(codexCred)},
	}
	for _, tc := range []struct {
		name, bearer, extra string
		want                any
	}{
		{"its own credential", codexCred, "", "codex-cli"},
		{"a body claiming another agent", codexCred, `,"harness":"claude-code"`, "codex-cli"},
		{"no credential", "", `,"harness":"claude-code"`, nil},
	} {
		scopes = nil
		if rec := raiseTool(t, p, "Other", tc.bearer, tc.extra); rec.Code != http.StatusCreated || len(scopes) != 1 {
			t.Fatalf("%s: %d %s, want one held approval", tc.name, rec.Code, rec.Body.String())
		}
		if got := scopes[0]["harness"]; got != tc.want {
			t.Errorf("%s: requested_scope.harness = %v, want %v (scope %v)", tc.name, got, tc.want, scopes[0])
		}
	}
	p.harnessRules, p.harnessCreds = nil, nil
	scopes = nil
	if rec := raiseTool(t, p, "Other", codexCred, ""); rec.Code != http.StatusCreated || len(scopes) != 1 {
		t.Fatalf("run-wide regime: %d %s", rec.Code, rec.Body.String())
	}
	if _, named := scopes[0]["harness"]; named {
		t.Errorf("a run without per-agent credentials named an agent: %v", scopes[0])
	}
}
