// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeAndValidateCreateRun_Reasons pins the machine-readable `reason`
// (#656) on every POST /runs field-validation refusal that decodeAndValidateCreateRun
// answers with no store/runner wired — the same shape
// TestCreateRun_TextFieldsAreCappedAndControlCharFree already pins for status
// and field name, now proven for the wire class too. Two causes never share a
// reason (#656's own rule), so each row here is a distinct cause and gets its
// own constant.
func TestDecodeAndValidateCreateRun_Reasons(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
		wantReason string
	}{
		{
			name:       "no agent, no image, no workspace",
			body:       `{}`,
			wantStatus: http.StatusBadRequest,
			wantReason: reasonAgentRequired,
		},
		{
			name:       "a client-forged reserved task",
			body:       `{"agent":"claude-code","task":"workspace record"}`,
			wantStatus: http.StatusBadRequest,
			wantReason: reasonRunTaskReserved,
		},
		{
			name:       "image and devcontainer_repo both set",
			body:       `{"agent":"claude-code","image":"ubuntu:24.04","devcontainer_repo":"org/app"}`,
			wantStatus: http.StatusBadRequest,
			wantReason: reasonInvalidImageBuildRequest,
		},
		{
			name:       "unknown confinement_class",
			body:       `{"agent":"claude-code","confinement_class":"CC9"}`,
			wantStatus: http.StatusBadRequest,
			wantReason: reasonConfinementClassUnknown,
		},
		{
			name:       "unknown task_mode",
			body:       `{"agent":"claude-code","task_mode":"bogus"}`,
			wantStatus: http.StatusBadRequest,
			wantReason: reasonTaskModeUnknown,
		},
		{
			name:       "unknown interactive_start",
			body:       `{"agent":"claude-code","interactive_start":"bogus"}`,
			wantStatus: http.StatusBadRequest,
			wantReason: reasonInteractiveStartUnknown,
		},
		{
			name:       "unknown tool_approvals",
			body:       `{"agent":"claude-code","tool_approvals":"bogus"}`,
			wantStatus: http.StatusBadRequest,
			wantReason: reasonToolApprovalsUnknown,
		},
		{
			name:       "tool_approvals=hold on codex-cli",
			body:       `{"agent":"codex-cli","tool_approvals":"hold"}`,
			wantStatus: http.StatusBadRequest,
			wantReason: reasonToolApprovalsHoldUnsupportedAgent,
		},
		{
			name:       "tool_approvals=hold on an interactive run",
			body:       `{"agent":"claude-code","interactive":true,"tool_approvals":"hold"}`,
			wantStatus: http.StatusBadRequest,
			wantReason: reasonToolApprovalsHoldInteractiveConflict,
		},
		{
			name:       "an over-long task",
			body:       `{"agent":"claude-code","task":"` + strings.Repeat("t", maxRunTaskLen+1) + `"}`,
			wantStatus: http.StatusBadRequest,
			wantReason: reasonRunFieldTooLong,
		},
		{
			name:       "a control character in the agent field",
			body:       `{"agent":"claude` + `\u0007` + `code"}`,
			wantStatus: http.StatusBadRequest,
			wantReason: reasonRunFieldControlChar,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			if _, _, _, _, ok := h.srv.decodeAndValidateCreateRun(w, r); ok {
				t.Fatalf("%s was accepted: %d %s", tc.name, w.Code, w.Body.String())
			}
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if got := errorReason(w); got != tc.wantReason {
				t.Errorf("reason = %q, want %q; body=%s", got, tc.wantReason, w.Body.String())
			}
		})
	}
}
