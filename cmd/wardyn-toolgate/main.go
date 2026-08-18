// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// wardyn-toolgate is the in-sandbox relay that turns Claude Code's tool-use
// permission prompts into Wardyn approvals. It is a minimal stdio MCP server
// exposing exactly one tool ("approve"); `claude -p --permission-prompt-tool
// mcp__gate__approve` calls it before every tool action the harness would
// otherwise have prompted a human for, and blocks until it answers.
//
// On each call it raises a `tool_call` approval through the egress proxy's
// brokered route (POST /wardyn/v1/approvals — tokenless from the sandbox; the
// proxy injects the run identity), then polls the approval until an operator
// decides in the Wardyn UI. APPROVED returns {"behavior":"allow"}; DENIED and
// EXPIRED return {"behavior":"deny"} — fail-closed, matching the approval
// FSM's own posture.
//
// The request/response contract with claude was pinned live against 2.1.234
// (C0 spike): the tool receives {"tool_name": ..., "input": {...}, "tool_use_id":
// ...} and must answer with a JSON-stringified PermissionResult in the tool
// result's text content. Two caveats carried from the same spike: the harness
// auto-approves its read-only-safe command class (echo and friends) without
// consulting any gate, and the agent must run with
// CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT=0 so a multi-hour human decision cannot
// trip the stdio idle timer (agent-run sets it).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "wardyn-toolgate:", err)
		os.Exit(1)
	}
}

// maxCmdBytes caps the command/summary text stored on the approval. The
// payload is decision context for a human, not an archive — the audit trail
// records the decision, the recording records the session.
const maxCmdBytes = 4096

func run(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("wardyn-toolgate", flag.ContinueOnError)
	// The proxy is the sandbox's one route out, so its address is already in
	// every agent's environment as the standard proxy variable.
	base := fs.String("base", os.Getenv("HTTP_PROXY"), "brokered API base URL (default: $HTTP_PROXY — the egress proxy)")
	poll := fs.Duration("poll", 2*time.Second, "approval poll interval")
	// A ceiling, not a timeout in the UX sense: claude blocks on this call for
	// as long as we keep polling, and the approval itself expires server-side
	// (FSM EXPIRED -> deny) long before this. This only bounds a gate whose
	// control plane stopped answering entirely.
	deadline := fs.Duration("deadline", 24*time.Hour, "give up (deny) after this long")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *base == "" {
		return fmt.Errorf("no -base and no HTTP_PROXY in the environment")
	}

	g := &gate{base: strings.TrimRight(*base, "/"), poll: *poll, deadline: *deadline,
		client: &http.Client{Timeout: 30 * time.Second}}
	return g.serve(in, out)
}

type gate struct {
	base     string
	poll     time.Duration
	deadline time.Duration
	client   *http.Client
}

// rpcMsg is the subset of JSON-RPC 2.0 the MCP stdio transport uses:
// newline-delimited messages, requests carrying an id, notifications without.
type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

func (g *gate) serve(in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	// A tools/call carrying a large Edit diff is one line; default 64KB is not
	// enough headroom.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(out)

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // not ours to diagnose — the client retries or dies loudly
		}
		if len(msg.ID) == 0 {
			continue // notification (notifications/initialized etc.) — nothing to answer
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = g.initializeResult(msg.Params)
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{
				"name":        "approve",
				"description": "Ask the Wardyn operator to approve this tool use",
				"inputSchema": map[string]any{"type": "object", "additionalProperties": true},
			}}}
		case "tools/call":
			result = g.decide(msg.Params)
		default:
			// Answer anything else with an empty result rather than an error:
			// hanging the client over an optional method it probes for
			// (ping, resources/list) would hang the agent.
			result = map[string]any{}
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result}); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (g *gate) initializeResult(params json.RawMessage) any {
	// Echo the client's protocol version — this gate has no version-dependent
	// behavior to negotiate.
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	if p.ProtocolVersion == "" {
		p.ProtocolVersion = "2024-11-05"
	}
	return map[string]any{
		"protocolVersion": p.ProtocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "gate", "version": "0.1.0"},
	}
}

// callArgs is what claude sends the permission-prompt tool (pinned live,
// 2.1.234): the gated tool's name and its full input.
type callArgs struct {
	ToolName  string          `json:"tool_name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

// decide raises the approval and blocks until it resolves. Every failure path
// returns deny — a gate that cannot reach its control plane must hold the
// door, not open it.
func (g *gate) decide(params json.RawMessage) any {
	var p struct {
		Arguments callArgs `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Arguments.ToolName == "" {
		return permissionResult(deny("malformed permission request"))
	}
	a := p.Arguments

	id, err := g.create(a)
	if err != nil {
		return permissionResult(deny("could not raise the approval: " + err.Error()))
	}

	end := time.Now().Add(g.deadline)
	for time.Now().Before(end) {
		state, err := g.state(id)
		if err == nil {
			switch state {
			case "APPROVED":
				// updatedInput must echo the input for the action to proceed
				// unchanged — an allow with no input is treated as a rewrite.
				return permissionResult(map[string]any{"behavior": "allow", "updatedInput": a.Input})
			case "DENIED":
				return permissionResult(deny("denied by the Wardyn operator"))
			case "EXPIRED":
				return permissionResult(deny("approval expired undecided — denied"))
			}
		}
		// PENDING, or a transient poll error: keep waiting. The proxy route is
		// the sandbox's own approval, so a persistent error ends at deadline.
		time.Sleep(g.poll)
	}
	return permissionResult(deny("approval wait deadline reached — denied"))
}

// create POSTs the tool_call approval through the brokered route and returns
// the approval id.
func (g *gate) create(a callArgs) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"kind":    "tool_call",
		"payload": map[string]any{"tool": a.ToolName, "cmd": summarize(a.ToolName, a.Input)},
	})
	resp, err := g.client.Post(g.base+"/wardyn/v1/approvals", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("approval create: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.ID == "" {
		return "", fmt.Errorf("approval create: no id in response")
	}
	return out.ID, nil
}

// state polls GET /wardyn/v1/approvals/{id} — the existing brokered read the
// egress WAIT flow already uses — and returns the approval's state string.
func (g *gate) state(id string) (string, error) {
	resp, err := g.client.Get(g.base + "/wardyn/v1/approvals/" + id)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var ap struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&ap); err != nil {
		return "", err
	}
	return ap.State, nil
}

// summarize renders the gated tool's input as the one-line decision context a
// human reads on the approval row. Bash gets its literal command; everything
// else gets its compact input JSON. Capped — context, not archive.
func summarize(tool string, input json.RawMessage) string {
	var cmd struct {
		Command string `json:"command"`
	}
	s := ""
	if json.Unmarshal(input, &cmd) == nil && cmd.Command != "" {
		s = cmd.Command
	} else {
		s = string(input)
	}
	if len(s) > maxCmdBytes {
		s = s[:maxCmdBytes] + " …[truncated]"
	}
	return s
}

func deny(msg string) map[string]any {
	return map[string]any{"behavior": "deny", "message": msg}
}

// permissionResult wraps a PermissionResult as the MCP tool result claude
// parses: JSON, stringified, in the first text content block.
func permissionResult(body map[string]any) any {
	b, _ := json.Marshal(body)
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}
}
