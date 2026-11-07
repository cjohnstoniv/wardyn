// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package modelusage

// anthropicUsage is the Messages API usage object. In a stream, message_start carries the input and cache
// counts and message_delta carries the cumulative output; a delta may repeat the input figures too.
type anthropicUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
}

type anthropicMessage struct {
	Model string          `json:"model"`
	Usage *anthropicUsage `json:"usage"`
}

func (p *parser) mergeAnthropic(u *anthropicUsage) {
	p.figures(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheCreationInputTokens, 0)
}

func (p *parser) anthropicBody(b []byte) {
	var m anthropicMessage
	if !p.decode(b, &m) {
		return
	}
	if m.Usage == nil {
		return
	}
	p.model(m.Model)
	p.mergeAnthropic(m.Usage)
	p.terminal()
}

func (p *parser) anthropicLine(l []byte) {
	data, ok := sseData(l)
	if !ok || len(data) == 0 {
		return
	}
	var ev struct {
		Type    string            `json:"type"`
		Message *anthropicMessage `json:"message"`
		Usage   *anthropicUsage   `json:"usage"`
	}
	if !p.decode(data, &ev) {
		return
	}
	p.anthropicEvent(ev.Type, ev.Message, ev.Usage)
}

func (p *parser) anthropicEvent(typ string, msg *anthropicMessage, usage *anthropicUsage) {
	switch typ {
	case "message_start":
		if msg != nil {
			p.model(msg.Model)
			if msg.Usage != nil {
				// The start's output_tokens is a placeholder: its figure is kept (max per field) but
				// only a message_delta's output_tokens satisfies the required output count.
				haveOut := p.haveOut
				p.mergeAnthropic(msg.Usage)
				p.haveOut = haveOut
			}
		}
	case "message_delta":
		if usage != nil {
			p.mergeAnthropic(usage)
			p.terminal()
		}
	}
}
