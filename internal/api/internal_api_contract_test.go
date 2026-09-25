// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// internalAPIContract is testdata/internal_api_v0.7.json: the internal API as
// the previous minor's proxy calls it.
type internalAPIContract struct {
	Release string `json:"release"`
	Routes  []struct {
		Method string `json:"method"`
		Path   string `json:"path"`
	} `json:"routes"`
	RenewResponseKeys    []string          `json:"renew_response_keys"`
	Decision             json.RawMessage   `json:"decision"`
	ApprovalRaises       []json.RawMessage `json:"approval_raises"`
	ApprovalResponseKeys []string          `json:"approval_response_keys"`
	MintBody             json.RawMessage   `json:"mint_body"`
}

// TestInternalAPI_ServesTheN1Proxy pins the upgrade support window (long-holds
// design rev 4 §4 row 5, RL-10): wardynd N keeps serving a proxy of N-1, which
// is what a run started before an upgrade still has until it is restarted.
// Every route that proxy dials must still be routed behind the run-token gate,
// every body it sends must still be accepted and read, and every response key
// it reads must still be written. A change here strands every live run on the
// previous release, so it needs the version window moved, not the fixture.
func TestInternalAPI_ServesTheN1Proxy(t *testing.T) {
	raw, err := os.ReadFile("testdata/internal_api_v0.7.json")
	if err != nil {
		t.Fatal(err)
	}
	var c internalAPIContract
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	srv, ast, _, _ := newAuthzMatrixServer(t)
	runID := uuid.New()
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, CreatedBy: "owner@example.com", State: types.RunRunning, Agent: "claude-code"}
	ast.mu.Unlock()
	id, err := srv.cfg.Identity.MintRunIdentity(context.Background(), runID, "owner@example.com", "", internalAudience)
	if err != nil {
		t.Fatal(err)
	}
	tok := id.Token
	fill := strings.NewReplacer("{id}", uuid.NewString(), "{run}", runID.String())

	for _, rt := range c.Routes {
		path := fill.Replace(rt.Path)
		if w := do(t, srv, rt.Method, path, "", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s with no run token: code %d, want 401 — the %s proxy's route is gone or ungated",
				rt.Method, rt.Path, w.Code, c.Release)
		}
	}

	w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", tok, "")
	var renew map[string]any
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &renew) != nil {
		t.Fatalf("renew: code %d body %s", w.Code, w.Body.String())
	}
	for _, k := range c.RenewResponseKeys {
		if _, ok := renew[k]; !ok {
			t.Errorf("renew response lacks %q, which the %s proxy reads", k, c.Release)
		}
	}
	if exp, _ := renew["expires_at"].(string); exp != "" {
		if _, err := time.Parse(time.RFC3339, exp); err != nil {
			t.Errorf("renew expires_at %q is not RFC3339, which the %s proxy parses", exp, c.Release)
		}
	}

	audit := srv.cfg.Audit.(*recRecorder)
	before := len(audit.events)
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, string(c.Decision)); w.Code >= 300 {
		t.Fatalf("decision from the %s proxy: code %d body %s", c.Release, w.Code, w.Body.String())
	}
	var row map[string]any
	for _, ev := range audit.events[before:] {
		if ev.Action == "egress.allow" {
			_ = json.Unmarshal(ev.Data, &row)
		}
	}
	if row["host"] != "registry.npmjs.org" || row["port"] != float64(443) || row["method"] != "CONNECT" || row["rule_source"] != "policy" {
		t.Errorf("egress.allow data = %v; want the %s proxy's decision fields read into the audit row", row, c.Release)
	}

	for _, body := range c.ApprovalRaises {
		w := do(t, srv, http.MethodPost, "/api/v1/internal/approvals", tok, string(body))
		var ar map[string]any
		if (w.Code != http.StatusCreated && w.Code != http.StatusOK) || json.Unmarshal(w.Body.Bytes(), &ar) != nil {
			t.Errorf("approval raise %s: code %d body %s; the %s proxy wants 200/201 and an approval", body, w.Code, w.Body.String(), c.Release)
			continue
		}
		approvalID, _ := ar["id"].(string)
		if approvalID == "" || approvalID == uuid.Nil.String() {
			t.Errorf("approval raise %s answered no id, which the %s proxy polls by", body, c.Release)
			continue
		}
		if w := do(t, srv, http.MethodGet, "/api/v1/internal/approvals/"+approvalID, tok, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"state"`) {
			t.Errorf("approval poll: code %d body %s; the %s proxy reads its state", w.Code, w.Body.String(), c.Release)
		}
	}
	now := time.Now()
	decided, _ := json.Marshal(types.ApprovalRequest{ID: uuid.New(), State: types.ApprovalApproved,
		DecisionScope: types.ApprovalScope("until"), DecisionExpiresAt: &now})
	for _, k := range c.ApprovalResponseKeys {
		if !strings.Contains(string(decided), `"`+k+`"`) {
			t.Errorf("a decided approval does not carry %q, which the %s proxy reads", k, c.Release)
		}
	}

	if w := do(t, srv, http.MethodPost, "/api/v1/internal/credentials/mint", tok, string(c.MintBody)); w.Code == http.StatusBadRequest || w.Code == http.StatusUnauthorized {
		t.Errorf("mint body from the %s proxy: code %d body %s; want it read (an unknown grant is not a bad request)", c.Release, w.Code, w.Body.String())
	}
}
