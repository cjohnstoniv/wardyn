// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
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
	DecisionVariants     []decisionVariant `json:"decision_variants"`
	ApprovalRaises       []json.RawMessage `json:"approval_raises"`
	ApprovalResponseKeys []string          `json:"approval_response_keys"`
	MintBody             json.RawMessage   `json:"mint_body"`
}

// decisionVariant is one entry of decision_variants: a decision log body
// captured verbatim from the N-1 (0.7.12) proxy's own wire vocabulary
// (internal/egress/proxy's llm_routes.go/decisions.go/credhold.go at
// f031df9a7), plus what wardynd's normalizeN1Decision (internal_n1.go) must turn
// it into once it is ingested.
type decisionVariant struct {
	Name   string          `json:"name"`
	Body   json.RawMessage `json:"body"`
	Expect struct {
		AuditAction        string `json:"audit_action"`
		Outcome            string `json:"outcome,omitempty"`
		EgressActionCount  *int   `json:"egress_action_count,omitempty"`
		ScanActionCount    *int   `json:"scan_action_count,omitempty"`
		DeniesDelta        *int   `json:"denies_delta,omitempty"`
		ReauthTimeoutDelta *int   `json:"reauth_timeout_delta,omitempty"`
	} `json:"expect"`
}

// metricCounter reads the integer value of a Prometheus counter line out of a
// /metrics scrape, given the line's exact prefix up to and including the
// trailing space before the value (e.g. "wardyn_egress_denies_total " or
// `wardyn_credential_reauth_total{outcome="timeout"} `).
func metricCounter(t *testing.T, body, prefix string) int {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil {
				t.Fatalf("metric %q: %v", prefix, err)
			}
			return n
		}
	}
	t.Fatalf("metric line %q not found in:\n%s", prefix, body)
	return 0
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

	// decision_variants pin C-03 (#1063): a v0.7.12 proxy's OLD wire values
	// (a scan "error", a "blind" bypass, a "pending" hold, the dotted dropped-
	// decisions rule_source, and the unchanged reauth-timeout one) must land
	// with the SAME meaning they had in 0.7 once normalizeN1Decision runs.
	for _, v := range c.DecisionVariants {
		t.Run("decision_variant/"+v.Name, func(t *testing.T) {
			before := len(audit.events)
			beforeMetrics := do(t, srv, http.MethodGet, "/metrics", adminToken, "").Body.String()

			w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, string(v.Body))
			if w.Code != http.StatusAccepted {
				t.Fatalf("%s proxy variant %s: code %d body %s", c.Release, v.Name, w.Code, w.Body.String())
			}
			got := audit.events[before:]

			if v.Expect.EgressActionCount != nil || v.Expect.ScanActionCount != nil {
				eg, sc := countActions(got)
				if v.Expect.EgressActionCount != nil {
					n := 0
					for _, k := range eg {
						n += k
					}
					if n != *v.Expect.EgressActionCount {
						t.Errorf("%s: egress.* actions = %v (%d total), want %d", v.Name, eg, n, *v.Expect.EgressActionCount)
					}
				}
				if v.Expect.ScanActionCount != nil {
					n := 0
					for _, k := range sc {
						n += k
					}
					if n != *v.Expect.ScanActionCount {
						t.Errorf("%s: llm.scan.* actions = %v (%d total), want %d", v.Name, sc, n, *v.Expect.ScanActionCount)
					}
				}
			}

			if v.Expect.AuditAction != "" {
				var outcome string
				found := false
				for _, ev := range got {
					if ev.Action == v.Expect.AuditAction {
						found, outcome = true, ev.Outcome
					}
				}
				if !found {
					t.Errorf("%s: no %s audit event; got %+v", v.Name, v.Expect.AuditAction, got)
				} else if v.Expect.Outcome != "" && outcome != v.Expect.Outcome {
					t.Errorf("%s: %s outcome = %q, want %q", v.Name, v.Expect.AuditAction, outcome, v.Expect.Outcome)
				}
			}

			if v.Expect.DeniesDelta != nil || v.Expect.ReauthTimeoutDelta != nil {
				afterMetrics := do(t, srv, http.MethodGet, "/metrics", adminToken, "").Body.String()
				if v.Expect.DeniesDelta != nil {
					before := metricCounter(t, beforeMetrics, "wardyn_egress_denies_total ")
					after := metricCounter(t, afterMetrics, "wardyn_egress_denies_total ")
					if after-before != *v.Expect.DeniesDelta {
						t.Errorf("%s: wardyn_egress_denies_total delta = %d, want %d", v.Name, after-before, *v.Expect.DeniesDelta)
					}
				}
				if v.Expect.ReauthTimeoutDelta != nil {
					const reauthTimeoutMetric = `wardyn_credential_reauth_total{outcome="timeout"} `
					before := metricCounter(t, beforeMetrics, reauthTimeoutMetric)
					after := metricCounter(t, afterMetrics, reauthTimeoutMetric)
					if after-before != *v.Expect.ReauthTimeoutDelta {
						t.Errorf("%s: wardyn_credential_reauth_total{outcome=timeout} delta = %d, want %d", v.Name, after-before, *v.Expect.ReauthTimeoutDelta)
					}
				}
			}
		})
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

// TestReviewRev08N1ProxyDecisionSemantics is the independent 0.8 review's own
// reproduction (F11 / issue #1063), adopted verbatim as a regression pin: a
// v0.7.12 proxy's decision bodies, sent as-is to an 0.8 daemon, must keep the
// meaning they carried on the wire in 0.7 once normalizeN1Decision runs.
func TestReviewRev08N1ProxyDecisionSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"scan-error", `{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"deny","rule_source":"scan:error","scan":{"action":"error","mode":"block","coverage":"inspectable"}}`},
		{"opaque-blind", `{"request":{"host":"api.anthropic.com","method":"CONNECT"},"decision":"allow","rule_source":"scan:opaque-tunnel","scan":{"action":"blind","mode":"alert","coverage":"tunneled-opaque"}}`},
		{"dropped-summary", `{"request":{"host":"api.example.com","method":"CONNECT"},"decision":"deny","rule_source":"egress.decisions.dropped:42"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tok := h.mintRunToken(t, uuid.New())
			h.audit.events = nil
			w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", tok, tc.body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("N-1 proxy decision: %d %s", w.Code, w.Body.String())
			}
			switch tc.name {
			case "scan-error":
				for _, ev := range h.audit.events {
					if strings.HasPrefix(ev.Action, "llm.scan.") && ev.Outcome != "failure" {
						t.Errorf("old proxy scan failure becomes action=%s outcome=%s, want failure", ev.Action, ev.Outcome)
					}
				}
			case "opaque-blind":
				eg, sc := countActions(h.audit.events)
				if len(eg) != 0 || len(sc) != 1 {
					t.Errorf("old proxy coverage-only signal: egress=%v scan=%v, want no egress and one scan", eg, sc)
				}
			case "dropped-summary":
				w := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "")
				if !strings.Contains(w.Body.String(), "wardyn_egress_denies_total 0\n") {
					t.Error("old proxy dropped-decisions summary increments policy-denial metric")
				}
			}
		})
	}
}
