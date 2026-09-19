// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// decisionLogFieldsNotIngested are egress.DecisionLog's top-level JSON fields
// handlePostDecision (internal.go) deliberately does NOT fold into the
// audit row's flat Data map: Request is exploded into host/port/method/path
// instead; Decision drives the action suffix ("egress."+Decision) and the
// outcome, never a Data field of its own; and Scan becomes its OWN separate
// llm.scan.* event rather than riding this one.
var decisionLogFieldsNotIngested = map[string]bool{"request": true, "decision": true, "scan": true}

// TestHandlePostDecisionCarriesEveryDecisionLogField pins handlePostDecision's
// CLOSED map[string]any literal (internal.go) to egress.DecisionLog's actual
// field set BY JSON TAG, reflectively — nothing pinned this before this lane
// (see the Cause/Via fields it was written for): a field added to
// DecisionLog is silently dropped at the one ingest chokepoint that turns a
// proxy decision into an audit row, with no test net to catch it.
func TestHandlePostDecisionCarriesEveryDecisionLogField(t *testing.T) {
	src := readIngestSource(t)
	typ := reflect.TypeOf(egress.DecisionLog{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		if decisionLogFieldsNotIngested[tag] {
			continue
		}
		if !strings.Contains(src, `"`+tag+`"`) {
			t.Errorf("egress.DecisionLog.%s (json %q) is not referenced anywhere in handlePostDecision — "+
				"a field silently dropped at the one place that turns a proxy decision into an audit row",
				f.Name, tag)
		}
	}
}

// readIngestSource returns handlePostDecision's own function body (internal.go,
// from its signature to the next top-level func), so the guard above is
// scoped to the ingest handler and cannot be satisfied by an unrelated mention
// of the same word elsewhere in the file.
func readIngestSource(t *testing.T) string {
	t.Helper()
	// go test's cwd is the package directory, so "internal.go" (this package's
	// own file) resolves without needing to locate the repo root.
	b, err := os.ReadFile("internal.go")
	if err != nil {
		t.Fatalf("read internal.go: %v", err)
	}
	full := string(b)
	start := strings.Index(full, "func (s *Server) handlePostDecision(")
	if start < 0 {
		t.Fatal("handlePostDecision not found in internal/api/internal.go — re-anchor this guard")
	}
	rest := full[start+1:]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		t.Fatal("could not find the end of handlePostDecision — re-anchor this guard")
	}
	return full[start : start+1+end]
}

// TestDecisionIngest_CauseAndViaLandInTheAuditRow is the end-to-end
// companion to the reflective guard above: it drives a real DecisionLog
// carrying Cause/Via through the actual HTTP ingest and reads back the
// PERSISTED audit event's Data payload, so a future refactor that keeps the
// map literal but stops it reaching the audit event still fails.
func TestDecisionIngest_CauseAndViaLandInTheAuditRow(t *testing.T) {
	h := newHarness(t)
	srv := h.srv
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)

	const cause = "tcp dial: dial tcp 10.0.0.1:443: connect: connection refused"
	body, err := json.Marshal(egress.DecisionLog{
		Request:    egress.Request{Host: "portal.sso.us-east-1.amazonaws.com", Port: 443, Method: http.MethodGet},
		Decision:   egress.Deny,
		RuleSource: "builtin:dial-failed",
		Cause:      cause,
		Via:        "direct",
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, string(body)); w.Code >= 300 {
		t.Fatalf("post decision: code = %d; body=%s", w.Code, w.Body.String())
	}

	if len(h.audit.events) == 0 {
		t.Fatal("no audit event recorded")
	}
	ev := h.audit.events[len(h.audit.events)-1]
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode audit event data: %v (data=%s)", err, ev.Data)
	}
	if data["cause"] != cause {
		t.Errorf("audit event data[\"cause\"] = %v, want %q", data["cause"], cause)
	}
	if data["via"] != "direct" {
		t.Errorf("audit event data[\"via\"] = %v, want %q", data["via"], "direct")
	}
}
