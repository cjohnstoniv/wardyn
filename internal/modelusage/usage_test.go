// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package modelusage

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// run feeds data in chunk-sized writes and returns the result. A chunk of 0 means one write.
func run(shape Shape, o Options, data []byte, chunk int) Usage {
	p := NewParser(shape, o)
	if chunk <= 0 {
		chunk = len(data) + 1
	}
	for i := 0; i < len(data); i += chunk {
		p.Write(data[i:min(i+chunk, len(data))])
	}
	return p.Result()
}

// runAllChunks proves the result does not depend on how the body was split.
func runAllChunks(t *testing.T, shape Shape, o Options, data []byte) Usage {
	t.Helper()
	want := run(shape, o, data, 0)
	for _, c := range []int{1, 3, 7, 64, 4096} {
		if got := run(shape, o, data, c); got != want {
			t.Fatalf("chunk %d: got %+v, whole-body %+v", c, got, want)
		}
	}
	return want
}

func expect(t *testing.T, got, want Usage) {
	t.Helper()
	if got != want {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestAnthropicStreamCounted(t *testing.T) {
	// Doc example: message_start input 25, message_delta output 15 (cumulative; the start's 1 is superseded).
	got := runAllChunks(t, ShapeAnthropicMessages, Options{Stream: true}, fixture(t, "anthropic_stream.sse"))
	expect(t, got, Usage{Model: "claude-opus-5-5", Input: 25, Output: 15, State: StateCounted})
}

func TestAnthropicStreamMaxPerField(t *testing.T) {
	got := runAllChunks(t, ShapeAnthropicMessages, Options{Stream: true}, fixture(t, "anthropic_stream_cache.sse"))
	expect(t, got, Usage{Model: "claude-opus-5-5", Input: 10682, Output: 510, State: StateCounted})
}

func TestAnthropicStreamCacheFields(t *testing.T) {
	// Field names per https://platform.claude.com/docs/en/api/messages/create (Usage): cache_creation_input_tokens,
	// cache_read_input_tokens.
	s := `data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":5,"cache_creation_input_tokens":100,"cache_read_input_tokens":200,"output_tokens":1}}}` + "\n" +
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":9}}` + "\n"
	got := run(ShapeAnthropicMessages, Options{Stream: true}, []byte(s), 0)
	expect(t, got, Usage{Model: "m", Input: 5, Output: 9, CacheRead: 200, CacheWrite: 100, State: StateCounted})
}

func TestAnthropicStreamCutIsPartial(t *testing.T) {
	full := fixture(t, "anthropic_stream.sse")
	cut := full[:bytes.Index(full, []byte("event: message_delta"))]
	got := runAllChunks(t, ShapeAnthropicMessages, Options{Stream: true}, cut)
	expect(t, got, Usage{Model: "claude-opus-5-5", Input: 25, Output: 1, State: StatePartial, Reason: ReasonStreamCut})
}

func TestAnthropicStreamCutBeforeStartIsUncounted(t *testing.T) {
	got := run(ShapeAnthropicMessages, Options{Stream: true}, []byte("event: ping\ndata: {\"type\": \"ping\"}\n\n"), 0)
	expect(t, got, Usage{State: StateUncounted, Reason: ReasonStreamCut})
}

func TestAnthropicLastLineWithoutNewline(t *testing.T) {
	s := `data: {"type":"message_delta","delta":{},"usage":{"input_tokens":2,"output_tokens":3}}`
	got := run(ShapeAnthropicMessages, Options{Stream: true}, []byte(s), 0)
	expect(t, got, Usage{Input: 2, Output: 3, State: StateCounted})
}

func TestAnthropicNonStream(t *testing.T) {
	// Message object per https://platform.claude.com/docs/en/api/messages/create: usage sits beside model.
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":"end_turn","usage":{"input_tokens":2095,"cache_creation_input_tokens":10,"cache_read_input_tokens":20,"output_tokens":503}}`
	got := runAllChunks(t, ShapeAnthropicMessages, Options{}, []byte(body))
	expect(t, got, Usage{Model: "claude-opus-5-5", Input: 2095, Output: 503, CacheRead: 20, CacheWrite: 10, State: StateCounted})
}

func TestNonStreamWithoutUsageIsUncounted(t *testing.T) {
	for _, shape := range []Shape{ShapeAnthropicMessages, ShapeOpenAIChat, ShapeOpenAIResponses, ShapeBedrockConverse} {
		got := run(shape, Options{}, []byte(`{"id":"x","model":"m","type":"error","error":{"message":"overloaded"}}`), 0)
		if got.State != StateUncounted || got.Reason != ReasonNoUsage {
			t.Fatalf("%s: %+v", shape, got)
		}
	}
}

func TestOpenAIChatStreamWithIncludeUsage(t *testing.T) {
	got := runAllChunks(t, ShapeOpenAIChat, Options{Stream: true, IncludeUsage: true}, fixture(t, "openai_chat_stream_usage.sse"))
	expect(t, got, Usage{Model: "gpt-5-2026-01-01", Input: 120, Output: 40, CacheRead: 64, Reasoning: 12, State: StateCounted})
}

func TestOpenAIChatStreamWithoutIncludeUsageIsUncounted(t *testing.T) {
	got := runAllChunks(t, ShapeOpenAIChat, Options{Stream: true}, fixture(t, "openai_chat_stream_nousage.sse"))
	expect(t, got, Usage{Model: "gpt-5-2026-01-01", State: StateUncounted, Reason: ReasonStreamWithoutUsage})
}

func TestOpenAIChatIncludeUsageButNoUsageChunk(t *testing.T) {
	data := fixture(t, "openai_chat_stream_nousage.sse")
	// A gateway that dropped the usage chunk: [DONE] arrived, still no usage.
	got := run(ShapeOpenAIChat, Options{Stream: true, IncludeUsage: true}, data, 0)
	if got.State != StateUncounted || got.Reason != ReasonStreamWithoutUsage {
		t.Fatalf("got %+v", got)
	}
	// Cut before [DONE]: a different reason.
	cut := data[:bytes.Index(data, []byte("data: [DONE]"))]
	got = run(ShapeOpenAIChat, Options{Stream: true, IncludeUsage: true}, cut, 0)
	if got.State != StateUncounted || got.Reason != ReasonStreamCut {
		t.Fatalf("got %+v", got)
	}
}

func TestOpenAIChatUsageChunkWithoutOptInIsStillCounted(t *testing.T) {
	// A server that reports usage anyway: Wardyn reports what the answer said.
	got := run(ShapeOpenAIChat, Options{Stream: true}, fixture(t, "openai_chat_stream_usage.sse"), 0)
	if got.State != StateCounted || got.Input != 120 {
		t.Fatalf("got %+v", got)
	}
}

func TestOpenAIChatNonStream(t *testing.T) {
	// Usage object per https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create
	body := `{"id":"chatcmpl-3","object":"chat.completion","model":"gpt-5","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":12,"total_tokens":21,"prompt_tokens_details":{"cached_tokens":4,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":7}}}`
	got := run(ShapeOpenAIChat, Options{}, []byte(body), 5)
	expect(t, got, Usage{Model: "gpt-5", Input: 9, Output: 12, CacheRead: 4, Reasoning: 7, State: StateCounted})
}

func TestOpenAIResponsesStream(t *testing.T) {
	got := runAllChunks(t, ShapeOpenAIResponses, Options{Stream: true}, fixture(t, "openai_responses_stream.sse"))
	expect(t, got, Usage{Model: "gpt-5-codex", Input: 37, Output: 11, State: StateCounted})
}

// A gateway (LiteLLM's proxy) ends a Responses stream with the chat stream's data: [DONE] line after
// response.completed: the sentinel is not an event, and the call stays counted.
func TestOpenAIResponsesStreamDoneSentinel(t *testing.T) {
	data := append(fixture(t, "openai_responses_stream.sse"), []byte("data: [DONE]\n\n")...)
	got := runAllChunks(t, ShapeOpenAIResponses, Options{Stream: true}, data)
	expect(t, got, Usage{Model: "gpt-5-codex", Input: 37, Output: 11, State: StateCounted})
}

func TestOpenAIResponsesStreamCutAndFailed(t *testing.T) {
	data := fixture(t, "openai_responses_stream.sse")
	cut := data[:bytes.Index(data, []byte("event: response.completed"))]
	got := run(ShapeOpenAIResponses, Options{Stream: true}, cut, 0)
	expect(t, got, Usage{Model: "gpt-5-codex", State: StateUncounted, Reason: ReasonStreamCut})
	failed := `data: {"type":"response.failed","response":{"model":"m","status":"failed","usage":null}}` + "\n"
	got = run(ShapeOpenAIResponses, Options{Stream: true}, []byte(failed), 0)
	expect(t, got, Usage{Model: "m", State: StateUncounted, Reason: ReasonNoUsage})
}

func TestOpenAIResponsesNonStream(t *testing.T) {
	body := `{"id":"resp_1","object":"response","model":"gpt-5-codex","status":"completed","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":30},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":20},"total_tokens":150}}`
	got := run(ShapeOpenAIResponses, Options{}, []byte(body), 0)
	expect(t, got, Usage{Model: "gpt-5-codex", Input: 100, Output: 50, CacheRead: 30, Reasoning: 20, State: StateCounted})
}

func TestOllamaStreamAndNonStream(t *testing.T) {
	// Final-object fields per https://github.com/ollama/ollama/blob/main/docs/api.md ("Chat request (Streaming)":
	// prompt_eval_count 26, eval_count 282; "Chat request (No streaming)": 26 and 298).
	got := runAllChunks(t, ShapeOllama, Options{}, fixture(t, "ollama_chat_stream.ndjson"))
	expect(t, got, Usage{Model: "llama3.2", Input: 26, Output: 282, State: StateCounted})
	one := `{"model":"llama3.2","created_at":"2023-12-12T14:13:43.416799Z","message":{"role":"assistant","content":"Hello!"},"done":true,"prompt_eval_count":26,"eval_count":298}`
	expect(t, run(ShapeOllama, Options{}, []byte(one), 0), Usage{Model: "llama3.2", Input: 26, Output: 298, State: StateCounted})
}

func TestOllamaCutAndCachedPrompt(t *testing.T) {
	data := fixture(t, "ollama_chat_stream.ndjson")
	cut := data[:bytes.IndexByte(data, '\n')+1]
	expect(t, run(ShapeOllama, Options{}, cut, 0), Usage{Model: "llama3.2", State: StateUncounted, Reason: ReasonStreamCut})
	// prompt_eval_count is absent when the prompt was cached: counted, input zero.
	noPrompt := `{"model":"m","done":true,"eval_count":4}`
	expect(t, run(ShapeOllama, Options{}, []byte(noPrompt), 0), Usage{Model: "m", Output: 4, State: StateCounted})
}

func TestBoundsAndGarbage(t *testing.T) {
	cases := []struct {
		name  string
		shape Shape
		o     Options
		data  []byte
		want  Reason
	}{
		{"garbage body", ShapeAnthropicMessages, Options{}, []byte("<html>502 Bad Gateway</html>"), ReasonParseError},
		{"garbage sse", ShapeAnthropicMessages, Options{Stream: true}, []byte("data: {not json\n"), ReasonParseError},
		{"negative count", ShapeOpenAIChat, Options{}, []byte(`{"usage":{"prompt_tokens":-1,"completion_tokens":2}}`), ReasonParseError},
		{"fractional count", ShapeOpenAIChat, Options{}, []byte(`{"usage":{"prompt_tokens":1.5,"completion_tokens":2}}`), ReasonParseError},
		{"body oversize", ShapeAnthropicMessages, Options{}, bytes.Repeat([]byte(" "), MaxBodyBytes+1), ReasonBodyOversize},
		{"line oversize", ShapeAnthropicMessages, Options{Stream: true}, append([]byte("data: "), bytes.Repeat([]byte("a"), MaxLineBytes+1)...), ReasonBodyOversize},
		{"unknown shape", Shape("nope"), Options{}, []byte(`{}`), ReasonNoUsage},
	}
	for _, c := range cases {
		for _, chunk := range []int{0, 1 << 16} {
			got := run(c.shape, c.o, c.data, chunk)
			if got.State != StateUncounted || got.Reason != c.want || got.Input != 0 || got.Output != 0 {
				t.Errorf("%s (chunk %d): got %+v, want uncounted/%s", c.name, chunk, got, c.want)
			}
		}
	}
}

func TestBodyAtCapIsParsed(t *testing.T) {
	head := `{"model":"m","usage":{"input_tokens":1,"output_tokens":2}}`
	body := head + strings.Repeat(" ", MaxBodyBytes-len(head))
	if got := run(ShapeAnthropicMessages, Options{}, []byte(body), 1<<20); got.State != StateCounted {
		t.Fatalf("got %+v", got)
	}
}

func TestGarbageAfterFinalUsageIsUncounted(t *testing.T) {
	s := `data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}` + "\n" +
		`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":3}}` + "\ndata: {oops\n"
	got := run(ShapeAnthropicMessages, Options{Stream: true}, []byte(s), 0)
	if got.State != StateUncounted || got.Reason != ReasonParseError || got.Output != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestUndecodable(t *testing.T) {
	p := NewParser(ShapeAnthropicMessages, Options{RequestModel: "req"})
	p.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	p.Undecodable()
	p.Write([]byte("more"))
	expect(t, p.Result(), Usage{Model: "req", State: StateUncounted, Reason: ReasonUndecodable})
	expect(t, p.Result(), Usage{Model: "req", State: StateUncounted, Reason: ReasonUndecodable})
}

func TestPanicBecomesMeterError(t *testing.T) {
	p := NewParser(ShapeAnthropicMessages, Options{}).(*parser)
	p.onBody = func([]byte) { panic("boom") }
	n, err := p.Write([]byte("{}"))
	if n != 2 || err != nil {
		t.Fatalf("Write must never fail: %d %v", n, err)
	}
	expect(t, p.Result(), Usage{State: StateUncounted, Reason: ReasonMeterError})
	q := NewParser(ShapeOpenAIChat, Options{Stream: true}).(*parser)
	q.onLine = func([]byte) { panic("boom") }
	q.Write([]byte("data: {}\n"))
	expect(t, q.Result(), Usage{State: StateUncounted, Reason: ReasonMeterError})
}

func TestWriteDoesNotTouchInput(t *testing.T) {
	data := fixture(t, "anthropic_stream.sse")
	orig := bytes.Clone(data)
	run(ShapeAnthropicMessages, Options{Stream: true}, data, 5)
	if !bytes.Equal(data, orig) {
		t.Fatal("parser mutated the bytes it was shown")
	}
}

func TestModelFallbackAndResultRepeatable(t *testing.T) {
	p := NewParser(ShapeBedrockConverse, Options{RequestModel: "anthropic.claude-x"})
	p.Write([]byte(`{"usage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`))
	want := Usage{Model: "anthropic.claude-x", Input: 1, Output: 2, State: StateCounted}
	expect(t, p.Result(), want)
	expect(t, p.Result(), want)
}

func TestClassify(t *testing.T) {
	cases := []struct {
		path  string
		shape Shape
		model string
		ok    bool
	}{
		{"/v1/messages", ShapeAnthropicMessages, "", true},
		{"/wardyn/llm/anthropic/v1/messages?beta=true", ShapeAnthropicMessages, "", true},
		{"/anthropic/v1/messages", ShapeAnthropicMessages, "", true},
		{"/v1/messages/count_tokens", "", "", false},
		{"/v1/messages/batches", "", "", false},
		{"/v1/messages/batches/msgbatch_1/results", "", "", false},
		{"/v1/chat/completions", ShapeOpenAIChat, "", true},
		{"/openai/deployments/d/chat/completions", ShapeOpenAIChat, "", true},
		{"/v1/responses", ShapeOpenAIResponses, "", true},
		{"/v1/responses/resp_1", "", "", false},
		{"/v1/responses/input_tokens", "", "", false},
		{"/v1/batches", "", "", false},
		{"/api/chat", ShapeOllama, "", true},
		{"/api/generate", ShapeOllama, "", true},
		{"/api/tags", "", "", false},
		{"/model/anthropic.claude-3-sonnet-20240229-v1:0/invoke", ShapeBedrockInvoke, "anthropic.claude-3-sonnet-20240229-v1:0", true},
		{"/model/us.anthropic.claude-3-5-sonnet-20240620-v1%3A0/invoke-with-response-stream", ShapeBedrockInvokeStream, "us.anthropic.claude-3-5-sonnet-20240620-v1:0", true},
		{"/model/m/converse", ShapeBedrockConverse, "m", true},
		{"/model/arn%3Aaws%3Abedrock%3Aus-east-1%3A1%3Ainference-profile%2Fp/converse-stream", ShapeBedrockConverseStream, "arn:aws:bedrock:us-east-1:1:inference-profile/p", true},
		{"/model/m/count-tokens", "", "", false},
		{"/model/m", "", "", false},
		{"/", "", "", false},
	}
	for _, c := range cases {
		shape, model, ok := Classify(c.path)
		if shape != c.shape || model != c.model || ok != c.ok {
			t.Errorf("%s: got (%q, %q, %v), want (%q, %q, %v)", c.path, shape, model, ok, c.shape, c.model, c.ok)
		}
	}
}

func TestReadRequest(t *testing.T) {
	got := ReadRequest([]byte(`{"model":"gpt-5","stream":true,"stream_options":{"include_usage":true},"messages":[]}`))
	if got != (Request{Model: "gpt-5", Stream: true, IncludeUsage: true}) {
		t.Fatalf("got %+v", got)
	}
	if got := ReadRequest([]byte(`{"model":"m"}`)); got != (Request{Model: "m"}) {
		t.Fatalf("got %+v", got)
	}
	if got := ReadRequest([]byte("not json")); got != (Request{}) {
		t.Fatalf("got %+v", got)
	}
}

// The header names and the invocationMetrics body key are not in AWS's public API reference (see
// bedrock.go), so these fixtures are illustrative: the figures are invented.
func TestBedrockInvokeHeadersAndBody(t *testing.T) {
	h := http.Header{}
	h.Set("X-Amzn-Bedrock-Input-Token-Count", "17")
	h.Set("X-Amzn-Bedrock-Output-Token-Count", "9")
	// Headers alone (a non-Anthropic model body).
	got := run(ShapeBedrockInvoke, Options{Header: h, RequestModel: "amazon.titan-text-express-v1"}, []byte(`{"inputTextTokenCount":17,"results":[]}`), 0)
	expect(t, got, Usage{Model: "amazon.titan-text-express-v1", Input: 17, Output: 9, State: StateCounted})
	// Headers plus an Anthropic body that agrees and adds cache counts: max per field, no double count.
	body := `{"type":"message","model":"anthropic.claude-x","usage":{"input_tokens":17,"output_tokens":9,"cache_read_input_tokens":3}}`
	got = run(ShapeBedrockInvoke, Options{Header: h}, []byte(body), 0)
	expect(t, got, Usage{Model: "anthropic.claude-x", Input: 17, Output: 9, CacheRead: 3, State: StateCounted})
	// Body only: invocation metrics.
	body = `{"completion":"x","amazon-bedrock-invocationMetrics":{"inputTokenCount":5,"outputTokenCount":6,"invocationLatency":10,"firstByteLatency":2}}`
	got = run(ShapeBedrockInvoke, Options{}, []byte(body), 0)
	expect(t, got, Usage{Input: 5, Output: 6, State: StateCounted})
	// Neither header, no usage in the body: uncounted, no usage.
	got = run(ShapeBedrockInvoke, Options{Header: http.Header{}}, []byte(`{"results":[]}`), 0)
	expect(t, got, Usage{State: StateUncounted, Reason: ReasonNoUsage})
	// A header that is present but malformed or negative makes the call uncounted, even when the
	// other header is fine: a figure that cannot be read is never counted as zero.
	for _, bad := range []string{"abc", "-1", "1.5", " "} {
		h := http.Header{}
		h.Set("X-Amzn-Bedrock-Input-Token-Count", bad)
		h.Set("X-Amzn-Bedrock-Output-Token-Count", "9")
		got = run(ShapeBedrockInvoke, Options{Header: h}, []byte(`{"results":[]}`), 0)
		expect(t, got, Usage{State: StateUncounted, Reason: ReasonParseError})
		h.Set("X-Amzn-Bedrock-Input-Token-Count", "17")
		h.Set("X-Amzn-Bedrock-Output-Token-Count", bad)
		got = run(ShapeBedrockInvoke, Options{Header: h}, []byte(`{"results":[]}`), 0)
		expect(t, got, Usage{State: StateUncounted, Reason: ReasonParseError})
	}
}

func TestBedrockConverseNonStream(t *testing.T) {
	// Usage object per https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_TokenUsage.html
	body := `{"output":{"message":{"role":"assistant","content":[]}},"stopReason":"end_turn","usage":{"inputTokens":30,"outputTokens":8,"totalTokens":38,"cacheReadInputTokens":11,"cacheWriteInputTokens":2},"metrics":{"latencyMs":300}}`
	got := run(ShapeBedrockConverse, Options{RequestModel: "m"}, []byte(body), 0)
	expect(t, got, Usage{Model: "m", Input: 30, Output: 8, CacheRead: 11, CacheWrite: 2, State: StateCounted})
}
