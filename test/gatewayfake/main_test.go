// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, srv *httptest.Server, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func seen(t *testing.T, srv *httptest.Server) seenReport {
	t.Helper()
	resp, err := http.Get(srv.URL + "/_seen")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out seenReport
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMessagesAnswersPlainAndStreamedAndRecordsTheCredential(t *testing.T) {
	srv := httptest.NewServer((&fake{}).handler())
	defer srv.Close()

	code, body := post(t, srv, "/v1/messages", `{"model":"m1","stream":false}`, map[string]string{"Authorization": "Bearer made-up"})
	if code != 200 || !strings.Contains(body, replyText) || !strings.Contains(body, `"model":"m1"`) {
		t.Fatalf("plain: %d %s", code, body)
	}
	code, body = post(t, srv, "/v1/messages", `{"model":"m1","stream":true}`, nil)
	if code != 200 {
		t.Fatalf("stream: %d", code)
	}
	for _, ev := range []string{"event: message_start", "event: content_block_delta", "event: message_stop", replyText} {
		if !strings.Contains(body, ev) {
			t.Errorf("stream lacks %q: %s", ev, body)
		}
	}

	got := seen(t, srv)
	if got.ModelCalls != 2 || got.ToolCalls != 0 || len(got.Requests) != 2 {
		t.Fatalf("seen = %+v", got)
	}
	if h := got.Requests[0].Headers["authorization"]; len(h) != 1 || h[0] != "Bearer made-up" || got.Requests[0].Route != "model" {
		t.Errorf("the credential did not reach /_seen: %+v", got.Requests[0])
	}
}

func TestMCPCountsOnlyToolCalls(t *testing.T) {
	srv := httptest.NewServer((&fake{}).handler())
	defer srv.Close()

	code, body := post(t, srv, "/mcp", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`, nil)
	if code != 200 || !strings.Contains(body, `"protocolVersion":"2025-03-26"`) {
		t.Fatalf("initialize: %d %s", code, body)
	}
	if code, body = post(t, srv, "/mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); code != 202 || body != "" {
		t.Fatalf("notification: %d %q", code, body)
	}
	if _, body = post(t, srv, "/mcp", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil); !strings.Contains(body, `"name":"echo"`) {
		t.Fatalf("tools/list: %s", body)
	}
	if got := seen(t, srv); got.ToolCalls != 0 {
		t.Fatalf("a list counted as a call: %+v", got)
	}
	if _, body = post(t, srv, "/mcp", `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`, nil); !strings.Contains(body, echoPrefix+"hi") {
		t.Fatalf("tools/call: %s", body)
	}
	if _, body = post(t, srv, "/mcp", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"other"}}`, nil); !strings.Contains(body, "unknown tool") {
		t.Fatalf("unknown tool: %s", body)
	}
	if _, body = post(t, srv, "/mcp", `{"jsonrpc":"2.0","id":5,"method":"nope"}`, nil); !strings.Contains(body, "-32601") {
		t.Fatalf("unknown method: %s", body)
	}

	got := seen(t, srv)
	if got.ToolCalls != 1 || got.ModelCalls != 0 {
		t.Fatalf("seen = %+v", got)
	}
	rpcs := make([]string, 0, len(got.Requests))
	for _, r := range got.Requests {
		if r.Route != "mcp" {
			t.Errorf("route = %q", r.Route)
		}
		rpcs = append(rpcs, r.RPC)
	}
	if want := "initialize,notifications/initialized,tools/list,tools/call,tools/call,nope"; strings.Join(rpcs, ",") != want {
		t.Errorf("rpcs = %v, want %s", rpcs, want)
	}
}

func TestOtherPathsAreRecordedAndRefused(t *testing.T) {
	srv := httptest.NewServer((&fake{}).handler())
	defer srv.Close()
	if code, _ := post(t, srv, "/v1/models", `{}`, nil); code != http.StatusNotFound {
		t.Fatalf("other path = %d, want 404", code)
	}
	got := seen(t, srv)
	if len(got.Requests) != 1 || got.Requests[0].Route != "other" || got.Requests[0].Path != "/v1/models" || got.ModelCalls != 0 {
		t.Fatalf("seen = %+v", got)
	}
}

func TestSeenDoesNotRecordItselfAndMCPGetIs405(t *testing.T) {
	srv := httptest.NewServer((&fake{}).handler())
	defer srv.Close()
	_ = seen(t, srv)
	if got := seen(t, srv); len(got.Requests) != 0 {
		t.Errorf("/_seen recorded itself: %+v", got)
	}
	resp, err := http.Get(srv.URL + "/mcp")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /mcp = %d, want 405", resp.StatusCode)
	}
}
