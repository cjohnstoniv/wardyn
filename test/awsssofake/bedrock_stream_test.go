// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awsssofake

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"testing"
)

// eventStreamMessage is one decoded message of application/vnd.amazon.eventstream.
type eventStreamMessage struct {
	headers map[string]string
	payload []byte
}

// decodeEventStream reads the framing the way a client does and checks what a client checks: both
// CRCs, the declared lengths, and string headers. It is written from the format, not from the
// encoder in bedrock.go, so the two cannot agree on a mistake.
func decodeEventStream(b []byte) ([]eventStreamMessage, error) {
	var msgs []eventStreamMessage
	for len(b) > 0 {
		if len(b) < 16 {
			return nil, fmt.Errorf("%d trailing bytes, less than the 16 an empty message takes", len(b))
		}
		total, hlen := binary.BigEndian.Uint32(b[0:4]), binary.BigEndian.Uint32(b[4:8])
		if got, want := crc32.ChecksumIEEE(b[:8]), binary.BigEndian.Uint32(b[8:12]); got != want {
			return nil, fmt.Errorf("prelude CRC %08x, want %08x", got, want)
		}
		if int(total) > len(b) || int(total) < 16+int(hlen) {
			return nil, fmt.Errorf("message length %d with headers %d in %d bytes", total, hlen, len(b))
		}
		if got, want := crc32.ChecksumIEEE(b[:total-4]), binary.BigEndian.Uint32(b[total-4:total]); got != want {
			return nil, fmt.Errorf("message CRC %08x, want %08x", got, want)
		}
		m := eventStreamMessage{headers: map[string]string{}, payload: b[12+hlen : total-4]}
		for h := b[12 : 12+hlen]; len(h) > 0; {
			n := int(h[0])
			if len(h) < 1+n+3 || h[1+n] != 7 {
				return nil, fmt.Errorf("header at %d bytes left is not a string header", len(h))
			}
			vl := int(binary.BigEndian.Uint16(h[2+n : 4+n]))
			if len(h) < 4+n+vl {
				return nil, fmt.Errorf("header value runs past the headers")
			}
			m.headers[string(h[1:1+n])] = string(h[4+n : 4+n+vl])
			h = h[4+n+vl:]
		}
		msgs = append(msgs, m)
		b = b[total:]
	}
	return msgs, nil
}

func postStream(t *testing.T, s *Server, op string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(s.URL()+"/model/us.anthropic.claude-haiku-4-5/"+op, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// The streaming operations answer in event-stream framing, not JSON: a decoder reads every message,
// each names its event type, and the answer text is the stub's marker.
func TestBedrockStreamingOperationsAnswerInEventStreamFraming(t *testing.T) {
	s := New()
	defer s.Close()

	t.Run("converse-stream", func(t *testing.T) {
		resp, body := postStream(t, s, "converse-stream")
		if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.amazon.eventstream" {
			t.Fatalf("Content-Type = %q", ct)
		}
		msgs, err := decodeEventStream(body)
		if err != nil {
			t.Fatal(err)
		}
		var types []string
		var text string
		for _, m := range msgs {
			if m.headers[":message-type"] != "event" || m.headers[":content-type"] != "application/json" {
				t.Fatalf("headers = %v", m.headers)
			}
			types = append(types, m.headers[":event-type"])
			var ev struct{ Delta struct{ Text string } }
			if err := json.Unmarshal(m.payload, &ev); err != nil {
				t.Fatalf("%s payload %q: %v", m.headers[":event-type"], m.payload, err)
			}
			text += ev.Delta.Text
		}
		if want := "messageStart contentBlockDelta contentBlockStop messageStop metadata"; strings.Join(types, " ") != want || text != bedrockStubMarker {
			t.Fatalf("events %q text %q, want %q and the marker", types, text, want)
		}
	})

	t.Run("invoke-with-response-stream", func(t *testing.T) {
		resp, body := postStream(t, s, "invoke-with-response-stream")
		if ct := resp.Header.Get("Content-Type"); ct != "application/vnd.amazon.eventstream" {
			t.Fatalf("Content-Type = %q", ct)
		}
		msgs, err := decodeEventStream(body)
		if err != nil {
			t.Fatal(err)
		}
		var types []string
		var text string
		for _, m := range msgs {
			if m.headers[":event-type"] != "chunk" {
				t.Fatalf("event type %q, want chunk", m.headers[":event-type"])
			}
			var chunk struct{ Bytes string }
			if err := json.Unmarshal(m.payload, &chunk); err != nil {
				t.Fatalf("chunk payload %q: %v", m.payload, err)
			}
			raw, err := base64.StdEncoding.DecodeString(chunk.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			var ev struct {
				Type  string
				Delta struct{ Text string }
			}
			if err := json.Unmarshal(raw, &ev); err != nil {
				t.Fatalf("model event %q: %v", raw, err)
			}
			types = append(types, ev.Type)
			text += ev.Delta.Text
		}
		if want := "message_start content_block_start content_block_delta content_block_stop message_delta message_stop"; strings.Join(types, " ") != want || text != bedrockStubMarker {
			t.Fatalf("events %q text %q, want %q and the marker", types, text, want)
		}
	})

	t.Run("a corrupted frame is refused by the decoder", func(t *testing.T) {
		_, body := postStream(t, s, "converse-stream")
		bad := bytes.Clone(body)
		bad[len(bad)/2] ^= 0xff
		if _, err := decodeEventStream(bad); err == nil {
			t.Fatal("the decoder accepted a corrupted stream, so it proves nothing about the real one")
		}
	})

	if s.BedrockCalls() != 3 {
		t.Fatalf("BedrockCalls = %d, want 3 answered streaming calls", s.BedrockCalls())
	}
}
