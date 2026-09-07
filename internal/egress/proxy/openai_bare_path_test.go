// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"testing"
)

// TestClassifyLLMBothSpellingsOfEveryPromptBearingArm pins F088: every
// prompt-bearing arm of both classifiers must recognise the BARE spelling as
// well as the "/"-prefixed one.
//
// `rest` is the sandbox-chosen path after /wardyn/llm/<vendor>/, joined with any
// configured gateway prefix — and with NO gateway configured (the default) the
// prefix is empty, so `POST /wardyn/llm/openai/responses` yields exactly
// "responses". classifyOpenAILLM's responses/embeddings arm was suffix-only
// while every sibling arm here and in classifyAnthropicLLM carries the bare
// form, so that spelling fell to the scanNone default: forwarded with the
// brokered credential, unscanned, unrefused under on_scanner_error=block, and
// with no coverage row — the exact "silently allowed" outcome the arm's own doc
// comment says it prevents.
//
// The NEGATIVE control moved to the METHOD axis when F112 made the default arm
// fail-closed: a POST the classifiers have no extractor for is now scanOpaque
// (the sandbox picks the suffix and the vendor adds endpoints, so an enumerated
// allowlist cannot be the fail-closed boundary), while a bodiless GET is what
// still classifies quiet. The parity claim above is unchanged — a named
// prompt-bearing arm must answer the same for both spellings — but a row
// asserting "POST <unlisted> == scanNone" would now be pinning the very gap
// F112 closed, so it is stated as scanOpaque here.
func TestClassifyLLMBothSpellingsOfEveryPromptBearingArm(t *testing.T) {
	names := map[int]string{scanNone: "scanNone", scanMessages: "scanMessages", scanOpaque: "scanOpaque"}
	cases := []struct {
		channel string
		method  string
		rest    string
		want    int
	}{
		{"openai", http.MethodPost, "responses", scanOpaque},
		{"openai", http.MethodPost, "v1/responses", scanOpaque},
		{"openai", http.MethodPost, "embeddings", scanOpaque},
		{"openai", http.MethodPost, "v1/embeddings", scanOpaque},
		{"openai", http.MethodPost, "completions", scanOpaque},
		{"openai", http.MethodPost, "v1/completions", scanOpaque},
		{"openai", http.MethodPost, "chat/completions", scanMessages},
		{"openai", http.MethodPost, "v1/chat/completions", scanMessages},
		// F112: the vendor's own content-upload surface, which the enumerated
		// default used to stream through quietly with the brokered credential.
		{"openai", http.MethodPost, "v1/files", scanOpaque},
		{"openai", http.MethodPost, "v1/audio/transcriptions", scanOpaque},
		{"openai", http.MethodPost, "v1/audio/translations", scanOpaque},
		{"openai", http.MethodPost, "v1/audio/speech", scanOpaque},
		{"openai", http.MethodPost, "v1/models", scanOpaque},
		// Negative control: a bodiless read is still quiet, both spellings.
		{"openai", http.MethodGet, "models", scanNone},
		{"openai", http.MethodGet, "v1/models", scanNone},
		{"anthropic", http.MethodPost, "messages", scanMessages},
		{"anthropic", http.MethodPost, "v1/messages", scanMessages},
		{"anthropic", http.MethodPost, "batches", scanOpaque},
		{"anthropic", http.MethodPost, "v1/messages/batches", scanOpaque},
		{"anthropic", http.MethodPost, "complete", scanOpaque},
		{"anthropic", http.MethodPost, "v1/complete", scanOpaque},
		{"anthropic", http.MethodPost, "v1/files", scanOpaque},
		{"anthropic", http.MethodPost, "v1/models", scanOpaque},
		{"anthropic", http.MethodGet, "models", scanNone},
		{"anthropic", http.MethodGet, "v1/models", scanNone},
		// F088/F112 SECOND AXIS — the VERB. hasScannableBody accepts POST, PUT
		// and PATCH, so a PUT/PATCH body reaches the vendor exactly as a POST
		// body does; the classifiers used to answer scanNone for anything but a
		// POST, which made `PUT /v1/messages` a silent brokered forward with the
		// secret in the body under mode=block. Every body-bearing verb that is
		// not the vendor's documented POST must land on the fail-closed default.
		{"anthropic", http.MethodPut, "v1/messages", scanOpaque},
		{"anthropic", http.MethodPatch, "v1/messages", scanOpaque},
		{"anthropic", http.MethodPut, "messages", scanOpaque},
		{"anthropic", http.MethodPatch, "v1/messages/count_tokens", scanOpaque},
		{"anthropic", http.MethodPut, "v1/files", scanOpaque},
		{"openai", http.MethodPut, "v1/chat/completions", scanOpaque},
		{"openai", http.MethodPatch, "v1/chat/completions", scanOpaque},
		{"openai", http.MethodPut, "chat/completions", scanOpaque},
		{"openai", http.MethodPut, "v1/files", scanOpaque},
		// Bodiless verbs stay quiet on BOTH vendors — the negative control that
		// keeps the fix from turning every read into an uninspected-channel row.
		{"anthropic", http.MethodDelete, "v1/files/abc", scanNone},
		{"anthropic", http.MethodHead, "v1/models", scanNone},
		{"openai", http.MethodDelete, "v1/files/abc", scanNone},
		{"openai", http.MethodHead, "v1/models", scanNone},
	}
	for _, tc := range cases {
		t.Run(tc.channel+"/"+tc.method+"/"+tc.rest, func(t *testing.T) {
			var got int
			if tc.channel == "openai" {
				got = classifyOpenAILLM(tc.method, tc.rest)
			} else {
				got = classifyAnthropicLLM(tc.method, tc.rest)
			}
			if got != tc.want {
				t.Fatalf("classify%s(%s, %q) = %s, want %s — a prompt-bearing endpoint must classify "+
					"the same in BOTH its bare and its \"/\"-prefixed spelling, and a POST with no "+
					"extractor must be honestly uninspected rather than quiet",
					tc.channel, tc.method, tc.rest, names[got], names[tc.want])
			}
		})
	}
}
