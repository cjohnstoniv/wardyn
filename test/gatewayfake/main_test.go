// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
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

// eventData returns the data payload of every server-sent event named name in body.
func eventData(t *testing.T, body, name string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, block := range strings.Split(body, "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) != 2 || lines[0] != "event: "+name {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &data); err != nil {
			t.Fatalf("event %s: %v", name, err)
		}
		out = append(out, data)
	}
	return out
}

// A streamed Messages answer carries token counts in message_start (input) and message_delta (output),
// the two events a usage meter reads.
func TestMessagesStreamCarriesUsage(t *testing.T) {
	srv := httptest.NewServer((&fake{}).handler())
	defer srv.Close()
	_, body := post(t, srv, "/v1/messages", `{"model":"m1","stream":true}`, nil)
	start := eventData(t, body, "message_start")
	delta := eventData(t, body, "message_delta")
	if len(start) != 1 || len(delta) != 1 {
		t.Fatalf("message_start %d, message_delta %d: %s", len(start), len(delta), body)
	}
	if in := start[0]["message"].(map[string]any)["usage"].(map[string]any)["input_tokens"]; in != float64(3) {
		t.Errorf("message_start input_tokens = %v, want 3", in)
	}
	if out := delta[0]["usage"].(map[string]any)["output_tokens"]; out != float64(5) {
		t.Errorf("message_delta output_tokens = %v, want 5", out)
	}
}

// A streamed Responses answer ends with response.completed, whose response object carries the usage.
func TestResponsesStreamEndsInCompletedWithUsage(t *testing.T) {
	srv := httptest.NewServer((&fake{}).handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-5","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status %d, content type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	completed := eventData(t, body, "response.completed")
	events := regexp.MustCompile(`(?m)^event: (.+)$`).FindAllStringSubmatch(body, -1)
	if len(completed) != 1 || events[len(events)-1][1] != "response.completed" {
		t.Fatalf("response.completed must be the last event, once: %s", body)
	}
	response := completed[0]["response"].(map[string]any)
	usage := response["usage"].(map[string]any)
	if usage["input_tokens"] != float64(3) || usage["output_tokens"] != float64(5) || response["model"] != "gpt-5" {
		t.Errorf("response = %v, want usage 3/5 and the requested model", response)
	}
	if len(eventData(t, body, "response.created")) != 1 || !strings.Contains(body, replyText) {
		t.Errorf("stream lacks response.created or the reply text: %s", body)
	}
}

func TestResponsesPlainAnswerAndRecording(t *testing.T) {
	srv := httptest.NewServer((&fake{}).handler())
	defer srv.Close()
	code, body := post(t, srv, "/v1/responses", `{"model":"gpt-5","input":"hi"}`, map[string]string{"Authorization": "Bearer made-up"})
	var got struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Usage  struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || code != 200 || got.Object != "response" || got.Model != "gpt-5" || got.Usage.In != 3 || got.Usage.Out != 5 {
		t.Fatalf("plain: %d %s", code, body)
	}
	if code, _ := post(t, srv, "/v1/responses", `{}`, nil); code != 200 {
		t.Fatalf("empty body: %d", code)
	}
	seen := seen(t, srv)
	if seen.ModelCalls != 2 || seen.Requests[0].Route != "model" || seen.Requests[0].Path != "/v1/responses" {
		t.Fatalf("seen = %+v", seen)
	}
}

func TestResponsesRefusesOtherMethods(t *testing.T) {
	srv := httptest.NewServer((&fake{}).handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/responses")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/responses = %d, want 405", resp.StatusCode)
	}
}
