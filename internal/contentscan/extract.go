// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package contentscan

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Channel identifies the wire schema of a request body so the right extractor
// is used. Detectors are channel-agnostic; only extraction is schema-aware.
type Channel string

const (
	// ChannelAnthropicMessages is an Anthropic POST /v1/messages request body.
	ChannelAnthropicMessages Channel = "anthropic.messages"
	// ChannelOpenAIChat is an OpenAI /v1/chat/completions request body.
	ChannelOpenAIChat Channel = "openai.chat"
	// ChannelGeneric is an arbitrary connector body: JSON => walk all string
	// leaves; otherwise scan the whole body as raw text.
	ChannelGeneric Channel = "generic"
	// ChannelMCP is an MCP/tool-gateway JSON-RPC body (walked like generic JSON).
	ChannelMCP Channel = "mcp.jsonrpc"
)

// Extract walks body for the given channel and yields text spans. It returns an
// error only when the body is malformed for that channel (caller treats this as
// a parse_error skip). Unknown JSON fields are ignored (forward-compatible).
func Extract(channel Channel, body []byte, yield func(Span)) error {
	switch channel {
	case ChannelAnthropicMessages:
		return extractAnthropicMessages(body, yield)
	case ChannelOpenAIChat:
		return extractOpenAIChat(body, yield)
	case ChannelGeneric, ChannelMCP:
		return extractGeneric(body, yield)
	default:
		return fmt.Errorf("contentscan: unsupported channel %q", channel)
	}
}

// extractGeneric walks an arbitrary connector body: if it is valid JSON, every
// string leaf is yielded (covers JSON connectors and MCP/JSON-RPC); otherwise the
// whole body is yielded as one raw-text span (bounded by the engine's per-span cap).
func extractGeneric(body []byte, yield func(Span)) error {
	if json.Valid(body) {
		walkJSONStrings(json.RawMessage(body), "$", yield)
		return nil
	}
	yield(Span{FieldPath: "$body", Text: string(body)})
	return nil
}

// extractAnthropicAttachments decodes base64 image/document attachment bytes in
// the newest message and yields the decoded content as spans (opt-in; off by
// default). Best-effort: undecodable/oversized blocks are skipped.
func extractAnthropicAttachments(body []byte, yield func(Span)) {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
		return
	}
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(req.Messages[len(req.Messages)-1], &m) != nil {
		return
	}
	raw := bytes.TrimSpace(m.Content)
	if len(raw) == 0 || raw[0] != '[' {
		return
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	for i, b := range blocks {
		var blk struct {
			Type   string `json:"type"`
			Source struct {
				Type string `json:"type"`
				Data string `json:"data"`
			} `json:"source"`
		}
		if json.Unmarshal(b, &blk) != nil {
			continue
		}
		if (blk.Type == "image" || blk.Type == "document") && blk.Source.Type == "base64" && blk.Source.Data != "" {
			if dec, err := base64.StdEncoding.DecodeString(blk.Source.Data); err == nil && len(dec) > 0 {
				yield(Span{
					FieldPath: fmt.Sprintf("messages[%d].content[%d].source.data(decoded)", len(req.Messages)-1, i),
					Text:      string(dec),
				})
			}
		}
	}
}

// extractOpenAIChat yields the text the agent is sending THIS turn for an OpenAI
// /v1/chat/completions request: the LAST message's content (string or text
// parts) plus its tool_call function arguments (a JSON string we walk). Same
// newest-message heuristic and known false-negatives as the Anthropic walker.
func extractOpenAIChat(body []byte, yield func(Span)) error {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("contentscan: parse openai.chat: %w", err)
	}
	n := len(req.Messages)
	if n == 0 {
		return nil
	}
	var m struct {
		Content   json.RawMessage `json:"content"`
		ToolCalls []struct {
			Function struct {
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(req.Messages[n-1], &m); err != nil {
		return fmt.Errorf("contentscan: parse openai message: %w", err)
	}
	prefix := fmt.Sprintf("messages[%d]", n-1)
	walkOpenAIContent(m.Content, prefix+".content", yield)
	for i, tc := range m.ToolCalls {
		args := tc.Function.Arguments
		if args == "" {
			continue
		}
		path := fmt.Sprintf("%s.tool_calls[%d].function.arguments", prefix, i)
		// arguments is a JSON-encoded string; walk its string leaves, or scan it
		// verbatim if it is not valid JSON.
		if json.Valid([]byte(args)) {
			walkJSONStrings(json.RawMessage(args), path, yield)
		} else {
			yield(Span{FieldPath: path, Text: args})
		}
	}
	return nil
}

// walkOpenAIContent handles an OpenAI message content that is a string or an
// array of parts (text parts yielded; image_url/other parts skipped).
func walkOpenAIContent(raw json.RawMessage, path string, yield func(Span)) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			yield(Span{FieldPath: path, Text: s})
		}
	case '[':
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) != nil {
			return
		}
		for i, p := range parts {
			if p.Text != "" {
				yield(Span{FieldPath: fmt.Sprintf("%s[%d].text", path, i), Text: p.Text})
			}
		}
	}
}

// extractAnthropicMessages yields the text the agent is sending THIS turn: the
// system prompt and the LAST message in messages[] (Anthropic orders messages
// oldest->newest, so the newest turn — where a freshly-pasted secret or a
// tool_result of a just-cat'd file lands — is the final element). Scanning only
// the newest message bounds cost while covering the primary inadvertent-leak
// paths. KNOWN false-negatives (documented in threatmodel §5.1a): a first request
// that seeds multiple prior messages, a turn that appends >1 new message, a secret
// split across turns, and base64 image/document attachment bytes (skipped here).
func extractAnthropicMessages(body []byte, yield func(Span)) error {
	var req struct {
		System   json.RawMessage   `json:"system"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return fmt.Errorf("contentscan: parse anthropic.messages: %w", err)
	}

	if len(bytes.TrimSpace(req.System)) > 0 {
		walkTextOrBlocks(req.System, "system", yield)
	}

	if n := len(req.Messages); n > 0 {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(req.Messages[n-1], &m); err != nil {
			return fmt.Errorf("contentscan: parse last message: %w", err)
		}
		walkTextOrBlocks(m.Content, fmt.Sprintf("messages[%d].content", n-1), yield)
	}
	return nil
}

// walkTextOrBlocks handles a field that is either a JSON string (the content
// shorthand) or an array of content blocks.
func walkTextOrBlocks(raw json.RawMessage, path string, yield func(Span)) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	switch raw[0] {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			yield(Span{FieldPath: path, Text: s})
		}
	case '[':
		var blocks []json.RawMessage
		if json.Unmarshal(raw, &blocks) != nil {
			return
		}
		for i, b := range blocks {
			walkBlock(b, fmt.Sprintf("%s[%d]", path, i), yield)
		}
	}
}

// walkBlock yields the scannable text of one content block: text blocks, the
// nested content of a tool_result, and every string leaf of a tool_use input.
func walkBlock(b json.RawMessage, path string, yield func(Span)) {
	var hdr struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"`
		Input   json.RawMessage `json:"input"`
	}
	if json.Unmarshal(b, &hdr) != nil {
		return
	}
	switch hdr.Type {
	case "text":
		if hdr.Text != "" {
			yield(Span{FieldPath: path + ".text", Text: hdr.Text})
		}
	case "tool_result":
		if len(bytes.TrimSpace(hdr.Content)) > 0 {
			walkTextOrBlocks(hdr.Content, path+".content", yield)
		}
	case "tool_use":
		if len(bytes.TrimSpace(hdr.Input)) > 0 {
			walkJSONStrings(hdr.Input, path+".input", yield)
		}
	default:
		// image / document / unknown: skip binary source.data; still scan a
		// stray text field if one is present (forward-compatible).
		if hdr.Text != "" {
			yield(Span{FieldPath: path + ".text", Text: hdr.Text})
		}
	}
}

// walkJSONStrings yields every string leaf of an arbitrary JSON value (used for
// tool_use.input, whose shape is tool-defined).
func walkJSONStrings(raw json.RawMessage, path string, yield func(Span)) {
	var v interface{}
	if json.Unmarshal(raw, &v) != nil {
		return
	}
	walkValue(v, path, yield)
}

func walkValue(v interface{}, path string, yield func(Span)) {
	switch t := v.(type) {
	case string:
		if t != "" {
			yield(Span{FieldPath: path, Text: t})
		}
	case map[string]interface{}:
		for k, val := range t {
			walkValue(val, path+"."+k, yield)
		}
	case []interface{}:
		for i, val := range t {
			walkValue(val, fmt.Sprintf("%s[%d]", path, i), yield)
		}
	}
}
