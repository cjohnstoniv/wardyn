// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestLLM404DetailIsSelfExplaining: the brokered-LLM 404 used to say "no LLM
// credential is brokered for api.anthropic.com" — a host the reader can do
// nothing with, and no word about whether waiting for an approval would help.
// It now carries the control plane's own reason when there is one, and ALWAYS
// the below-policy clause, because an agent that retries a missing model
// credential retries forever.
func TestLLM404DetailIsSelfExplaining(t *testing.T) {
	generic := llm404Detail("")
	if !strings.Contains(generic, llmNoCredentialDetail) || !strings.Contains(generic, llmBelowPolicyClause) {
		t.Errorf("generic detail = %q, want the route sentence plus the below-policy clause", generic)
	}
	compiled := llm404Detail("  Bedrock is configured but its credential expired at 2026-09-12T00:00:00Z; reconnect it  ")
	if !strings.Contains(compiled, "Bedrock is configured but its credential expired") {
		t.Errorf("compiled detail = %q, want the control plane's own reason", compiled)
	}
	if strings.Contains(compiled, llmNoCredentialDetail) {
		t.Errorf("compiled detail = %q, must not also carry the generic sentence", compiled)
	}
	if !strings.Contains(compiled, llmBelowPolicyClause) {
		t.Errorf("compiled detail = %q, want the below-policy clause either way", compiled)
	}
}

// TestLocalLLM404RendersTheCompiledDetail drives the real route: no injector, so
// no credential is brokered, and the 404 body must be the dispatch-compiled
// detail plus the clause.
func TestLocalLLM404RendersTheCompiledDetail(t *testing.T) {
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:                uuid.New(),
		Policy:               CompilePolicy(types.RunPolicySpec{}),
		Sink:                 &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver:             publicResolver{},
		Dial:                 redirectDial("127.0.0.1:1"),
		LLMUnavailableDetail: "Bedrock is configured but its credential expired at 2026-09-12T00:00:00Z; reconnect it",
	})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, "POST", llmAnthropicPrefix+"v1/messages", strings.NewReader(`{}`)))

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404 when no LLM credential", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"no_llm_credential", "credential expired at 2026-09-12T00:00:00Z", llmBelowPolicyClause} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %q is missing %q", body, want)
		}
	}
}
