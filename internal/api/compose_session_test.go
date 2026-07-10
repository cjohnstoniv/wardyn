// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lastAuditEvent returns the LAST recorded event with the given action (the
// pipeline can record several of the same action across rounds/subtests; the
// last one is the one the current round just wrote). Fails the test if none.
func lastAuditEvent(t *testing.T, events []types.AuditEvent, action string) types.AuditEvent {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Action == action {
			return events[i]
		}
	}
	t.Fatalf("no %q audit event recorded (have %d events)", action, len(events))
	return types.AuditEvent{}
}

// multiRoundClarifier is a composer.Composer + composer.Clarifier fake that
// asks one question per round for rounds 0 and 1, then reports ready at round 2
// — a REAL multi-round conversation, unlike composer.FakeComposer (which only
// ever asks once, on the first empty-transcript call). This is what lets a
// single test drive clarify(round 0) -> clarify(round 1) -> propose(round 2)
// and assert the client's session id threads through every one of those audit
// events identically.
type multiRoundClarifier struct {
	proposal composer.Proposal
}

func (m *multiRoundClarifier) Propose(_ context.Context, _ composer.ComposeRequest) (composer.Proposal, error) {
	return m.proposal, nil
}

func (m *multiRoundClarifier) Clarify(_ context.Context, req composer.ComposeRequest) (composer.Clarification, error) {
	if req.Round >= 2 {
		return composer.Clarification{Ready: true}, nil
	}
	return composer.Clarification{Ready: false, Questions: []composer.Question{
		{ID: fmt.Sprintf("q%d", req.Round), Question: fmt.Sprintf("question for round %d?", req.Round)},
	}}, nil
}

// wireMultiRoundComposer registers a multiRoundClarifier as the sole (default)
// backend on srv.
func wireMultiRoundComposer(t *testing.T, srv *Server) {
	t.Helper()
	mrc := &multiRoundClarifier{proposal: composer.Proposal{
		Run:          composer.RunInput{Agent: "claude-code", Task: "build a small website"},
		InlinePolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
		Summary:      "proposed a throwaway sandbox",
	}}
	srv.cfg.Composer = singleBackendRegistry(t, mrc)
}

// TestComposeSessionID_InvalidRejectedBothTransports asserts a malformed
// session_id 4xxes BEFORE any backend call, on BOTH transports — it is
// validated in the pre-flush block (composer.ValidateRequest), so the SSE
// transport must ALSO surface a real HTTP status, not a 200 + EvError frame.
func TestComposeSessionID_InvalidRejectedBothTransports(t *testing.T) {
	h := newStreamComposerHarness(t)
	body := `{"prompt":"build a small website","workspace":{"kind":"ephemeral"},"mode":"skip","session_id":"not-a-uuid"}`

	rBuf := httptest.NewRequest(http.MethodPost, "/api/v1/runs/compose", strings.NewReader(body))
	rBuf.Header.Set("Authorization", "Bearer "+adminToken)
	wBuf := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(wBuf, rBuf)
	if wBuf.Code != http.StatusBadRequest {
		t.Fatalf("buffer transport: code = %d, want 400; body=%s", wBuf.Code, wBuf.Body.String())
	}

	rSSE := httptest.NewRequest(http.MethodPost, "/api/v1/runs/compose", strings.NewReader(body))
	rSSE.Header.Set("Authorization", "Bearer "+adminToken)
	rSSE.Header.Set("Accept", "text/event-stream")
	wSSE := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(wSSE, rSSE)
	if wSSE.Code != http.StatusBadRequest {
		t.Fatalf("SSE transport: code = %d, want 400 (pre-flush validation), got %d; body=%s",
			http.StatusBadRequest, wSSE.Code, wSSE.Body.String())
	}
}

// TestComposeAudit_PromptCappedWithTruncationMarker proves the audit cap is
// actually applied in the real pipeline (not just the composer.CapAuditText
// unit test): a prompt within composer's own input cap (16 KiB) but well past
// the 2 KiB per-field audit cap must land in run.compose's Data truncated with
// the explicit marker, not verbatim.
func TestComposeAudit_PromptCappedWithTruncationMarker(t *testing.T) {
	h := newStreamComposerHarness(t)
	longPrompt := strings.Repeat("a", 5000) // > MaxAuditFieldBytes, < MaxPromptBytes
	body := fmt.Sprintf(`{"prompt":%q,"workspace":{"kind":"ephemeral"},"mode":"skip"}`, longPrompt)
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("compose: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	ev := lastAuditEvent(t, h.audit.events, "run.compose")
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode run.compose audit data: %v", err)
	}
	prompt, _ := data["prompt"].(string)
	if prompt == "" {
		t.Fatalf("run.compose audit data has no prompt field: %+v", data)
	}
	if len(prompt) > composer.MaxAuditFieldBytes {
		t.Errorf("audited prompt len = %d, want <= %d", len(prompt), composer.MaxAuditFieldBytes)
	}
	if prompt == longPrompt {
		t.Errorf("prompt was not truncated at all")
	}
	if !strings.Contains(prompt, "truncated") {
		t.Errorf("audited prompt must carry the explicit truncation marker, got tail %q", prompt[max(0, len(prompt)-30):])
	}
}

// TestComposeSessionID_ThreadsAcrossRoundsAndIntoRunCreate is the money test
// for S2: one client-minted session id resent across a 3-round compose
// conversation (clarify round 0, clarify round 1, propose round 2) must show up
// verbatim as correlation_id on EVERY advisory audit event, AND on the
// eventual run.create's compose_session_id — proving the full round -> round ->
// launch thread Decision 1/7 depend on (PG-gated: create-run needs a real
// store).
func TestComposeSessionID_ThreadsAcrossRoundsAndIntoRunCreate(t *testing.T) {
	srv, _ := pgHarness(t)
	audit := srv.cfg.Audit.(*recRecorder)
	wireMultiRoundComposer(t, srv)

	sid := uuid.NewString()
	const prompt = "build and deploy the example web app features"

	// Round 0: no transcript yet -> clarify, one question, no answers to report.
	body0 := fmt.Sprintf(`{"prompt":%q,"workspace":{"kind":"ephemeral"},"session_id":%q,"round":0}`, prompt, sid)
	w0 := do(t, srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body0)
	if w0.Code != http.StatusOK {
		t.Fatalf("round 0: code = %d, want 200; body=%s", w0.Code, w0.Body.String())
	}
	var clar0 clarifyResponse
	if err := json.Unmarshal(w0.Body.Bytes(), &clar0); err != nil {
		t.Fatalf("decode round 0: %v", err)
	}
	if clar0.Kind != "questions" || len(clar0.Questions) != 1 {
		t.Fatalf("round 0 = %+v, want exactly 1 clarify question", clar0)
	}

	data0 := decodeAuditData(t, lastAuditEvent(t, audit.events, "run.compose.clarify"))
	if data0["correlation_id"] != sid {
		t.Errorf("round 0 correlation_id = %v, want %v", data0["correlation_id"], sid)
	}
	if _, has := data0["answers"]; has {
		t.Errorf("round 0 must not carry an answers field (round 0, nothing answered yet), got %v", data0["answers"])
	}
	if qs, _ := data0["question_list"].([]any); len(qs) != 1 {
		t.Errorf("round 0 question_list = %v, want 1 entry", data0["question_list"])
	}

	// Round 1: answer q0, resend the session id -> clarify again (q1), and now
	// the accumulated transcript is the "delta answers" this stateless pipeline
	// can see.
	body1 := fmt.Sprintf(`{"prompt":%q,"workspace":{"kind":"ephemeral"},"session_id":%q,"round":1,`+
		`"transcript":[{"question":"question for round 0?","answer":"yes, deploy to prod"}]}`, prompt, sid)
	w1 := do(t, srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body1)
	if w1.Code != http.StatusOK {
		t.Fatalf("round 1: code = %d, want 200; body=%s", w1.Code, w1.Body.String())
	}
	data1 := decodeAuditData(t, lastAuditEvent(t, audit.events, "run.compose.clarify"))
	if data1["correlation_id"] != sid {
		t.Errorf("round 1 correlation_id = %v, want %v", data1["correlation_id"], sid)
	}
	answers1, _ := data1["answers"].(string)
	if !strings.Contains(answers1, "yes, deploy to prod") {
		t.Errorf("round 1 answers = %q, want it to contain the round-0 answer", answers1)
	}

	// Round 2: both answered -> ready -> a proposal (run.compose).
	body2 := fmt.Sprintf(`{"prompt":%q,"workspace":{"kind":"ephemeral"},"session_id":%q,"round":2,`+
		`"transcript":[{"question":"question for round 0?","answer":"yes, deploy to prod"},`+
		`{"question":"question for round 1?","answer":"no DB access needed"}]}`, prompt, sid)
	w2 := do(t, srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body2)
	if w2.Code != http.StatusOK {
		t.Fatalf("round 2: code = %d, want 200; body=%s", w2.Code, w2.Body.String())
	}
	var prop composeResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &prop); err != nil {
		t.Fatalf("decode round 2: %v", err)
	}
	if prop.Kind != "proposal" {
		t.Fatalf("round 2 kind = %q, want proposal", prop.Kind)
	}

	data2 := decodeAuditData(t, lastAuditEvent(t, audit.events, "run.compose"))
	if data2["correlation_id"] != sid {
		t.Errorf("run.compose correlation_id = %v, want %v", data2["correlation_id"], sid)
	}
	if data2["prompt"] != prompt {
		t.Errorf("run.compose prompt = %v, want %q", data2["prompt"], prompt)
	}
	transcript2, _ := data2["transcript"].(string)
	if !strings.Contains(transcript2, "yes, deploy to prod") || !strings.Contains(transcript2, "no DB access needed") {
		t.Errorf("run.compose transcript = %q, want both round answers", transcript2)
	}
	if _, has := data2["proposed"]; !has {
		t.Errorf("run.compose must carry a proposed field")
	}
	if _, has := data2["setup_items"]; !has {
		t.Errorf("run.compose must carry a setup_items field")
	}
	if ws, _ := data2["workspaces"].([]any); len(ws) != 1 {
		t.Errorf("run.compose workspaces = %v, want 1 entry (the ephemeral workspace)", data2["workspaces"])
	} else if kind, _ := ws[0].(map[string]any)["kind"].(string); kind != "ephemeral" {
		t.Errorf("run.compose workspaces[0].kind = %q, want ephemeral", kind)
	}

	// Launch: create-run carries compose_session_id -> run.create's audit Data
	// must stamp it, the terminal hop of the round -> round -> launch thread.
	createBody := fmt.Sprintf(`{"agent":"claude-code","repo":"acme/widgets","compose_session_id":%q}`, sid)
	wc := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, createBody)
	if wc.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", wc.Code, wc.Body.String())
	}
	dataCreate := decodeAuditData(t, lastAuditEvent(t, audit.events, "run.create"))
	if dataCreate["compose_session_id"] != sid {
		t.Errorf("run.create compose_session_id = %v, want %v", dataCreate["compose_session_id"], sid)
	}

	// Same UUID contract as the compose endpoint: a non-UUID id is audit
	// graffiti and must 400 before anything is created.
	wbad := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","repo":"acme/widgets","compose_session_id":"not-a-uuid"}`)
	if wbad.Code != http.StatusBadRequest {
		t.Errorf("create run with non-UUID compose_session_id: code = %d, want 400", wbad.Code)
	}
}

// decodeAuditData unmarshals an audit event's Data into a generic map.
func decodeAuditData(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode %s audit data: %v", ev.Action, err)
	}
	return data
}
