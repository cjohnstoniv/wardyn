// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command gatewayfake stands behind an AI gateway in the nightly gateway job
// (scripts/kind-gateway-run.sh) and in the gateway recipe test
// (internal/egress/proxy/gateway_recipes_docker_test.go): a canned Anthropic Messages and OpenAI Responses
// endpoint, a one-tool MCP server, and a /_seen listing of what reached it. It holds no provider key and
// calls nothing; the only credential it ever sees is the throwaway value the job makes up.
//
// A TEST BINARY, never built by `make agent-images` and never published.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
)

const (
	// replyText is the model's canned answer, and echoPrefix the tool's.
	replyText  = "hello from the gateway fake"
	echoPrefix = "echo: "
	// maxBody bounds a request the fake reads; maxSeen bounds the list /_seen keeps.
	maxBody = 1 << 20
	maxSeen = 200
)

// seenRequest is one request that reached the fake: which of its two routes it
// took, and every header the gateway forwarded.
type seenRequest struct {
	Route   string              `json:"route"` // "model", "mcp" or "other"
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	RPC     string              `json:"rpc,omitempty"` // the MCP method, for an mcp request
	Headers map[string][]string `json:"headers"`
}

// seenReport is /_seen: counts to assert on, and the requests behind them.
type seenReport struct {
	ModelCalls int           `json:"model_calls"`
	ToolCalls  int           `json:"tool_calls"` // tools/call on the one tool, not initialize or tools/list
	Requests   []seenRequest `json:"requests"`
}

type fake struct {
	mu   sync.Mutex
	seen seenReport
}

func (f *fake) record(r *http.Request, route, rpc string) {
	h := make(map[string][]string, len(r.Header))
	for k, v := range r.Header {
		h[strings.ToLower(k)] = append([]string(nil), v...)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen.Requests) < maxSeen {
		f.seen.Requests = append(f.seen.Requests, seenRequest{Route: route, Method: r.Method, Path: r.URL.Path, RPC: rpc, Headers: h})
	}
}

func (f *fake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("/_seen", f.serveSeen)
	mux.HandleFunc("/v1/messages", f.serveMessages)
	mux.HandleFunc("/v1/messages/count_tokens", f.serveCountTokens)
	mux.HandleFunc("/v1/responses", f.serveResponses)
	mux.HandleFunc("/mcp", f.serveMCP)
	// Any other path the gateway forwards is recorded and refused, so /_seen
	// names every path a harness reached beyond the two routes above.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, "other", "")
		http.NotFound(w, r)
	})
	return mux
}

func (f *fake) serveSeen(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	out := f.seen
	out.Requests = append([]seenRequest(nil), f.seen.Requests...)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// serveMessages answers POST /v1/messages. A "stream": true request gets the
// server-sent events the Messages API sends, a plain one the single message.
func (f *fake) serveMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = readJSON(r, &req)
	f.record(r, "model", "")
	f.mu.Lock()
	f.seen.ModelCalls++
	f.mu.Unlock()
	model := req.Model
	if model == "" {
		model = "fake-model"
	}
	message := map[string]any{
		"id": "msg_gatewayfake", "type": "message", "role": "assistant", "model": model,
		"content":     []map[string]string{{"type": "text", "text": replyText}},
		"stop_reason": "end_turn", "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": 3, "output_tokens": 5},
	}
	if !req.Stream {
		writeJSON(w, http.StatusOK, message)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	start := map[string]any{
		"id": "msg_gatewayfake", "type": "message", "role": "assistant", "model": model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": 3, "output_tokens": 1},
	}
	events := []struct {
		name string
		data map[string]any
	}{
		{"message_start", map[string]any{"type": "message_start", "message": start}},
		{"content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}}},
		{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": replyText}}},
		{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}},
		{"message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 5}}},
		{"message_stop", map[string]any{"type": "message_stop"}},
	}
	for _, e := range events {
		b, _ := json.Marshal(e.data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, b)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

// serveResponses answers POST /v1/responses. A "stream": true request gets the server-sent events the
// Responses API sends, ending in response.completed with the usage; a plain one gets the response object.
func (f *fake) serveResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = readJSON(r, &req)
	f.record(r, "model", "")
	f.mu.Lock()
	f.seen.ModelCalls++
	f.mu.Unlock()
	model := req.Model
	if model == "" {
		model = "fake-model"
	}
	// The object carries the fields a client library reads, so a gateway that parses it into a typed
	// response has what it expects.
	response := func(status string, usage any) map[string]any {
		return map[string]any{
			"id": "resp_gatewayfake", "object": "response", "created_at": 1700000000, "status": status, "model": model,
			"error": nil, "incomplete_details": nil, "instructions": nil, "metadata": map[string]string{},
			"parallel_tool_calls": true, "temperature": 1.0, "top_p": 1.0, "tool_choice": "auto", "tools": []any{},
			"text": map[string]any{"format": map[string]string{"type": "text"}}, "truncation": "disabled", "store": false,
			"output": []any{messageItem(status)}, "usage": usage,
		}
	}
	usage := map[string]any{
		"input_tokens": 3, "input_tokens_details": map[string]int{"cached_tokens": 0},
		"output_tokens": 5, "output_tokens_details": map[string]int{"reasoning_tokens": 0}, "total_tokens": 8,
	}
	if !req.Stream {
		writeJSON(w, http.StatusOK, response("completed", usage))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	part := map[string]any{"type": "output_text", "text": replyText, "annotations": []any{}}
	events := []struct {
		name string
		data map[string]any
	}{
		{"response.created", map[string]any{"type": "response.created", "response": response("in_progress", nil)}},
		{"response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": messageItem("in_progress")}},
		{"response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_1", "output_index": 0, "content_index": 0, "delta": replyText}},
		{"response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": "msg_1", "output_index": 0, "content_index": 0, "text": replyText}},
		{"response.content_part.done", map[string]any{"type": "response.content_part.done", "item_id": "msg_1", "output_index": 0, "content_index": 0, "part": part}},
		{"response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": messageItem("completed")}},
		{"response.completed", map[string]any{"type": "response.completed", "response": response("completed", usage)}},
	}
	for i, e := range events {
		e.data["sequence_number"] = i
		b, _ := json.Marshal(e.data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.name, b)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

// messageItem is the one assistant message item a Responses answer holds.
func messageItem(status string) map[string]any {
	return map[string]any{
		"type": "message", "id": "msg_1", "role": "assistant", "status": status,
		"content": []any{map[string]any{"type": "output_text", "text": replyText, "annotations": []any{}}},
	}
}

// serveCountTokens answers the harness's token count so a run does not stall on it.
func (f *fake) serveCountTokens(w http.ResponseWriter, r *http.Request) {
	f.record(r, "model", "")
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": 3})
}

// rpcRequest is the slice of a JSON-RPC 2.0 request the fake reads. A request
// with no id is a notification and gets no body back.
type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		ProtocolVersion string `json:"protocolVersion"`
		Name            string `json:"name"`
		Arguments       struct {
			Text string `json:"text"`
		} `json:"arguments"`
	} `json:"params"`
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

// serveMCP is a stateless MCP server over streamable HTTP with one tool, echo.
func (f *fake) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// No server-initiated stream: the transport lets a server say so with 405.
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req rpcRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
		return
	}
	f.record(r, "mcp", req.Method)
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		version := req.Params.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "gatewayfake", "version": "0"},
		}))
	case "ping":
		writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{}))
	case "tools/list":
		writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{"tools": []map[string]any{{
			"name":        "echo",
			"description": "Returns the text it is given.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]string{"type": "string"}}},
		}}}))
	case "tools/call":
		if req.Params.Name != "echo" {
			writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "unknown tool"}})
			return
		}
		f.mu.Lock()
		f.seen.ToolCalls++
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{
			"content": []map[string]string{{"type": "text", "text": echoPrefix + req.Params.Arguments.Text}},
		}))
	default:
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
	}
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()
	// Bound before the log line, which names the bound address: with port 0 a caller learns the port from it.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("gatewayfake listening on %s", ln.Addr())
	log.Fatal(http.Serve(ln, (&fake{}).handler()))
}
