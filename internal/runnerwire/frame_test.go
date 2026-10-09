// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"
)

func sampleFrames() []Frame {
	return []Frame{
		{Type: TypeHello, Payload: []byte(`{}`)},
		{Type: TypeAuth},
		{Type: TypePending, Seq: 1, Payload: []byte(`[]`)},
		{Type: TypeCall, Seq: 2, Stream: 2, Payload: []byte(`{"method":"status"}`)},
		{Type: TypeReply, Seq: 3, Stream: 2},
		{Type: TypeEvent, Seq: 4, Payload: []byte(`{"kind":"caps"}`)},
		{Type: TypeOpen, Stream: 1, Payload: []byte(`{"kind":"pty"}`)},
		{Type: TypeData, Stream: 1, Payload: bytes.Repeat([]byte{7}, MaxDataFrame)},
		{Type: TypeWindow, Payload: EncodeWindow(1 << 20)},
		{Type: TypeWindow, Stream: 3, Payload: EncodeWindow(5)},
		{Type: TypeClose, Stream: 3},
		{Type: TypeReset, Stream: 3, Payload: EncodeReset(ResetReconnect)},
		{Type: TypeAck, Payload: EncodeAck(9)},
		{Type: TypePing, Payload: EncodePing(testTime)},
		{Type: TypeCall, Seq: 5, Stream: 4, Payload: bytes.Repeat([]byte{1}, MaxControlFrame)},
	}
}

func TestFrameRoundTrip(t *testing.T) {
	for _, f := range sampleFrames() {
		b, err := f.Encode(nil)
		if err != nil {
			t.Fatalf("%s encode: %v", f.Type, err)
		}
		if len(b) != HeaderLen+len(f.Payload) {
			t.Fatalf("%s: %d wire bytes, want %d", f.Type, len(b), HeaderLen+len(f.Payload))
		}
		got, err := Decode(b)
		if err != nil {
			t.Fatalf("%s decode: %v", f.Type, err)
		}
		if got.Type != f.Type || got.Stream != f.Stream || got.Seq != f.Seq || !bytes.Equal(got.Payload, f.Payload) {
			t.Fatalf("%s did not round-trip: %+v", f.Type, got)
		}
	}
}

func TestFrameRandomRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for range 2000 {
		ty := Type(1 + rng.Intn(int(typeEnd)-1))
		f := Frame{Type: ty, Flags: uint8(rng.Intn(256)), Payload: make([]byte, rng.Intn(300))}
		rng.Read(f.Payload)
		if ty.Sequenced() {
			f.Seq = 1 + rng.Uint64()%1000
		}
		switch specs[ty].stream {
		case streamNonZero:
			f.Stream = 1 + rng.Uint32()%100
		case streamAny:
			f.Stream = rng.Uint32() % 100
		}
		b, err := f.Encode(nil)
		if err != nil {
			t.Fatalf("encode %+v: %v", f, err)
		}
		got, err := Decode(b)
		if err != nil || got.Type != f.Type || got.Flags != f.Flags || got.Stream != f.Stream || got.Seq != f.Seq || !bytes.Equal(got.Payload, f.Payload) {
			t.Fatalf("round trip %+v -> %+v (%v)", f, got, err)
		}
	}
}

func FuzzDecode(f *testing.F) {
	for _, fr := range sampleFrames()[:8] {
		b, _ := fr.Encode(nil)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		fr, err := Decode(b)
		if err != nil {
			return
		}
		again, err := fr.Encode(nil)
		if err != nil {
			t.Fatalf("a decoded frame must re-encode: %v", err)
		}
		if !bytes.Equal(again, b) {
			t.Fatalf("re-encode differs: %x vs %x", again, b)
		}
	})
}

func TestFrameLimits(t *testing.T) {
	if _, err := (Frame{Type: TypeData, Stream: 1, Payload: make([]byte, MaxDataFrame+1)}).Encode(nil); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize DATA encoded: %v", err)
	}
	if _, err := (Frame{Type: TypeCall, Seq: 1, Stream: 2, Payload: make([]byte, MaxControlFrame+1)}).Encode(nil); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("oversize control frame encoded: %v", err)
	}
	// The length field alone refuses an oversized frame: no payload is read or allocated.
	h, _ := (Frame{Type: TypeData, Stream: 1}).Encode(nil)
	h[16], h[17], h[18], h[19] = 0xff, 0xff, 0xff, 0xff
	if _, err := Decode(h); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("a 4 GiB length was not refused as too large: %v", err)
	}
}

func TestFrameRejectsMalformed(t *testing.T) {
	good := func(f Frame) []byte { b, _ := f.Encode(nil); return b }
	cases := map[string][]byte{
		"unknown type":     append([]byte{0xee}, good(Frame{Type: TypePing})[1:]...),
		"zero type":        append([]byte{0}, good(Frame{Type: TypePing})[1:]...),
		"reserved set":     func() []byte { b := good(Frame{Type: TypePing}); b[2] = 1; return b }(),
		"seq on unseq'd":   func() []byte { b := good(Frame{Type: TypePing}); b[15] = 1; return b }(),
		"no seq on seq'd":  func() []byte { b := good(Frame{Type: TypeReady, Seq: 1}); b[15] = 0; return b }(),
		"stream on ctl":    func() []byte { b := good(Frame{Type: TypePing}); b[7] = 1; return b }(),
		"call on stream 0": func() []byte { b := good(Frame{Type: TypeCall, Seq: 1, Stream: 2}); b[7] = 0; return b }(),
		"trailing bytes":   append(good(Frame{Type: TypePing}), 0),
		"truncated header": good(Frame{Type: TypePing})[:10],
		"truncated body":   good(Frame{Type: TypeData, Stream: 1, Payload: []byte("abcd")})[:HeaderLen+2],
	}
	for name, b := range cases {
		if _, err := Decode(b); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if _, err := ReadFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Errorf("empty input: %v, want EOF", err)
	}
	if _, err := ReadFrame(bytes.NewReader(good(Frame{Type: TypePing})[:10])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("short header: %v, want ErrUnexpectedEOF", err)
	}
}

func TestTypeDirections(t *testing.T) {
	orgOnly := []Type{TypeHello, TypeAuthOK, TypeRevoked, TypeGoaway, TypePending, TypeLease}
	runnerOnly := []Type{TypeAuth, TypeState, TypeReady, TypeEvent}
	for _, ty := range orgOnly {
		if !ty.AllowedFrom(true) || ty.AllowedFrom(false) {
			t.Errorf("%s must be org -> runner only", ty)
		}
	}
	for _, ty := range runnerOnly {
		if ty.AllowedFrom(true) || !ty.AllowedFrom(false) {
			t.Errorf("%s must be runner -> org only", ty)
		}
	}
	for _, ty := range []Type{TypeCall, TypeReply, TypeOpen, TypeData, TypeWindow, TypeClose, TypeReset, TypeAck, TypePing, TypePong} {
		if !ty.AllowedFrom(true) || !ty.AllowedFrom(false) {
			t.Errorf("%s must go either way", ty)
		}
	}
}

func TestSequencedTypesMatchDesign(t *testing.T) {
	want := map[Type]bool{TypePending: true, TypeState: true, TypeReady: true, TypeCall: true, TypeReply: true, TypeEvent: true, TypeLease: true}
	for ty := Type(1); ty < typeEnd; ty++ {
		if ty.Sequenced() != want[ty] {
			t.Errorf("%s sequenced=%v, design says %v", ty, ty.Sequenced(), want[ty])
		}
	}
}
