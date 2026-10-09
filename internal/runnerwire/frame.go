// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package runnerwire is the wire contract between the organisation's runner hub
// and a wardyn-runnerd: the 20-byte frame, the stream-id space, flow control,
// replay and resume rules, and the typed REPLY error. It carries no policy; the
// org decides, the runner carries bytes. Transports (the WebSocket and the
// in-memory loopback) implement Conn.
package runnerwire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Type is a frame's type byte.
type Type uint8

const (
	TypeHello Type = iota + 1
	TypeAuth
	TypeAuthOK
	TypeRevoked
	TypeGoaway
	TypePending
	TypeState
	TypeReady
	TypeCall
	TypeReply
	TypeEvent
	TypeLease
	TypeOpen
	TypeOpenOK
	TypeData
	TypeWindow
	TypeClose
	TypeReset
	TypeAck
	TypePing
	TypePong
	typeEnd
)

// HeaderLen is the fixed frame header size.
const HeaderLen = 20

// Frame size limits. A frame over its limit closes the session.
const (
	MaxControlFrame = 1 << 20
	MaxDataFrame    = 64 << 10
)

// Frame is one decoded frame. Seq is non-zero exactly for the sequenced types.
type Frame struct {
	Type    Type
	Flags   uint8
	Stream  uint32
	Seq     uint64
	Payload []byte
}

var (
	ErrFrameTooLarge = errors.New("runnerwire: frame exceeds its size limit")
	ErrBadFrame      = errors.New("runnerwire: malformed frame")
)

type streamRule uint8

const (
	streamZero streamRule = iota // control: stream 0
	streamAny                    // a stream id or 0 (connection-level)
	streamNonZero
)

type direction uint8

const (
	dirEither direction = iota
	dirOrgToRunner
	dirRunnerToOrg
)

type typeSpec struct {
	name   string
	seqd   bool
	stream streamRule
	dir    direction
}

var specs = [typeEnd]typeSpec{
	TypeHello:   {"HELLO", false, streamZero, dirOrgToRunner},
	TypeAuth:    {"AUTH", false, streamZero, dirRunnerToOrg},
	TypeAuthOK:  {"AUTH_OK", false, streamZero, dirOrgToRunner},
	TypeRevoked: {"REVOKED", false, streamZero, dirOrgToRunner},
	TypeGoaway:  {"GOAWAY", false, streamZero, dirOrgToRunner},
	TypePending: {"PENDING", true, streamZero, dirOrgToRunner},
	TypeState:   {"STATE", true, streamZero, dirRunnerToOrg},
	TypeReady:   {"READY", true, streamZero, dirRunnerToOrg},
	TypeCall:    {"CALL", true, streamNonZero, dirEither},
	TypeReply:   {"REPLY", true, streamNonZero, dirEither},
	TypeEvent:   {"EVENT", true, streamZero, dirRunnerToOrg},
	TypeLease:   {"LEASE", true, streamZero, dirOrgToRunner},
	TypeOpen:    {"OPEN", false, streamNonZero, dirEither},
	TypeOpenOK:  {"OPEN_OK", false, streamNonZero, dirEither},
	TypeData:    {"DATA", false, streamNonZero, dirEither},
	TypeWindow:  {"WINDOW", false, streamAny, dirEither},
	TypeClose:   {"CLOSE", false, streamNonZero, dirEither},
	TypeReset:   {"RESET", false, streamNonZero, dirEither},
	TypeAck:     {"ACK", false, streamZero, dirEither},
	TypePing:    {"PING", false, streamZero, dirEither},
	TypePong:    {"PONG", false, streamZero, dirEither},
}

func (t Type) valid() bool { return t > 0 && t < typeEnd }

func (t Type) String() string {
	if !t.valid() {
		return fmt.Sprintf("TYPE(%d)", uint8(t))
	}
	return specs[t].name
}

// Sequenced reports whether frames of this type carry a sequence number and sit
// in the replay buffer until acknowledged.
func (t Type) Sequenced() bool { return t.valid() && specs[t].seqd }

// AllowedFrom reports whether a peer on the given side may send this type.
func (t Type) AllowedFrom(org bool) bool {
	if !t.valid() {
		return false
	}
	switch specs[t].dir {
	case dirOrgToRunner:
		return org
	case dirRunnerToOrg:
		return !org
	}
	return true
}

func (f Frame) maxPayload() int {
	if f.Type == TypeData {
		return MaxDataFrame
	}
	return MaxControlFrame
}

// Validate checks the frame against its type's rules: size, sequence presence,
// stream id class and a zero reserved field.
func (f Frame) Validate() error {
	if !f.Type.valid() {
		return fmt.Errorf("%w: unknown type %d", ErrBadFrame, uint8(f.Type))
	}
	if len(f.Payload) > f.maxPayload() {
		return fmt.Errorf("%w: %s payload %d bytes", ErrFrameTooLarge, f.Type, len(f.Payload))
	}
	spec := specs[f.Type]
	if spec.seqd != (f.Seq != 0) {
		return fmt.Errorf("%w: %s seq %d", ErrBadFrame, f.Type, f.Seq)
	}
	switch spec.stream {
	case streamZero:
		if f.Stream != 0 {
			return fmt.Errorf("%w: %s on stream %d", ErrBadFrame, f.Type, f.Stream)
		}
	case streamNonZero:
		if f.Stream == 0 {
			return fmt.Errorf("%w: %s on stream 0", ErrBadFrame, f.Type)
		}
	}
	return nil
}

// Encode appends the frame's wire bytes to dst: type u8, flags u8, reserved u16,
// stream u32, seq u64, length u32, payload, all big-endian.
func (f Frame) Encode(dst []byte) ([]byte, error) {
	if err := f.Validate(); err != nil {
		return dst, err
	}
	var h [HeaderLen]byte
	h[0], h[1] = byte(f.Type), f.Flags
	binary.BigEndian.PutUint32(h[4:], f.Stream)
	binary.BigEndian.PutUint64(h[8:], f.Seq)
	binary.BigEndian.PutUint32(h[16:], uint32(len(f.Payload)))
	return append(append(dst, h[:]...), f.Payload...), nil
}

// ReadFrame reads one frame from r. A length over the type's limit is refused
// before any payload is read, so an oversized frame cannot make the reader
// allocate; a short read is io.ErrUnexpectedEOF.
func ReadFrame(r io.Reader) (Frame, error) {
	var h [HeaderLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Frame{}, err
	}
	f := Frame{Type: Type(h[0]), Flags: h[1], Stream: binary.BigEndian.Uint32(h[4:]), Seq: binary.BigEndian.Uint64(h[8:])}
	if binary.BigEndian.Uint16(h[2:]) != 0 {
		return Frame{}, fmt.Errorf("%w: reserved field set", ErrBadFrame)
	}
	if !f.Type.valid() {
		return Frame{}, fmt.Errorf("%w: unknown type %d", ErrBadFrame, h[0])
	}
	n := binary.BigEndian.Uint32(h[16:])
	if int64(n) > int64(f.maxPayload()) {
		return Frame{}, fmt.Errorf("%w: %s length %d", ErrFrameTooLarge, f.Type, n)
	}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return Frame{}, err
		}
	}
	if err := f.Validate(); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// Decode decodes exactly one frame from b (one WebSocket binary message).
func Decode(b []byte) (Frame, error) {
	r := &sliceReader{b: b}
	f, err := ReadFrame(r)
	if err != nil {
		return Frame{}, err
	}
	if len(r.b) != 0 {
		return Frame{}, fmt.Errorf("%w: %d trailing bytes", ErrBadFrame, len(r.b))
	}
	return f, nil
}

type sliceReader struct{ b []byte }

func (s *sliceReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}
