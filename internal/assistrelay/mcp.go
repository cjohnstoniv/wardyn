// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistrelay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sync"
)

// toolName is the shape of a Wardyn tool name, here and at the proxy.
var toolName = regexp.MustCompile(`^[a-z_]{1,32}$`)

// mcpProtocol is the protocol version answered when the harness names none.
const mcpProtocol = "2025-06-18"

// mcpLineMax bounds one MCP message: a tool call's arguments are capped at 256 KiB by the proxy.
const mcpLineMax = 1 << 20

// Tool is one of Wardyn's read-only tools as the server lists it.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolServer is `wardyn-assist tools`: a stdio MCP server named wardyn. No tool logic runs in the sandbox:
// each call goes to the server through the proxy.
type ToolServer struct {
	API *API
	// Session is the session this process speaks for, fixed when it starts. It is never read again from a
	// file or anything else that can change: a reset replaces the process, it never re-tags one.
	Session string
	// Log takes counts and states only.
	Log io.Writer
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads the tool list once, then answers MCP requests on in until it closes. It returns an error, and
// serves nothing, when the server answers no usable tool list.
func (s *ToolServer) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	tools, status, err := s.API.Tools(ctx)
	if err != nil {
		fmt.Fprintf(s.Log, "wardyn-assist tools: tool list unavailable (status %d)\n", status)
		return err
	}
	var mu sync.Mutex
	enc := json.NewEncoder(out)
	reply := func(id json.RawMessage, result any, rerr *rpcError) {
		msg := map[string]any{"jsonrpc": "2.0", "id": id}
		if rerr != nil {
			msg["error"] = rerr
		} else {
			msg["result"] = result
		}
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(msg)
	}
	var wg sync.WaitGroup
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), mcpLineMax)
	for sc.Scan() {
		var req rpcRequest
		if json.Unmarshal(sc.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue // not a request, or a notification: nothing to answer
		}
		switch req.Method {
		case "initialize":
			reply(req.ID, initializeResult(req.Params), nil)
		case "ping":
			reply(req.ID, map[string]any{}, nil)
		case "tools/list":
			reply(req.ID, map[string]any{"tools": mcpTools(tools)}, nil)
		case "tools/call":
			wg.Add(1)
			go func(req rpcRequest) {
				defer wg.Done()
				reply(req.ID, s.call(ctx, req.Params), nil)
			}(req)
		default:
			reply(req.ID, nil, &rpcError{Code: -32601, Message: "method not found"})
		}
	}
	wg.Wait()
	return sc.Err()
}

func initializeResult(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	if p.ProtocolVersion == "" {
		p.ProtocolVersion = mcpProtocol
	}
	return map[string]any{
		"protocolVersion": p.ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "wardyn", "version": "1"},
	}
}

func mcpTools(tools []Tool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": schema})
	}
	return out
}

// call forwards one tools/call as POST tools/<name> with this process's session id. The server's JSON comes
// back as text content; a refusal comes back as an error result carrying the server's message.
func (s *ToolServer) call(ctx context.Context, params json.RawMessage) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(params, &p) != nil || !toolName.MatchString(p.Name) {
		return toolError("unknown tool")
	}
	status, body, err := s.API.Call(ctx, p.Name, s.Session, p.Arguments)
	if err != nil {
		fmt.Fprintf(s.Log, "wardyn-assist tools: a call got no answer\n")
		return toolError("the tool call got no answer")
	}
	if status != 200 {
		fmt.Fprintf(s.Log, "wardyn-assist tools: a call was refused (status %d)\n", status)
		return toolError(serverMessage(status, body))
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(body)}}}
}

func toolError(msg string) map[string]any {
	return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": msg}}}
}

// serverMessage is the message of an error answer: its "error" or "message" field, else the status alone.
func serverMessage(status int, body []byte) string {
	var m struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &m) == nil {
		if m.Message != "" {
			return m.Message
		}
		if m.Error != "" {
			return m.Error
		}
	}
	return fmt.Sprintf("the tool call was refused (status %d)", status)
}
