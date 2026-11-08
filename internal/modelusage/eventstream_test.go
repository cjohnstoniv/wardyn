// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package modelusage

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// encodeFrame builds one AWS event-stream message: prelude (total length, header length, CRC of those eight
// bytes), headers, payload, and a CRC of everything before it (the AWS event-stream framing; no vendor page
// was fetched for it, so the layout is from the format as implemented by the AWS SDKs). Headers here are
// string-typed (type 7), plus whatever raw header bytes a test appends.
func encodeFrame(eventType, messageType string, payload []byte, rawHeaders ...[]byte) []byte {
	var h bytes.Buffer
	str := func(name, val string) {
		h.WriteByte(byte(len(name)))
		h.WriteString(name)
		h.WriteByte(7)
		binary.Write(&h, binary.BigEndian, uint16(len(val)))
		h.WriteString(val)
	}
	str(":message-type", messageType)
	if eventType != "" {
		str(":event-type", eventType)
	}
	str(":content-type", "application/json")
	for _, r := range rawHeaders {
		h.Write(r)
	}
	total := uint32(12 + h.Len() + len(payload) + 4)
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, total)
	binary.Write(&b, binary.BigEndian, uint32(h.Len()))
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
	b.Write(h.Bytes())
	b.Write(payload)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
	return b.Bytes()
}

// Event shapes per https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_ConverseStream.html:
// the metadata event carries usage (inputTokens, outputTokens, totalTokens, cacheReadInputTokens,
// cacheWriteInputTokens) and metrics.latencyMs.
func converseStream() []byte {
	var s []byte
	s = append(s, encodeFrame("messageStart", "event", []byte(`{"role":"assistant"}`))...)
	s = append(s, encodeFrame("contentBlockDelta", "event", []byte(`{"contentBlockIndex":0,"delta":{"text":"Hi"}}`))...)
	s = append(s, encodeFrame("messageStop", "event", []byte(`{"stopReason":"end_turn"}`))...)
	s = append(s, encodeFrame("metadata", "event", []byte(`{"usage":{"inputTokens":30,"outputTokens":8,"totalTokens":38,"cacheReadInputTokens":11,"cacheWriteInputTokens":2},"metrics":{"latencyMs":300}}`))...)
	return s
}

func TestConverseStreamFrames(t *testing.T) {
	got := runAllChunks(t, ShapeBedrockConverseStream, Options{RequestModel: "m"}, converseStream())
	expect(t, got, Usage{Model: "m", Input: 30, Output: 8, CacheRead: 11, CacheWrite: 2, State: StateCounted})
}

func TestConverseStreamMetadataWrapper(t *testing.T) {
	f := encodeFrame("metadata", "event", []byte(`{"metadata":{"usage":{"inputTokens":4,"outputTokens":5}}}`))
	expect(t, run(ShapeBedrockConverseStream, Options{}, f, 0), Usage{Input: 4, Output: 5, State: StateCounted})
}

func TestConverseStreamCutBeforeMetadata(t *testing.T) {
	s := converseStream()
	cut := s[:len(s)-40] // the metadata frame is torn
	expect(t, runAllChunks(t, ShapeBedrockConverseStream, Options{}, cut), Usage{State: StateUncounted, Reason: ReasonStreamCut})
}

func TestConverseStreamExceptionFrameIsNotCounted(t *testing.T) {
	f := encodeFrame("throttlingException", "exception", []byte(`{"message":"slow down","usage":{"inputTokens":99,"outputTokens":99}}`))
	expect(t, run(ShapeBedrockConverseStream, Options{}, f, 0), Usage{State: StateUncounted, Reason: ReasonStreamCut})
}

// The InvokeModelWithResponseStream chunk is {"bytes": base64(model event)}; for an Anthropic model on
// Bedrock the events are the Messages stream events, and the last chunk also carries
// amazon-bedrock-invocationMetrics.
func chunk(event string) []byte {
	payload := `{"bytes":"` + base64.StdEncoding.EncodeToString([]byte(event)) + `"}`
	return encodeFrame("chunk", "event", []byte(payload))
}

func TestInvokeStreamAnthropicEvents(t *testing.T) {
	var s []byte
	s = append(s, chunk(`{"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":12,"cache_read_input_tokens":3,"output_tokens":1}}}`)...)
	s = append(s, chunk(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"a"}}`)...)
	s = append(s, chunk(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":21},"amazon-bedrock-invocationMetrics":{"inputTokenCount":12,"outputTokenCount":21,"invocationLatency":1000,"firstByteLatency":100}}`)...)
	got := runAllChunks(t, ShapeBedrockInvokeStream, Options{}, s)
	expect(t, got, Usage{Model: "claude-x", Input: 12, Output: 21, CacheRead: 3, State: StateCounted})
}

func TestInvokeStreamMetricsOnly(t *testing.T) {
	// A non-Anthropic model: only the invocation metrics on the last chunk count. The metrics key is
	// not in AWS's public API reference (see bedrock.go), so this chunk is illustrative.
	var s []byte
	s = append(s, chunk(`{"outputText":"a","index":0}`)...)
	s = append(s, chunk(`{"outputText":"b","amazon-bedrock-invocationMetrics":{"inputTokenCount":7,"outputTokenCount":2}}`)...)
	expect(t, run(ShapeBedrockInvokeStream, Options{RequestModel: "amazon.titan"}, s, 0), Usage{Model: "amazon.titan", Input: 7, Output: 2, State: StateCounted})
}

func TestInvokeStreamCutIsPartial(t *testing.T) {
	s := chunk(`{"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":12,"output_tokens":1}}}`)
	got := run(ShapeBedrockInvokeStream, Options{}, s, 0)
	expect(t, got, Usage{Model: "claude-x", Input: 12, Output: 1, State: StatePartial, Reason: ReasonStreamCut})
}

func TestEventStreamEdgeCases(t *testing.T) {
	good := converseStream()
	flip := func(b []byte, i int) []byte { c := bytes.Clone(b); c[i] ^= 0xff; return c }
	oversize := func() []byte {
		var b bytes.Buffer
		binary.Write(&b, binary.BigEndian, uint32(maxFrame+1))
		binary.Write(&b, binary.BigEndian, uint32(0))
		binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
		return b.Bytes()
	}
	badLens := func() []byte { // headers longer than the frame, with a valid prelude CRC
		var b bytes.Buffer
		binary.Write(&b, binary.BigEndian, uint32(20))
		binary.Write(&b, binary.BigEndian, uint32(500))
		binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
		return append(b.Bytes(), make([]byte, 8)...)
	}
	tooShort := func() []byte { // total below the minimum of 16
		var b bytes.Buffer
		binary.Write(&b, binary.BigEndian, uint32(8))
		binary.Write(&b, binary.BigEndian, uint32(0))
		binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
		return b.Bytes()
	}
	cases := []struct {
		name string
		data []byte
		want Reason
	}{
		{"prelude crc", flip(good, 9), ReasonParseError},
		{"message crc", flip(good, 30), ReasonParseError},
		{"total length lies", flip(good, 2), ReasonParseError},
		{"frame over cap", oversize(), ReasonBodyOversize},
		{"header length exceeds frame", badLens(), ReasonParseError},
		{"total below minimum", tooShort(), ReasonParseError},
		{"unknown header type", encodeFrame("metadata", "event", []byte(`{}`), []byte{1, 'x', 99}), ReasonParseError},
		{"truncated header value", encodeFrame("metadata", "event", []byte(`{}`), []byte{1, 'x', 7, 0xff, 0xff}), ReasonParseError},
		{"payload not json", encodeFrame("metadata", "event", []byte(`oops`)), ReasonParseError},
	}
	for _, c := range cases {
		for _, chunk := range []int{0, 1, 5} {
			got := run(ShapeBedrockConverseStream, Options{}, c.data, chunk)
			if got.State != StateUncounted || got.Reason != c.want || got.Input != 0 {
				t.Errorf("%s (chunk %d): got %+v, want uncounted/%s", c.name, chunk, got, c.want)
			}
		}
	}
}

func TestEventStreamSkipsOtherHeaderTypes(t *testing.T) {
	// Types 0/1 (bool), 2 (byte), 3 (short), 4 (int), 5 (long), 6 (bytes), 8 (timestamp), 9 (uuid) are skipped by width.
	raw := [][]byte{
		{1, 'a', 0}, {1, 'b', 1}, {1, 'c', 2, 9}, {1, 'd', 3, 0, 1}, {1, 'e', 4, 0, 0, 0, 1},
		append([]byte{1, 'f', 5}, make([]byte, 8)...),
		{1, 'g', 6, 0, 2, 'h', 'i'},
		append([]byte{1, 'h', 8}, make([]byte, 8)...),
		append([]byte{1, 'i', 9}, make([]byte, 16)...),
	}
	f := encodeFrame("metadata", "event", []byte(`{"usage":{"inputTokens":1,"outputTokens":2}}`), raw...)
	expect(t, run(ShapeBedrockConverseStream, Options{}, f, 1), Usage{Input: 1, Output: 2, State: StateCounted})
}

func TestEventStreamHugeWriteStaysBounded(t *testing.T) {
	// One Write far larger than a frame must not buffer it whole: the oversize frame is refused on its prelude.
	p := NewParser(ShapeBedrockConverseStream, Options{}).(*parser)
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, uint32(maxFrame*3))
	binary.Write(&b, binary.BigEndian, uint32(0))
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(b.Bytes()))
	b.Write(make([]byte, 8<<20))
	p.Write(b.Bytes())
	if len(p.buf) != 0 {
		t.Fatalf("buffer retained %d bytes", len(p.buf))
	}
	if r := p.Result(); r.Reason != ReasonBodyOversize {
		t.Fatalf("got %+v", r)
	}
}
