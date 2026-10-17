// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// The decision sink wire (k8s runners): TLS 1.3 with ALPN SinkALPN, the proxy
// pinning runnerd's SPKI; frames are a 4-byte big-endian length and one JSON
// SinkFrame. The proxy's first frame is hello.
const (
	SinkALPN     = "wardyn-sink/1"
	SinkVersion  = 1
	MaxSinkFrame = 1<<20 + 8<<10
)

// Frame names. Proxy to runnerd: hello, record, refused, drained, lapsed.
// runnerd to proxy: accept, grant, ack, drain.
const (
	FrameHello   = "hello"
	FrameRecord  = "record"
	FrameRefused = "refused"
	FrameDrained = "drained"
	FrameLapsed  = "lapsed"
	FrameAccept  = "accept"
	FrameGrant   = "grant"
	FrameAck     = "ack"
	FrameDrain   = "drain"
)

// SinkFrame is every sink frame; T names it and only its own fields are set.
type SinkFrame struct {
	T string `json:"t"`
	// hello
	V        int    `json:"v,omitempty"`
	RunID    string `json:"run_id,omitempty"`
	Instance string `json:"instance,omitempty"`
	Key      string `json:"key,omitempty"`
	// every frame after hello
	Gen uint64 `json:"gen,omitempty"`
	// record, accept, ack
	Seq  int64           `json:"seq,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
	// ack
	Disposition string `json:"disposition,omitempty"`
	// refused
	Total uint64 `json:"total,omitempty"`
	// drained
	FinalSeq int64  `json:"final_seq,omitempty"`
	Consumed uint64 `json:"consumed,omitempty"`
	// lapsed
	Reason string `json:"reason,omitempty"`
	// grant
	Epoch   uuid.UUID `json:"epoch,omitempty"`
	Base    int64     `json:"base,omitempty"`
	Through int64     `json:"through,omitempty"`
	Bytes   int64     `json:"bytes,omitempty"`
}

// RecordBody is a record frame's body. Its hash is the SHA-256 of the body
// bytes as sent, and a resend carries the identical bytes.
type RecordBody struct {
	Admitted bool               `json:"admitted,omitempty"`
	Epoch    uuid.UUID          `json:"epoch"`
	Activity bool               `json:"activity,omitempty"`
	Decision egress.DecisionLog `json:"decision"`
}

// WriteSinkFrame writes one length-prefixed frame.
func WriteSinkFrame(w io.Writer, f SinkFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(b) > MaxSinkFrame {
		return errors.New("sink frame too large")
	}
	buf := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	copy(buf[4:], b)
	_, err = w.Write(buf)
	return err
}

// ReadSinkFrame reads one length-prefixed frame, refusing an oversized one.
func ReadSinkFrame(r io.Reader) (SinkFrame, error) {
	var f SinkFrame
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return f, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size == 0 || size > MaxSinkFrame {
		return f, errors.New("sink frame size out of bounds")
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return f, err
	}
	err := json.Unmarshal(b, &f)
	return f, err
}
