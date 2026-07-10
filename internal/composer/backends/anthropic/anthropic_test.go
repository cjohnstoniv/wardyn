// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/composer/backends/composertest"
)

// capturedRequest records what the fake server received from the backend so a test
// can assert request shaping (model, system prompt, user message, schema/tool).
type capturedRequest struct {
	body map[string]any
	path string
}

// newFakeServer returns an httptest.Server that records the first Messages request
// into rec and replies with a Messages response whose content carries the given
// blocks. textJSON is emitted as a text block (Structured Outputs); when toolName
// is non-empty the same JSON is emitted as a tool_use block instead.
func newFakeServer(t *testing.T, rec *capturedRequest, textJSON, toolName string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		if err := json.Unmarshal(raw, &rec.body); err != nil {
			t.Errorf("unmarshal request body: %v (body=%s)", err, raw)
		}
		var content []map[string]any
		if toolName != "" {
			content = []map[string]any{{
				"type":  "tool_use",
				"id":    "toolu_test",
				"name":  toolName,
				"input": json.RawMessage(textJSON),
			}}
		} else {
			content = []map[string]any{{"type": "text", "text": textJSON}}
		}
		resp := map[string]any{
			"id":          "msg_test",
			"type":        "message",
			"role":        "assistant",
			"model":       "claude-sonnet-4-5",
			"stop_reason": "end_turn",
			"content":     content,
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 20},
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertValidProposal checks the fixture mapped into the Proposal we expect,
// plus the anthropic-specific fields composertest.AssertValidProposal doesn't cover.
func assertValidProposal(t *testing.T, p composer.Proposal) {
	t.Helper()
	composertest.AssertValidProposal(t, p)
	if got := len(p.InlinePolicy.AllowedDomains); got != 2 {
		t.Errorf("allowed_domains len = %d, want 2", got)
	}
	if got := len(p.InlinePolicy.EligibleGrants); got != 2 {
		t.Fatalf("eligible_grants len = %d, want 2", got)
	}
	if !strings.Contains(p.Summary, "healthz") {
		t.Errorf("summary = %q, want it to mention healthz", p.Summary)
	}
}

// dig walks a nested map[string]any by keys, returning the leaf and whether the
// full path resolved.
func dig(m map[string]any, keys ...string) (any, bool) {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[k]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func TestNewComposer_Validation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "missing model",
			cfg:     Config{Transport: TransportAPI, APIKey: "k"},
			wantErr: "Model is required",
		},
		{
			name:    "api without key",
			cfg:     Config{Transport: TransportAPI, Model: "claude-sonnet-4-5"},
			wantErr: "api transport requires APIKey",
		},
		{
			name:    "bedrock without region",
			cfg:     Config{Transport: TransportBedrock, Model: "anthropic.claude-sonnet-4-5"},
			wantErr: "bedrock transport requires Region",
		},
		{
			name:    "unknown transport",
			cfg:     Config{Transport: "grpc", Model: "claude-sonnet-4-5"},
			wantErr: "unknown transport",
		},
		{
			name: "api ok",
			cfg:  Config{Transport: TransportAPI, Model: "claude-sonnet-4-5", APIKey: "k"},
		},
		{
			name: "default transport is api ok",
			cfg:  Config{Model: "claude-sonnet-4-5", APIKey: "k"},
		},
		{
			name: "bedrock ok",
			cfg:  Config{Transport: TransportBedrock, Model: "anthropic.claude-sonnet-4-5", Region: "us-east-1"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewComposer(tc.cfg)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("NewComposer() error = nil, want %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("NewComposer() error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewComposer() unexpected error: %v", err)
			}
			if c == nil {
				t.Fatal("NewComposer() returned nil composer with no error")
			}
		})
	}
}

func TestPropose_StructuredOutputs(t *testing.T) {
	var rec capturedRequest
	srv := newFakeServer(t, &rec, composertest.ValidProposalJSON, "")

	c, err := NewComposer(Config{
		Transport:    TransportAPI,
		Model:        "claude-sonnet-4-5",
		APIKey:       "test",
		extraOptions: []option.RequestOption{option.WithBaseURL(srv.URL), option.WithHTTPClient(srv.Client())},
	})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}

	p, err := c.Propose(context.Background(), composertest.SampleRequest())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	assertValidProposal(t, p)

	// Request shaping: model, system prompt, user message, structured-output schema.
	if got := rec.body["model"]; got != "claude-sonnet-4-5" {
		t.Errorf("request model = %v, want claude-sonnet-4-5", got)
	}
	sys, ok := rec.body["system"].([]any)
	if !ok || len(sys) == 0 {
		t.Fatalf("request system = %v, want non-empty array", rec.body["system"])
	}
	sysText, _ := dig(sys[0].(map[string]any), "text")
	if st, _ := sysText.(string); !strings.Contains(st, "Wardyn's Run Composer") {
		t.Errorf("system prompt does not look like composer.SystemPrompt(): %q", st)
	}
	// User message must carry the fenced untrusted attachment section.
	msgs, ok := rec.body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("request messages = %v, want non-empty array", rec.body["messages"])
	}
	userContent, _ := dig(msgs[0].(map[string]any), "content")
	uc, _ := userContent.([]any)
	if len(uc) == 0 {
		t.Fatalf("user message content empty: %v", userContent)
	}
	userText, _ := dig(uc[0].(map[string]any), "text")
	if ut, _ := userText.(string); !strings.Contains(ut, "BEGIN UNTRUSTED ATTACHMENTS") {
		t.Errorf("user message missing untrusted-attachment fence: %q", ut)
	}
	// Structured Outputs: output_config.format.schema must be present, NOT a tool.
	schema, ok := dig(rec.body, "output_config", "format", "schema")
	if !ok {
		t.Fatalf("request missing output_config.format.schema; body=%v", rec.body)
	}
	sm, _ := schema.(map[string]any)
	if sm["type"] != "object" {
		t.Errorf("schema type = %v, want object", sm["type"])
	}
	if _, hasTools := rec.body["tools"]; hasTools {
		t.Error("Structured Outputs path should NOT send tools")
	}
}

func TestPropose_ForcedToolFallback(t *testing.T) {
	var rec capturedRequest
	srv := newFakeServer(t, &rec, composertest.ValidProposalJSON, composer.ProposalSchemaName)

	c, err := NewComposer(Config{
		Transport:     TransportAPI,
		Model:         "claude-sonnet-4-5",
		APIKey:        "test",
		useForcedTool: true,
		extraOptions:  []option.RequestOption{option.WithBaseURL(srv.URL), option.WithHTTPClient(srv.Client())},
	})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}

	p, err := c.Propose(context.Background(), composertest.SampleRequest())
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	assertValidProposal(t, p)

	// Forced-tool path: a single tool whose name is the schema name, pinned via
	// tool_choice; output_config.format must NOT be present.
	tools, ok := rec.body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("request tools = %v, want exactly one tool", rec.body["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != composer.ProposalSchemaName {
		t.Errorf("tool name = %v, want %q", tool["name"], composer.ProposalSchemaName)
	}
	if _, ok := dig(tool, "input_schema", "properties"); !ok {
		t.Errorf("tool input_schema missing properties; tool=%v", tool)
	}
	if ap, _ := dig(tool, "input_schema", "additionalProperties"); ap != false {
		t.Errorf("tool input_schema.additionalProperties = %v, want false", ap)
	}
	choiceName, _ := dig(rec.body, "tool_choice", "name")
	if choiceName != composer.ProposalSchemaName {
		t.Errorf("tool_choice.name = %v, want %q", choiceName, composer.ProposalSchemaName)
	}
	if _, hasFmt := dig(rec.body, "output_config", "format"); hasFmt {
		t.Error("forced-tool path should NOT send output_config.format")
	}
}

func TestPropose_StripsCodeFence(t *testing.T) {
	var rec capturedRequest
	fenced := "```json\n" + composertest.ValidProposalJSON + "\n```"
	srv := newFakeServer(t, &rec, fenced, "")

	c, err := NewComposer(Config{
		Transport:    TransportAPI,
		Model:        "claude-sonnet-4-5",
		APIKey:       "test",
		extraOptions: []option.RequestOption{option.WithBaseURL(srv.URL), option.WithHTTPClient(srv.Client())},
	})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	p, err := c.Propose(context.Background(), composertest.SampleRequest())
	if err != nil {
		t.Fatalf("Propose with fenced JSON: %v", err)
	}
	assertValidProposal(t, p)
}

// TestPropose_MalformedFailsClosed verifies that a model response that never
// produces valid JSON drives ProposeWithRetry to exhaust its attempts and fail
// closed (no partial proposal), AND that the backend re-issued the request.
func TestPropose_MalformedFailsClosed(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		resp := map[string]any{
			"id":          "msg_test",
			"type":        "message",
			"role":        "assistant",
			"model":       "claude-sonnet-4-5",
			"stop_reason": "end_turn",
			"content":     []map[string]any{{"type": "text", "text": "this is not json at all"}},
			"usage":       map[string]any{"input_tokens": 1, "output_tokens": 1},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	const attempts = 3
	c, err := NewComposer(Config{
		Transport:    TransportAPI,
		Model:        "claude-sonnet-4-5",
		APIKey:       "test",
		MaxAttempts:  attempts,
		extraOptions: []option.RequestOption{option.WithBaseURL(srv.URL), option.WithHTTPClient(srv.Client())},
	})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}

	_, err = c.Propose(context.Background(), composertest.SampleRequest())
	if err == nil {
		t.Fatal("Propose() error = nil, want fail-closed error for malformed output")
	}
	if !strings.Contains(err.Error(), "invalid output after") {
		t.Errorf("error = %q, want it to mention bounded-retry fail-closed", err)
	}
	if calls != attempts {
		t.Errorf("server saw %d calls, want %d (one per retry attempt)", calls, attempts)
	}
}

// TestPropose_UnknownGrantKindFailsClosed ensures schema-shaped-but-invalid output
// (an unknown grant kind) is rejected by ParseProposal and surfaces fail-closed.
func TestPropose_UnknownGrantKindFailsClosed(t *testing.T) {
	bad := strings.Replace(composertest.ValidProposalJSON, `"kind": "github_token"`, `"kind": "root_shell"`, 1)
	var rec capturedRequest
	srv := newFakeServer(t, &rec, bad, "")

	c, err := NewComposer(Config{
		Transport:    TransportAPI,
		Model:        "claude-sonnet-4-5",
		APIKey:       "test",
		MaxAttempts:  1,
		extraOptions: []option.RequestOption{option.WithBaseURL(srv.URL), option.WithHTTPClient(srv.Client())},
	})
	if err != nil {
		t.Fatalf("NewComposer: %v", err)
	}
	if _, err := c.Propose(context.Background(), composertest.SampleRequest()); err == nil {
		t.Fatal("Propose() error = nil, want fail-closed error for unknown grant kind")
	}
}

func TestStripCodeFence(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain json untouched", `{"a":1}`, `{"a":1}`},
		{"json fence", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"bare fence", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"leading/trailing space", "  {\"a\":1}  ", `{"a":1}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripCodeFence(strings.TrimSpace(tc.in)); got != tc.want {
				t.Errorf("stripCodeFence(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtractJSON_Errors(t *testing.T) {
	if _, err := extractJSON(nil, false); err == nil {
		t.Error("extractJSON(nil) error = nil, want error")
	}
	// Structured-output path with no text content fails closed.
	if _, err := extractJSON(&sdk.Message{}, false); err == nil {
		t.Error("extractJSON(empty, structured) error = nil, want no-text error")
	}
	// Forced-tool path with no tool_use block fails closed.
	if _, err := extractJSON(&sdk.Message{}, true); err == nil {
		t.Error("extractJSON(empty, forced-tool) error = nil, want no-tool error")
	}
}
