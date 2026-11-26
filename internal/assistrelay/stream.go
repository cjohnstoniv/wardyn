// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package assistrelay runs inside an assistant run. One binary carries each person turn from Wardyn to the
// Claude Code harness, carries the harness's stream back to Wardyn as events, and serves Wardyn's read-only
// tools to the harness over stdio MCP. Every call leaves through the run's proxy, which adds the run token
// toward the control plane only. Nothing the person typed or the model wrote is logged.
package assistrelay

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Event kinds the relay posts. It never posts a proposal or a tool record: those are the server's.
const (
	kindState      = "state"
	kindText       = "text"
	kindToolCall   = "tool_call"
	kindToolResult = "tool_result"
	kindNotice     = "notice"
	kindTurnEnd    = "turn_end"
	kindError      = "error"
)

const (
	// summaryMax is the length, in characters, of a tool call's or a tool result's display summary.
	summaryMax = 200
	// toolPrefix is how the harness names a tool of the wardyn MCP server.
	toolPrefix = "mcp__wardyn__"
	// endConversation is the harness tool whose use ends the current turn, never the session.
	endConversation = "EndConversation"
)

// Event is one event the relay posts. The session id is added when it is posted.
type Event struct {
	Kind    string
	Payload map[string]any
}

// Usage is the harness's token counts for one turn, as its result line gives them.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// Step is what one line of the harness stream carries.
type Step struct {
	// Events are the line's text pieces, tool calls and tool results, in the order the line gives them. The
	// relay batches text events and posts the others as they are.
	Events []Event
	// ToolCalls counts the line's tool uses, an EndConversation use excluded.
	ToolCalls int
	// EndTurn is set by a result line and by an EndConversation tool use.
	EndTurn bool
	// EndConversation is set when the turn ends on an EndConversation tool use.
	EndConversation bool
	// Usage is set by a result line.
	Usage *Usage
	// Failed is set when a result line says the turn did not succeed.
	Failed bool
	// HarnessSession is the harness's own session id, from its init line.
	HarnessSession string
}

// Parser turns harness stream-json lines into steps. It remembers the name of each tool use, so a result can
// say which tool it answers.
type Parser struct {
	names map[string]string
}

// NewParser returns a parser with no tool uses seen.
func NewParser() *Parser { return &Parser{names: map[string]string{}} }

type streamLine struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	SessionID string          `json:"session_id"`
	Message   json.RawMessage `json:"message"` // an object on assistant and user lines; a string on some system lines
	Usage     *Usage          `json:"usage"`
	IsError   bool            `json:"is_error"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// Line parses one line of the harness stream. A blank line is an empty step; a line that is not JSON is an
// error, which the caller counts and skips.
func (p *Parser) Line(line []byte) (Step, error) {
	var st Step
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return st, nil
	}
	var l streamLine
	if err := json.Unmarshal(line, &l); err != nil {
		return st, err
	}
	switch l.Type {
	case "system":
		if l.Subtype == "init" {
			st.HarnessSession = l.SessionID
		}
	case "assistant":
		for _, b := range blocks(l.Message) {
			p.assistantBlock(&st, b)
		}
	case "user":
		for _, b := range blocks(l.Message) {
			if b.Type == "tool_result" {
				st.Events = append(st.Events, Event{Kind: kindToolResult, Payload: map[string]any{
					"name": p.names[b.ToolUseID], "ok": !b.IsError, "summary": summarize(resultText(b.Content)),
				}})
			}
		}
	case "result":
		st.EndTurn, st.Usage = true, l.Usage
		st.Failed = l.IsError || l.Subtype != "success"
	}
	return st, nil
}

func (p *Parser) assistantBlock(st *Step, b contentBlock) {
	switch b.Type {
	case "text":
		if b.Text != "" {
			st.Events = append(st.Events, Event{Kind: kindText, Payload: map[string]any{"text": b.Text}})
		}
	case "tool_use":
		if b.Name == endConversation {
			st.EndTurn, st.EndConversation = true, true
			return
		}
		name := strings.TrimPrefix(b.Name, toolPrefix)
		p.names[b.ID] = name
		st.ToolCalls++
		st.Events = append(st.Events, Event{Kind: kindToolCall, Payload: map[string]any{
			"name": name, "summary": summarize(compact(b.Input)),
		}})
	}
}

// blocks reads a message's content as blocks. A message that is not an object, and content given as a plain
// string, carry none.
func blocks(message json.RawMessage) []contentBlock {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if len(message) == 0 || message[0] != '{' || json.Unmarshal(message, &m) != nil {
		return nil
	}
	var out []contentBlock
	if len(m.Content) == 0 || m.Content[0] != '[' || json.Unmarshal(m.Content, &out) != nil {
		return nil
	}
	return out
}

// resultText is a tool result's content as text: a string as it is, a list of blocks by their text.
func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []contentBlock
	if json.Unmarshal(raw, &parts) == nil {
		texts := make([]string, 0, len(parts))
		for _, b := range parts {
			if b.Type == "text" {
				texts = append(texts, b.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return compact(raw)
}

func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

// summarize cuts text to its first summaryMax characters.
func summarize(s string) string {
	if utf8.RuneCountInString(s) <= summaryMax {
		return s
	}
	return string([]rune(s)[:summaryMax])
}
