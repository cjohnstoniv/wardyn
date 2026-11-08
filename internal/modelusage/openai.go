// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package modelusage

import "bytes"

// chatUsage is the Chat Completions usage object. prompt_tokens already includes cached tokens.
type chatUsage struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// responsesUsage is the Responses API usage object.
type responsesUsage struct {
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type chatEnvelope struct {
	Model string     `json:"model"`
	Usage *chatUsage `json:"usage"`
}

func (p *parser) mergeChat(e *chatEnvelope) {
	p.model(e.Model)
	if e.Usage == nil {
		return
	}
	u := e.Usage
	p.figures(u.PromptTokens, u.CompletionTokens, u.PromptTokensDetails.CachedTokens, 0, u.CompletionTokensDetails.ReasoningTokens)
	p.terminal()
}

func (p *parser) chatBody(b []byte) {
	var e chatEnvelope
	if p.decode(b, &e) {
		p.mergeChat(&e)
	}
}

// chatLine reads one chunk. Without stream_options.include_usage a stream has no usage chunk, so the call
// ends uncounted/stream_without_usage; the request is never rewritten to ask for one.
func (p *parser) chatLine(l []byte) {
	data, ok := sseData(l)
	if !ok || len(data) == 0 {
		return
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		p.done = true
		return
	}
	var e chatEnvelope
	if p.decode(data, &e) {
		p.mergeChat(&e)
	}
}

type responsesEnvelope struct {
	Model string          `json:"model"`
	Usage *responsesUsage `json:"usage"`
}

func (p *parser) mergeResponses(e *responsesEnvelope) {
	p.model(e.Model)
	if e.Usage == nil {
		return
	}
	u := e.Usage
	p.figures(u.InputTokens, u.OutputTokens, u.InputTokensDetails.CachedTokens, 0, u.OutputTokensDetails.ReasoningTokens)
	p.terminal()
}

func (p *parser) responsesBody(b []byte) {
	var e responsesEnvelope
	if p.decode(b, &e) {
		p.mergeResponses(&e)
	}
}

// responsesLine reads response.created (model) and the terminal response.completed / response.incomplete /
// response.failed events, whose response object carries usage when the model ran. A gateway that
// frames every stream as a chat stream (LiteLLM's proxy) ends it with data: [DONE], which is no event.
func (p *parser) responsesLine(l []byte) {
	data, ok := sseData(l)
	if !ok || len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var ev struct {
		Type     string             `json:"type"`
		Response *responsesEnvelope `json:"response"`
	}
	if !p.decode(data, &ev) || ev.Response == nil {
		return
	}
	switch ev.Type {
	case "response.created", "response.in_progress":
		p.model(ev.Response.Model)
	case "response.completed", "response.incomplete", "response.failed":
		p.ended = true
		p.mergeResponses(ev.Response)
	}
}
