// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
)

// composeAssistRequest is the POST /api/v1/runs/compose/assist body: the operator's
// ONE plain-language question about the proposed setup, plus the dialog context the
// UI already holds. It is ESCALATION-ONLY (fired only when the operator opens the
// "Ask something else" box) and ADVISORY: the answer is inert text, never re-graded,
// clamped, or fed back into the pipeline. Field names are the frozen Wave-0 contract.
type composeAssistRequest struct {
	Step       string             `json:"step"`      // UI step the question was asked from (audited)
	Prompt     string             `json:"prompt"`    // the operator's task description (dialog context)
	Workspace  composer.Workspace `json:"workspace"` // {kind,path,read_write,repo}
	Backend    string             `json:"backend"`   // "" = registry default
	Transcript []composer.QA      `json:"transcript"`
	Round      int                `json:"round"`

	// Extra step context folded into the question passed to Assist (never audited).
	CurrentQuestion string `json:"currentQuestion"`
	Notes           string `json:"notes"`
	ProposalSummary string `json:"proposalSummary"`
	Question        string `json:"question"`
}

// composeAssistResponse carries the inert advisory answer rendered as plain text.
type composeAssistResponse struct {
	Answer string `json:"answer"`
}

// handleComposeAssist answers one operator question about the proposed sandbox. It
// gates on the SAME composer-enabled 404 as compose (composer egress already
// permitted ⇒ no new allowed destination), reuses the hardened backend transport via
// the registry, and records an audit event that carries NO prompt/question/secret
// content — only the backend used and the UI step.
func (s *Server) handleComposeAssist(w http.ResponseWriter, r *http.Request) {
	if s.composerEnabledOrNotFound(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, composer.MaxTotalInputBytes+64*1024)
	var req composeAssistRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "compose assist request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid compose assist request: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Question) == "" {
		writeError(w, http.StatusBadRequest, "a question is required")
		return
	}

	creq := composer.ComposeRequest{
		Prompt:     req.Prompt,
		Workspace:  req.Workspace,
		Transcript: req.Transcript,
		Round:      req.Round,
	}
	// Assist is advisory + escalation-only: unlike compose it can be asked from the
	// clarify/review steps where the UI hasn't (re)sent prompt/workspace. So do NOT
	// run the strict create-a-run ValidateRequest — the body is already size-bounded
	// by the MaxBytesReader above and the real content is the question. Default an
	// empty workspace to ephemeral so the prompt builder has a valid kind.
	if creq.Workspace.Kind == "" {
		creq.Workspace.Kind = composer.WorkspaceEphemeral
	}

	answer, err := s.cfg.Composer.Assist(r.Context(), req.Backend, creq, assistQuestion(req))
	if err != nil {
		if errors.Is(err, composer.ErrUnknownBackend) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "composer backend: "+err.Error())
		return
	}

	// Advisory audit: NO prompt/question/answer/secret content — only the backend
	// used and the UI step, so the Ask-usage rate is measurable without leaking.
	backend := req.Backend
	if backend == "" {
		backend = s.cfg.Composer.Default()
	}
	auditData, _ := json.Marshal(map[string]string{"backend": backend, "step": req.Step})
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"run.compose.assist", "compose", "success", auditData))

	writeJSON(w, http.StatusOK, composeAssistResponse{Answer: answer})
}

// assistQuestion folds the step context (which question is under review, operator
// notes, the proposal summary) into a short "Context:" line prepended to the actual
// question, so the backend explainer has the same dialog state the UI shows.
func assistQuestion(req composeAssistRequest) string {
	var ctx []string
	if q := strings.TrimSpace(req.CurrentQuestion); q != "" {
		ctx = append(ctx, "the operator is reviewing the clarifying question \""+q+"\"")
	}
	if n := strings.TrimSpace(req.Notes); n != "" {
		ctx = append(ctx, "their working notes: "+n)
	}
	if ps := strings.TrimSpace(req.ProposalSummary); ps != "" {
		ctx = append(ctx, "the proposed setup summary: "+ps)
	}
	q := strings.TrimSpace(req.Question)
	if len(ctx) == 0 {
		return q
	}
	return "Context: " + strings.Join(ctx, "; ") + ".\n" + q
}

// composeTelemetryRequest is the POST /api/v1/runs/compose/telemetry body: a
// client-side funnel beacon. It records mode transitions + risk levels ONLY — never
// prompt/secret content — funneled into the same audit chokepoint as an event.
type composeTelemetryRequest struct {
	Mode          string `json:"mode"`
	CorrelationID string `json:"correlation_id"`
	Risk          string `json:"risk"`
}

// handleComposeTelemetry records a client-funnel beacon (mode transition / risk) so
// the pre-first-call funnel is capturable. It leaks nothing: mode + risk +
// correlation_id only. Same composer-enabled gate; returns 204.
func (s *Server) handleComposeTelemetry(w http.ResponseWriter, r *http.Request) {
	if s.composerEnabledOrNotFound(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var req composeTelemetryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "compose telemetry body too large")
			return
		}
		// Lenient: a malformed beacon must never break the funnel; drop it quietly.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	auditData, _ := json.Marshal(map[string]string{
		"mode": req.Mode, "risk": req.Risk, "correlation_id": req.CorrelationID})
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"run.compose.client", "compose", "success", auditData))
	w.WriteHeader(http.StatusNoContent)
}
