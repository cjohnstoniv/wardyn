// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLLMUpstreamPathIsTheClassifiedPath pins F035: the path content inspection
// JUDGED and the path the upstream RECEIVES must be the same bytes.
//
// classifyLLM keys on `rest`, which is derived from r.URL.Path — the
// PERCENT-DECODED path. forwardInspectedLLM used to rebuild the upstream URL by
// string-concatenating that decoded value and handing it to
// http.NewRequestWithContext, which re-PARSES it: a decoded "#" became a
// FRAGMENT and a decoded "?" a QUERY. So a sandbox POST to
// /wardyn/llm/anthropic/v1/messages%23z classified as "v1/messages#z"
// (scanNone — streamed through unscanned, no scan summary) while the wire
// carried POST /v1/messages, the exact endpoint the operator asked to have
// scanned. That is a bypass of block mode from inside the sandbox, on both the
// brokered route and — through this same shared tail — the TLS-MITM lane.
func TestLLMUpstreamPathIsTheClassifiedPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{"fragment_suffix", llmAnthropicPrefix + "v1/messages%23z"},
		{"query_suffix", llmAnthropicPrefix + "v1/messages%3Fz=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cu := captureUpstream(t, true, "llm-ok")
			p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
				anthropicInjector(), testInsecureTLSConfig)
			p.scanner = scanEngine(t, "block", scanTestSecret)

			body := anthropicMessagesBody("please use key " + scanTestSecret + " now")
			rec := httptest.NewRecorder()
			req := mustLocalReq(t, http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			p.ServeHTTP(rec, req)

			if cu.path == "/v1/messages" {
				t.Fatalf("upstream received %q for a request classified as %q: the scanner and the "+
					"forwarder disagreed about where the path ends, so a secret-bearing Messages call "+
					"reached the vendor UNSCANNED under mode=block (upstream body %q)",
					cu.path, strings.TrimPrefix(req.URL.Path, llmAnthropicPrefix), cu.body)
			}
			// The upstream must see EXACTLY the path the classifier judged: the
			// proxy re-encodes the "#"/"?" as %23/%3F on the wire, and the
			// upstream decodes them back to the same bytes.
			want := "/" + strings.TrimPrefix(req.URL.Path, llmAnthropicPrefix)
			if cu.path != want {
				t.Fatalf("upstream path = %q, want %q (the classified path, byte for byte)", cu.path, want)
			}
		})
	}

	// Control: the HONEST spelling is classified as Messages and blocked, on the
	// same proxy — which is what makes the cases above a differential and not a
	// scanner failure.
	t.Run("plain_messages_is_blocked", func(t *testing.T) {
		cu := captureUpstream(t, true, "llm-ok")
		p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
			anthropicInjector(), testInsecureTLSConfig)
		p.scanner = scanEngine(t, "block", scanTestSecret)

		rec := httptest.NewRecorder()
		req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages",
			strings.NewReader(anthropicMessagesBody("please use key "+scanTestSecret+" now")))
		req.Header.Set("Content-Type", "application/json")
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden || cu.reached {
			t.Fatalf("control: plain /v1/messages must block (status=%d reached=%v)", rec.Code, cu.reached)
		}
	})

	// Control: %2F — the encoding that decodes INTO a path separator — must
	// classify as Messages and block, so the fix cannot be read as "escape
	// everything and stop classifying".
	t.Run("encoded_separator_still_classifies", func(t *testing.T) {
		cu := captureUpstream(t, true, "llm-ok")
		p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv),
			anthropicInjector(), testInsecureTLSConfig)
		p.scanner = scanEngine(t, "block", scanTestSecret)

		rec := httptest.NewRecorder()
		req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1%2Fmessages",
			strings.NewReader(anthropicMessagesBody("please use key "+scanTestSecret+" now")))
		req.Header.Set("Content-Type", "application/json")
		p.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden || cu.reached {
			t.Fatalf("control: %%2F-encoded /v1/messages must block (status=%d reached=%v)", rec.Code, cu.reached)
		}
	})
}
