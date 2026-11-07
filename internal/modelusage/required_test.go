// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package modelusage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// A call is counted only when its required input and output figures arrived. A missing or null one is
// never read as zero; an explicit zero is a figure.
func TestRequiredCountsMustArrive(t *testing.T) {
	type dialect struct {
		name     string
		shape    Shape
		opts     Options
		inKey    string
		outKey   string
		wrap     func(usage string) []byte
		optional string // a figure the dialect may omit; the "only output" case then counts
	}
	body := func(u string) []byte { return []byte(`{"model":"m","usage":` + u + `}`) }
	chatStream := func(u string) []byte {
		return []byte("data: {\"model\":\"m\",\"choices\":[],\"usage\":" + u + "}\n\ndata: [DONE]\n\n")
	}
	responsesStream := func(u string) []byte {
		return []byte("data: {\"type\":\"response.completed\",\"response\":{\"model\":\"m\",\"usage\":" + u + "}}\n\n")
	}
	anthropicStream := func(u string) []byte {
		return []byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":" + u + "}}\n\n" +
			"data: {\"type\":\"message_delta\",\"usage\":" + u + "}\n\n")
	}
	ollama := func(u string) []byte {
		var o map[string]json.RawMessage
		_ = json.Unmarshal([]byte(u), &o)
		o["model"], o["done"] = json.RawMessage(`"m"`), json.RawMessage(`true`)
		b, _ := json.Marshal(o)
		return b
	}
	dialects := []dialect{
		{name: "anthropic body", shape: ShapeAnthropicMessages, inKey: "input_tokens", outKey: "output_tokens", wrap: body},
		{name: "anthropic stream", shape: ShapeAnthropicMessages, opts: Options{Stream: true}, inKey: "input_tokens", outKey: "output_tokens", wrap: anthropicStream},
		{name: "openai chat body", shape: ShapeOpenAIChat, inKey: "prompt_tokens", outKey: "completion_tokens", wrap: body},
		{name: "openai chat stream", shape: ShapeOpenAIChat, opts: Options{Stream: true, IncludeUsage: true}, inKey: "prompt_tokens", outKey: "completion_tokens", wrap: chatStream},
		{name: "openai responses body", shape: ShapeOpenAIResponses, inKey: "input_tokens", outKey: "output_tokens", wrap: body},
		{name: "openai responses stream", shape: ShapeOpenAIResponses, opts: Options{Stream: true}, inKey: "input_tokens", outKey: "output_tokens", wrap: responsesStream},
		{name: "bedrock converse body", shape: ShapeBedrockConverse, inKey: "inputTokens", outKey: "outputTokens", wrap: body},
		{name: "bedrock converse stream", shape: ShapeBedrockConverseStream, inKey: "inputTokens", outKey: "outputTokens",
			wrap: func(u string) []byte { return encodeFrame("metadata", "event", []byte(`{"usage":`+u+`}`)) }},
		{name: "bedrock invoke body (anthropic usage)", shape: ShapeBedrockInvoke, inKey: "input_tokens", outKey: "output_tokens", wrap: body},
		{name: "bedrock invoke body (metrics)", shape: ShapeBedrockInvoke, inKey: "inputTokenCount", outKey: "outputTokenCount",
			wrap: func(u string) []byte { return []byte(`{"amazon-bedrock-invocationMetrics":` + u + `}`) }},
		{name: "bedrock invoke stream (metrics)", shape: ShapeBedrockInvokeStream, inKey: "inputTokenCount", outKey: "outputTokenCount",
			wrap: func(u string) []byte { return chunk(`{"outputText":"a","amazon-bedrock-invocationMetrics":` + u + `}`) }},
		{name: "ollama", shape: ShapeOllama, inKey: "prompt_eval_count", outKey: "eval_count", optional: "prompt_eval_count",
			wrap: ollama},
	}
	obj := func(fields ...string) string {
		s := "{"
		for i, f := range fields {
			if i > 0 {
				s += ","
			}
			s += f
		}
		return s + "}"
	}
	for _, d := range dialects {
		in := func(v string) string { return fmt.Sprintf("%q:%s", d.inKey, v) }
		out := func(v string) string { return fmt.Sprintf("%q:%s", d.outKey, v) }
		type tc struct {
			name        string
			usage       string
			counted     bool
			wantIn, out int64
		}
		cases := []tc{
			{name: "empty usage", usage: obj()},
			{name: "null output", usage: obj(in("17"), out("null"))},
			{name: "only input", usage: obj(in("17"))},
			{name: "explicit zero input", usage: obj(in("0"), out("5")), counted: true, wantIn: 0, out: 5},
			{name: "both figures", usage: obj(in("17"), out("9")), counted: true, wantIn: 17, out: 9},
		}
		if d.optional == d.inKey {
			cases = append(cases,
				tc{name: "null input is allowed", usage: obj(in("null"), out("5")), counted: true, out: 5},
				tc{name: "only output is allowed", usage: obj(out("5")), counted: true, out: 5})
		} else {
			cases = append(cases,
				tc{name: "null input", usage: obj(in("null"), out("5"))},
				tc{name: "only output", usage: obj(out("5"))})
		}
		for _, c := range cases {
			got := run(d.shape, d.opts, d.wrap(c.usage), 0)
			if c.counted {
				if got.State != StateCounted || got.Input != c.wantIn || got.Output != c.out {
					t.Errorf("%s / %s: got %+v, want counted %d in %d out", d.name, c.name, got, c.wantIn, c.out)
				}
				continue
			}
			if got.State != StateUncounted || got.Reason != ReasonNoUsage || got.Input != 0 || got.Output != 0 {
				t.Errorf("%s / %s: got %+v, want uncounted/no_usage", d.name, c.name, got)
			}
		}
	}
}

// Anthropic figures can arrive across message_start and message_delta; the call counts once both have.
func TestAnthropicStreamRequiredAcrossEvents(t *testing.T) {
	stream := func(start, delta string) []byte {
		return []byte("data: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":" + start + "}}\n\n" +
			"data: {\"type\":\"message_delta\",\"usage\":" + delta + "}\n\n")
	}
	o := Options{Stream: true}
	expect(t, run(ShapeAnthropicMessages, o, stream(`{"input_tokens":12}`, `{"output_tokens":5}`), 0),
		Usage{Model: "m", Input: 12, Output: 5, State: StateCounted})
	expect(t, run(ShapeAnthropicMessages, o, stream(`{"input_tokens":12}`, `{}`), 0),
		Usage{Model: "m", State: StateUncounted, Reason: ReasonNoUsage})
	expect(t, run(ShapeAnthropicMessages, o, stream(`{}`, `{"output_tokens":5}`), 0),
		Usage{Model: "m", State: StateUncounted, Reason: ReasonNoUsage})
}

// Bedrock InvokeModel headers: one header alone is not a count; both, with a zero among them, are.
func TestBedrockInvokeLoneHeaderIsUncounted(t *testing.T) {
	hdr := func(in, out string) Options {
		h := http.Header{}
		if in != "" {
			h.Set("X-Amzn-Bedrock-Input-Token-Count", in)
		}
		if out != "" {
			h.Set("X-Amzn-Bedrock-Output-Token-Count", out)
		}
		return Options{Header: h}
	}
	for _, o := range []Options{hdr("17", ""), hdr("", "9")} {
		got := run(ShapeBedrockInvoke, o, []byte(`{"results":[]}`), 0)
		expect(t, got, Usage{State: StateUncounted, Reason: ReasonNoUsage})
	}
	// A lone header completed by the body's own figure counts.
	got := run(ShapeBedrockInvoke, hdr("17", ""), []byte(`{"usage":{"output_tokens":9}}`), 0)
	expect(t, got, Usage{Input: 17, Output: 9, State: StateCounted})
	expect(t, run(ShapeBedrockInvoke, hdr("0", "5"), []byte(`{"results":[]}`), 0), Usage{Output: 5, State: StateCounted})
}

// message_start's output_tokens is a placeholder: only a non-null output_tokens on a message_delta is the
// final count. Input and cache figures still carry over from message_start.
func TestAnthropicOutputCountsOnlyFromTheTerminalDelta(t *testing.T) {
	const start = `{"type":"message_start","message":{"model":"m","usage":{"input_tokens":12,"output_tokens":1}}}`
	delta := func(u string) string { return `{"type":"message_delta","usage":` + u + `}` }
	sse := func(events ...string) []byte {
		var b []byte
		for _, e := range events {
			b = append(b, "data: "...)
			b = append(b, e...)
			b = append(b, "\n\n"...)
		}
		return b
	}
	chunks := func(events ...string) []byte {
		var b []byte
		for _, e := range events {
			b = append(b, chunk(e)...)
		}
		return b
	}
	paths := []struct {
		name  string
		shape Shape
		opts  Options
		build func(...string) []byte
	}{
		{"anthropic stream", ShapeAnthropicMessages, Options{Stream: true}, sse},
		{"bedrock invoke stream", ShapeBedrockInvokeStream, Options{}, chunks},
	}
	for _, p := range paths {
		for _, u := range []string{`{}`, `{"output_tokens":null}`} {
			got := run(p.shape, p.opts, p.build(start, delta(u)), 0)
			expect(t, got, Usage{Model: "m", State: StateUncounted, Reason: ReasonNoUsage})
		}
		got := run(p.shape, p.opts, p.build(start, delta(`{"output_tokens":0}`)), 0)
		expect(t, got, Usage{Model: "m", Input: 12, Output: 1, State: StateCounted})
	}
}
