// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// backendParam reads an optional ?backend= override for a composer-backed call.
func backendParam(r *http.Request) string { return r.URL.Query().Get("backend") }

func (s *Server) composerBackendOrDefault(r *http.Request) string {
	if b := backendParam(r); b != "" {
		return b
	}
	if s.cfg.Composer != nil {
		return s.cfg.Composer.Default()
	}
	return ""
}

// handleSuggestVerifyFix is the AGENTIC half of the verify fix loop (the
// deterministic half is one-click "approve this denied egress host / add this
// missing secret", computed in the UI from observed-egress + missing secrets).
// It asks a compose backend to propose the single most likely concrete fix for
// a FAILED verify — a missing egress domain, a missing secret, or a corrected
// command — from the failing step + the already-SECRET-MASKED logs (safe to
// send to the model) + the detected profile. Advisory + human-gated: it returns
// a suggestion the operator applies via the existing endpoints; it never
// auto-applies anything. Capped rounds are enforced by the UI.
func (s *Server) handleSuggestVerifyFix(w http.ResponseWriter, r *http.Request) {
	if s.composerEnabledOrNotFound(w) {
		return
	}
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}
	var vr workspacescan.VerifyResult
	if len(ws.VerifyResult) == 0 || json.Unmarshal(ws.VerifyResult, &vr) != nil || vr.OK || len(vr.Steps) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no failed verify result to diagnose — run verify first")
		return
	}
	profile, _ := workspaceProfile(ws)

	question := buildVerifyFixQuestion(vr, profile)
	// Empty ephemeral workspace so the composer's prompt builder has a valid kind;
	// the real content is the question (already secret-masked).
	creq := composer.ComposeRequest{Workspace: composer.Workspace{Kind: composer.WorkspaceEphemeral}}
	answer, aerr := s.cfg.Composer.Assist(r.Context(), backendParam(r), creq, question)
	if aerr != nil {
		if errors.Is(aerr, composer.ErrUnknownBackend) {
			writeError(w, http.StatusBadRequest, aerr.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "composer backend: "+aerr.Error())
		return
	}
	// Content-free audit: backend only, never the suggestion or logs.
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"workspace.import.suggest_fix", id.String(), "success", mustJSON(map[string]any{
			"backend": s.composerBackendOrDefault(r),
		})))
	writeJSON(w, http.StatusOK, map[string]any{"suggestion": answer})
}

// buildVerifyFixQuestion assembles a compact, secret-masked prompt describing
// the failed verify for the backend to diagnose.
func buildVerifyFixQuestion(vr workspacescan.VerifyResult, p workspacescan.WorkspaceProfile) string {
	var b strings.Builder
	b.WriteString("A workspace environment VERIFY run failed inside a confined sandbox with default-deny egress. ")
	b.WriteString("Detected: languages=[" + strings.Join(p.Languages, ",") + "] packageManagers=[" + strings.Join(p.PackageManagers, ",") + "]")
	if len(p.ServicesNeeded) > 0 {
		b.WriteString(" services=[" + strings.Join(p.ServicesNeeded, ",") + "]")
	}
	b.WriteString(".\n\nSteps run:\n")
	for _, st := range vr.Steps {
		fmtLine(&b, st)
	}
	b.WriteString("\nGiven the sandbox is default-deny egress (only registries the scan detected are allowed) and secrets are broker-injected by NAME, suggest the SINGLE most likely concrete fix: either an egress domain to allow, a secret to add by name, or a corrected setup command. Be specific and concise (2-3 sentences).")
	return b.String()
}

func fmtLine(b *strings.Builder, st workspacescan.VerifyStepResult) {
	status := "ok"
	if st.TimedOut {
		status = "TIMED OUT"
	} else if st.ExitCode != 0 {
		status = "FAILED exit " + strconv.Itoa(st.ExitCode)
	}
	b.WriteString("- [" + st.Stage + "] `" + st.Command + "` → " + status + "\n")
	if st.ExitCode != 0 || st.TimedOut {
		tail := st.LogTail
		if tail == "" {
			tail = st.LogHead
		}
		if tail != "" {
			b.WriteString("  log: " + strings.ReplaceAll(strings.TrimSpace(tail), "\n", " ") + "\n")
		}
	}
}
