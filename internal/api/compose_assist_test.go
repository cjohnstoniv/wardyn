// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
)

// trackCAssistBackend is a fake composer backend implementing Assister, for the
// assist endpoint test. Prefixed "trackC" to avoid collisions with other api-package
// test fakes when merged.
type trackCAssistBackend struct {
	answer       string
	lastQuestion string
}

func (b *trackCAssistBackend) Propose(_ context.Context, req composer.ComposeRequest) (composer.Proposal, error) {
	return composer.Proposal{}, nil
}

func (b *trackCAssistBackend) Assist(_ context.Context, _ composer.ComposeRequest, question string) (string, error) {
	b.lastQuestion = question
	return b.answer, nil
}

// trackCAssistHarness wires backend as the sole composer registry entry on the
// standard api harness so the assist/telemetry endpoints are Enabled().
func trackCAssistHarness(t *testing.T, backend composer.Composer) *harness {
	t.Helper()
	h := newHarness(t)
	h.srv.cfg.Composer = singleBackendRegistry(t, backend)
	return h
}

func TestComposeAssist_ReturnsAnswer(t *testing.T) {
	be := &trackCAssistBackend{answer: "The agent has no internet access and cannot reach GitHub."}
	h := trackCAssistHarness(t, be)

	body := `{"step":"clarify","prompt":"add a healthz endpoint","workspace":{"kind":"ephemeral"},` +
		`"question":"Can it reach the internet?","currentQuestion":"Push access?","notes":"unsure","proposalSummary":"CC2 run"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose/assist", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("assist code = %d, body=%s", w.Code, w.Body.String())
	}
	var resp composeAssistResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Answer != be.answer {
		t.Errorf("answer = %q, want %q", resp.Answer, be.answer)
	}

	// The step context was folded into the question the backend saw.
	if !strings.Contains(be.lastQuestion, "Can it reach the internet?") {
		t.Errorf("backend question missing operator question: %q", be.lastQuestion)
	}
	if !strings.Contains(be.lastQuestion, "Context:") || !strings.Contains(be.lastQuestion, "Push access?") {
		t.Errorf("backend question missing folded step context: %q", be.lastQuestion)
	}

	// Audit records the backend + step ONLY — never prompt/question/answer content.
	ev := lastAuditEvent(t, h.audit.events, "run.compose.assist")
	s := string(ev.Data)
	for _, leak := range []string{"reach the internet", "healthz", "no internet access", "Push access"} {
		if strings.Contains(s, leak) {
			t.Errorf("assist audit leaked content %q: %s", leak, s)
		}
	}
	if !strings.Contains(s, `"step":"clarify"`) || !strings.Contains(s, `"backend":"fake"`) {
		t.Errorf("assist audit missing backend/step: %s", s)
	}
}

func TestComposeAssist_RejectsEmptyQuestion(t *testing.T) {
	h := trackCAssistHarness(t, &trackCAssistBackend{answer: "x"})
	body := `{"prompt":"add a healthz endpoint","workspace":{"kind":"ephemeral"},"question":"  "}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose/assist", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty question code = %d, want 400", w.Code)
	}
}

func TestComposeTelemetry_RecordsBeaconOnly(t *testing.T) {
	h := trackCAssistHarness(t, &trackCAssistBackend{answer: "x"})

	body := `{"mode":"review","correlation_id":"corr-123","risk":"low"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose/telemetry", adminToken, body)
	if w.Code != http.StatusNoContent {
		t.Fatalf("telemetry code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	ev := lastAuditEvent(t, h.audit.events, "run.compose.client")
	s := string(ev.Data)
	if !strings.Contains(s, `"mode":"review"`) || !strings.Contains(s, `"risk":"low"`) || !strings.Contains(s, `"correlation_id":"corr-123"`) {
		t.Errorf("telemetry audit missing mode/risk/correlation_id: %s", s)
	}
}

// TestComposeAssist_404WhenDisabled confirms both new routes share the compose
// composer-enabled 404 gate (newHarness sets no Composer).
func TestComposeAssist_404WhenDisabled(t *testing.T) {
	h := newHarness(t)
	body := `{"question":"anything","workspace":{"kind":"ephemeral"},"prompt":"p"}`
	if w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose/assist", adminToken, body); w.Code != http.StatusNotFound {
		t.Errorf("assist disabled code = %d, want 404", w.Code)
	}
	if w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose/telemetry", adminToken, `{"mode":"describe"}`); w.Code != http.StatusNotFound {
		t.Errorf("telemetry disabled code = %d, want 404", w.Code)
	}
}
