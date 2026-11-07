// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package modelusage

import (
	"encoding/json"
	"strconv"
)

// Bedrock InvokeModel reports token counts in these response headers, ahead of any model-specific body.
// The two names below are not in AWS's public API reference: the InvokeModel and
// InvokeModelWithResponseStream pages (docs.aws.amazon.com/bedrock/latest/APIReference) list only the
// content-type, performance-config and service-tier response headers. They are the names Bedrock is
// seen to send; a wrong name only leaves the call uncounted (no usage), never miscounted.
const (
	headerInputTokens  = "X-Amzn-Bedrock-Input-Token-Count"
	headerOutputTokens = "X-Amzn-Bedrock-Output-Token-Count"
)

// converseUsage is the Converse and ConverseStream usage object. inputTokens excludes cache tokens.
type converseUsage struct {
	InputTokens           *int64 `json:"inputTokens"`
	OutputTokens          *int64 `json:"outputTokens"`
	CacheReadInputTokens  int64  `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int64  `json:"cacheWriteInputTokens"`
}

func (p *parser) converseBody(b []byte) {
	var m struct {
		Usage *converseUsage `json:"usage"`
	}
	if !p.decode(b, &m) || m.Usage == nil {
		return
	}
	p.mergeConverse(m.Usage)
	p.terminal()
}

func (p *parser) mergeConverse(u *converseUsage) {
	p.figures(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheWriteInputTokens, 0)
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
		p.terminal()
	}
}

// invocationMetrics is appended by Bedrock to the last body or stream chunk of an InvokeModel call.
// The key "amazon-bedrock-invocationMetrics" and these two field names are not in AWS's public API
// reference or the model parameter pages (the Titan pages document inputTextTokenCount instead); a
// wrong name only leaves the call uncounted.
type invocationMetrics struct {
	InputTokenCount  *int64 `json:"inputTokenCount"`
	OutputTokenCount *int64 `json:"outputTokenCount"`
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
	p.figures(m.InputTokenCount, m.OutputTokenCount, 0, 0, 0)
	p.terminal()
}

// invokeBody: headers first, then the native body. Per field the larger figure wins, so a header and the
// body agreeing is not double counting. The figures may arrive split between headers and body; the call is
// counted only when both the input and the output have.
func (p *parser) invokeBody(b []byte) {
	if h := p.o.Header; h != nil {
		in, inSet, inOK := headerCount(h.Get(headerInputTokens))
		out, outSet, outOK := headerCount(h.Get(headerOutputTokens))
		if (inSet && !inOK) || (outSet && !outOK) {
			// A figure that is there but cannot be read is never counted as zero.
			p.fail(ReasonParseError)
			return
		}
		if inSet || outSet {
			p.figures(setOnly(in, inSet), setOnly(out, outSet), 0, 0, 0)
		}
	}
	var n invokeNative
	if json.Unmarshal(b, &n) != nil {
		// Headers can be enough for a counted call; a body that is not JSON only matters without them.
		if p.seen {
			p.terminal()
		} else {
			p.fail(ReasonParseError)
		}
		return
	}
	p.model(n.Model)
	if n.Usage != nil {
		p.mergeAnthropic(n.Usage)
	}
	if n.Metrics != nil {
		p.figures(n.Metrics.InputTokenCount, n.Metrics.OutputTokenCount, 0, 0, 0)
	}
	if p.seen {
		p.terminal()
	}
}

// setOnly points at n when it was set, else nil.
func setOnly(n int64, set bool) *int64 {
	if !set {
		return nil
	}
	return &n
}

// headerCount reads one token-count header: set is false for an absent (empty) header, ok is false for
// a present value that is not a non-negative integer.
func headerCount(v string) (n int64, set, ok bool) {
	if v == "" {
		return 0, false, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, true, err == nil && n >= 0
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
