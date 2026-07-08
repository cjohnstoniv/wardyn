// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
)

// TestNoAmbientKeyLeakOnAzure guards against the OpenAI SDK's environment
// defaults bleeding an ambient OPENAI_API_KEY into the Azure request as a stray
// Authorization: Bearer header. Azure must authenticate ONLY via the Api-Key
// header (or an Entra bearer middleware), never with an unrelated OpenAI key.
func TestNoAmbientKeyLeakOnAzure(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-LEAKED-ambient-key")
	var gotAuth, gotApiKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotApiKey = r.Header.Get("Api-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chatCompletionResponse(validProposalJSON, "")))
	}))
	defer srv.Close()

	c, err := NewComposer(Config{
		Transport:  TransportAzure,
		Model:      "dep",
		BaseURL:    srv.URL,
		APIKey:     "azure-key",
		AzureAuth:  AzureAuthAPIKey,
		httpClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Propose(context.Background(), composer.ComposeRequest{Prompt: "hi"}); err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("ambient OPENAI_API_KEY leaked as Authorization=%q on the azure request", gotAuth)
	}
	if gotApiKey != "azure-key" {
		t.Errorf("Api-Key header = %q, want azure-key", gotApiKey)
	}
}

// TestNoAmbientBaseURLRedirectOnAPI guards against an ambient OPENAI_BASE_URL in
// the daemon environment silently redirecting the "api" transport (and the real
// configured key) to an operator-unintended host. With no caller BaseURL we pin
// the canonical production endpoint, so the ambient value must be ignored.
func TestNoAmbientBaseURLRedirectOnAPI(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chatCompletionResponse(validProposalJSON, "")))
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL)

	c, err := NewComposer(Config{
		Transport: TransportAPI,
		Model:     "gpt-4o",
		APIKey:    "sk-real-key",
		// no BaseURL set by caller -> must pin api.openai.com, not the env value
	})
	if err != nil {
		t.Fatal(err)
	}
	// We don't care that this fails to reach the real network; we only assert the
	// ambient httptest server was NOT contacted.
	_, _ = c.Propose(context.Background(), composer.ComposeRequest{Prompt: "hi"})
	if hit {
		t.Errorf("api transport followed ambient OPENAI_BASE_URL %s instead of the pinned production endpoint", srv.URL)
	}
}
