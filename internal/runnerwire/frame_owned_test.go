// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// rawHeader builds a frame header by hand, so a case can break a rule Encode would refuse.
func rawHeader(typ byte, reserved uint16, stream uint32, seq uint64, n uint32) []byte {
	h := make([]byte, HeaderLen)
	h[0] = typ
	binary.BigEndian.PutUint16(h[2:], reserved)
	binary.BigEndian.PutUint32(h[4:], stream)
	binary.BigEndian.PutUint64(h[8:], seq)
	binary.BigEndian.PutUint32(h[16:], n)
	return h
}

func TestDecodeOwnedAnswersAsDecode(t *testing.T) {
	data := bytes.Repeat([]byte{7}, MaxDataFrame)
	validData := encodedFrame(t, Frame{Type: TypeData, Stream: 1, Payload: data})
	validPing := encodedFrame(t, Frame{Type: TypePing, Payload: EncodePing(time.Now())})
	withBytes := func(h []byte, n int) []byte { return append(h, bytes.Repeat([]byte{1}, n)...) }
	// DATA on stream 0 is invalid (Validate), and the message also carries one trailing byte.
	invalidAndTrailing := withBytes(rawHeader(byte(TypeData), 0, 0, 0, 4), 5)

	cases := []struct {
		name string
		msg  []byte
	}{
		{"empty", nil},
		{"one byte", []byte{1}},
		{"header only", rawHeader(byte(TypePing), 0, 0, 0, 0)},
		{"valid ping", validPing},
		{"valid 64 KiB data", validData},
		{"reserved field set", rawHeader(byte(TypePing), 1, 0, 0, 0)},
		{"unknown type", rawHeader(250, 0, 0, 0, 0)},
		{"data over the limit", withBytes(rawHeader(byte(TypeData), 0, 1, 0, MaxDataFrame+1), MaxDataFrame+1)},
		{"payload one byte short", validData[:len(validData)-1]},
		{"one trailing byte", append(append([]byte{}, validPing...), 0)},
		{"invalid and one trailing byte", invalidAndTrailing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want, wantErr := Decode(tc.msg)
			got, gotErr := decodeOwned(tc.msg)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("Decode err=%v, decodeOwned err=%v", wantErr, gotErr)
			}
			if wantErr != nil {
				for _, target := range []error{ErrBadFrame, ErrFrameTooLarge, io.ErrUnexpectedEOF, io.EOF} {
					if errors.Is(wantErr, target) != errors.Is(gotErr, target) {
						t.Fatalf("Decode err=%v, decodeOwned err=%v differ on %v", wantErr, gotErr, target)
					}
				}
				if wantErr.Error() != gotErr.Error() {
					t.Fatalf("Decode err=%q, decodeOwned err=%q", wantErr, gotErr)
				}
				return
			}
			if got.Type != want.Type || got.Flags != want.Flags || got.Stream != want.Stream || got.Seq != want.Seq || !bytes.Equal(got.Payload, want.Payload) {
				t.Fatalf("Decode %+v, decodeOwned %+v", want, got)
			}
			if tc.name == "valid 64 KiB data" {
				if &got.Payload[0] != &tc.msg[HeaderLen] || cap(got.Payload) != len(got.Payload) {
					t.Fatalf("payload is not the message's own bytes, capped: cap %d len %d", cap(got.Payload), len(got.Payload))
				}
			}
		})
	}
	// The table must hold both outcomes, or it proves nothing.
	if _, err := Decode(validData); err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(invalidAndTrailing); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("the both-ways-bad case is not refused as a bad frame: %v", err)
	}
}

func TestDecodeCopiesThePayload(t *testing.T) {
	msg := encodedFrame(t, Frame{Type: TypeData, Stream: 1, Payload: []byte("abcdef")})
	f, err := Decode(msg)
	if err != nil {
		t.Fatal(err)
	}
	for i := range msg {
		msg[i] = 'x'
	}
	if string(f.Payload) != "abcdef" {
		t.Fatalf("Decode aliased the message: %q", f.Payload)
	}
}

func TestWebSocketDataFramesRoundTripThroughThePool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const frames = 200
	payload := func(i int) []byte {
		return bytes.Repeat([]byte{byte(i % 251)}, 1+(i*327)%MaxDataFrame)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ws := NewWebSocket(conn)
		for i := 0; i < frames; i++ {
			if err := ws.WriteFrame(ctx, Frame{Type: TypeData, Stream: 1, Payload: payload(i)}); err != nil {
				return
			}
			if i%10 == 9 {
				if err := ws.WriteFrame(ctx, Frame{Type: TypePing, Payload: EncodePing(time.Now())}); err != nil {
					return
				}
			}
		}
		_, _, _ = conn.Read(ctx) // hold the link open until the client is done
	}))
	defer server.Close()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	ws := NewWebSocket(conn)
	var held []Frame // every received DATA frame is kept to the end: no later write may overwrite one
	for len(held) < frames {
		f, err := ws.ReadFrame(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == TypeData {
			held = append(held, f)
		}
	}
	for i, f := range held {
		if !bytes.Equal(f.Payload, payload(i)) {
			t.Fatalf("frame %d changed: %d bytes", i, len(f.Payload))
		}
	}
}
