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
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// trackCAssistBackend is a fake composer backend implementing Assister, for the
// assist endpoint test. Prefixed "trackC" to avoid collisions with other api-package
// test fakes when merged.
type trackCAssistBackend struct {
	answer       string
	lastQuestion string
	lastReq      composer.ComposeRequest
}

func (b *trackCAssistBackend) Propose(_ context.Context, req composer.ComposeRequest) (composer.Proposal, error) {
	return composer.Proposal{}, nil
}

func (b *trackCAssistBackend) Assist(_ context.Context, req composer.ComposeRequest, question string) (string, error) {
	b.lastReq = req
	b.lastQuestion = question
	return b.answer, nil
}

// trackCAssistServer builds a Server whose composer registry has one fake backend.
func trackCAssistServer(t *testing.T, backend composer.Composer, audit *recRecorder) *Server {
	t.Helper()
	reg, err := composer.NewRegistry("fake", []composer.RegistryEntry{
		{Info: composer.BackendInfo{Name: "fake", Provider: "fake"}, Composer: backend},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return New(Config{
		Identity:      mustIDP(t),
		Approvals:     newFakeApprovals(),
		Broker:        &fakeBroker{},
		Audit:         audit,
		AdminToken:    adminToken,
		TrustDomain:   "wardyn.local",
		DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
		Composer:      reg,
	})
}

func TestComposeAssist_ReturnsAnswer(t *testing.T) {
	audit := &recRecorder{}
	be := &trackCAssistBackend{answer: "The agent has no internet access and cannot reach GitHub."}
	srv := trackCAssistServer(t, be, audit)

	body := `{"step":"clarify","prompt":"add a healthz endpoint","workspace":{"kind":"ephemeral"},` +
		`"question":"Can it reach the internet?","currentQuestion":"Push access?","notes":"unsure","proposalSummary":"CC2 run"}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs/compose/assist", adminToken, body)
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
	var found bool
	for _, ev := range audit.events {
		if ev.Action != "run.compose.assist" {
			continue
		}
		found = true
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
	if !found {
		t.Error("no run.compose.assist audit event recorded")
	}
}

func TestComposeAssist_RejectsEmptyQuestion(t *testing.T) {
	srv := trackCAssistServer(t, &trackCAssistBackend{answer: "x"}, &recRecorder{})
	body := `{"prompt":"add a healthz endpoint","workspace":{"kind":"ephemeral"},"question":"  "}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs/compose/assist", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty question code = %d, want 400", w.Code)
	}
}

func TestComposeTelemetry_RecordsBeaconOnly(t *testing.T) {
	audit := &recRecorder{}
	srv := trackCAssistServer(t, &trackCAssistBackend{answer: "x"}, audit)

	body := `{"mode":"review","correlation_id":"corr-123","risk":"low"}`
	w := do(t, srv, http.MethodPost, "/api/v1/runs/compose/telemetry", adminToken, body)
	if w.Code != http.StatusNoContent {
		t.Fatalf("telemetry code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	var found bool
	for _, ev := range audit.events {
		if ev.Action != "run.compose.client" {
			continue
		}
		found = true
		s := string(ev.Data)
		if !strings.Contains(s, `"mode":"review"`) || !strings.Contains(s, `"risk":"low"`) || !strings.Contains(s, `"correlation_id":"corr-123"`) {
			t.Errorf("telemetry audit missing mode/risk/correlation_id: %s", s)
		}
	}
	if !found {
		t.Error("no run.compose.client audit event recorded")
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
