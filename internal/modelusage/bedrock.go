// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package modelusage

import (
	"encoding/json"
	"strconv"
)

// Bedrock InvokeModel reports token counts in these response headers, ahead of any model-specific body.
const (
	headerInputTokens  = "X-Amzn-Bedrock-Input-Token-Count"
	headerOutputTokens = "X-Amzn-Bedrock-Output-Token-Count"
)

// converseUsage is the Converse and ConverseStream usage object. inputTokens excludes cache tokens.
type converseUsage struct {
	InputTokens           int64 `json:"inputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	CacheReadInputTokens  int64 `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
}

func (p *parser) converseBody(b []byte) {
	var m struct {
		Usage *converseUsage `json:"usage"`
	}
	if !p.decode(b, &m) || m.Usage == nil {
		return
	}
	p.mergeConverse(m.Usage)
	p.final = true
}

func (p *parser) mergeConverse(u *converseUsage) {
	p.merge(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheWriteInputTokens, 0)
}

// converseFrame reads event frames; only the metadata event carries usage. The payload is the event's own
// object, but a wrapper keyed "metadata" is accepted too.
func (p *parser) converseFrame(f frame) {
	if f.headers[":message-type"] != "event" {
		return
	}
	var m struct {
		Usage    *converseUsage `json:"usage"`
		Metadata *struct {
			Usage *converseUsage `json:"usage"`
		} `json:"metadata"`
	}
	if !p.decode(f.payload, &m) {
		return
	}
	u := m.Usage
	if u == nil && m.Metadata != nil {
		u = m.Metadata.Usage
	}
	if u != nil {
		p.mergeConverse(u)
		p.final = true
	}
}

// invocationMetrics is appended by Bedrock to the last body or stream chunk of an InvokeModel call.
type invocationMetrics struct {
	InputTokenCount  int64 `json:"inputTokenCount"`
	OutputTokenCount int64 `json:"outputTokenCount"`
}

// invokeNative holds what a native model body can say about usage: the Anthropic usage object and the
// Bedrock invocation metrics, either of which may be present.
type invokeNative struct {
	Type    string             `json:"type"`
	Usage   *anthropicUsage    `json:"usage"`
	Message *anthropicMessage  `json:"message"`
	Model   string             `json:"model"`
	Metrics *invocationMetrics `json:"amazon-bedrock-invocationMetrics"`
}

func (p *parser) mergeMetrics(m *invocationMetrics) {
	p.merge(m.InputTokenCount, m.OutputTokenCount, 0, 0, 0)
	p.final = true
}

// invokeBody: headers first, then the native body. Per field the larger figure wins, so a header and the
// body agreeing is not double counting.
func (p *parser) invokeBody(b []byte) {
	if h := p.o.Header; h != nil {
		in, okIn := headerCount(h.Get(headerInputTokens))
		out, okOut := headerCount(h.Get(headerOutputTokens))
		if okIn || okOut {
			p.merge(in, out, 0, 0, 0)
			p.final = true
		}
	}
	var n invokeNative
	if json.Unmarshal(b, &n) != nil {
		// Headers are enough for a counted call; a body that is not JSON only matters without them.
		if !p.final {
			p.fail(ReasonParseError)
		}
		return
	}
	p.model(n.Model)
	if n.Usage != nil {
		p.mergeAnthropic(n.Usage)
		p.final = true
	}
	if n.Metrics != nil {
		p.mergeMetrics(n.Metrics)
	}
}

func headerCount(v string) (int64, bool) {
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil && n >= 0
}

// invokeFrame reads InvokeModelWithResponseStream chunk events: each payload is {"bytes": <base64>} and the
// decoded bytes are the model's own stream event, with the invocation metrics on the last one.
func (p *parser) invokeFrame(f frame) {
	if f.headers[":message-type"] != "event" {
		return
	}
	var c struct {
		Bytes []byte `json:"bytes"` // encoding/json decodes the base64
	}
	if !p.decode(f.payload, &c) {
		return
	}
	var n invokeNative
	if len(c.Bytes) == 0 || !p.decode(c.Bytes, &n) {
		return
	}
	p.anthropicEvent(n.Type, n.Message, n.Usage)
	if n.Metrics != nil {
		p.mergeMetrics(n.Metrics)
	}
}
