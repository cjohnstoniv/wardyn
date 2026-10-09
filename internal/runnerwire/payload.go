// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// Handshake payloads.
type (
	Hello struct {
		Versions     []int     `json:"versions"`
		Nonce        []byte    `json:"nonce"`
		ServerTime   time.Time `json:"server_time"`
		OrgURLSHA256 string    `json:"org_url_sha256"`
	}
	Auth struct {
		RunnerID string  `json:"runner_id"`
		Version  int     `json:"version"`
		Sig      []byte  `json:"sig"`
		Resume   *Resume `json:"resume,omitempty"`
	}
	Resume struct {
		SessionID string `json:"session_id"`
		LastSeq   uint64 `json:"last_seq"`
	}
	AuthOK struct {
		SessionID    string        `json:"session_id"`
		Resumed      bool          `json:"resumed"`
		PingInterval time.Duration `json:"ping_interval"`
	}
	// Reason is the payload of REVOKED and GOAWAY.
	Reason struct {
		Reason string `json:"reason"`
	}
)

// Call and Reply are the CALL and REPLY payloads. The frame's stream is the call's own id.
type Call struct {
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args,omitempty"`
}

type Reply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Event kinds, runner to org.
const (
	EventCaps          = "caps"
	EventPosture       = "posture"
	EventWaiting       = "waiting"
	EventAgentExit     = "agent_exit"
	EventActionResult  = "action_result"
	EventSpool         = "spool"
	EventResidentErase = "resident_erase"
)

// EventKinds is the closed list.
var EventKinds = []string{EventCaps, EventPosture, EventWaiting, EventAgentExit, EventActionResult, EventSpool, EventResidentErase}

// Event is the EVENT payload. Call, when non-zero, names the in-flight CALL the
// event belongs to (a waiting report), so the org can deliver it on that
// caller's goroutine before the REPLY.
type Event struct {
	Kind  string          `json:"kind"`
	Call  uint32          `json:"call,omitempty"`
	RunID *uuid.UUID      `json:"run_id,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Validate refuses a kind outside the closed list.
func (e Event) Validate() error {
	for _, k := range EventKinds {
		if k == e.Kind {
			return nil
		}
	}
	return fmt.Errorf("%w: unknown event kind %q", ErrBadFrame, e.Kind)
}

// Caps is the `caps` event: the runner's Docker ClassSupport plus its capacity.
// All of it is runner_asserted.
type Caps struct {
	Support substrate.ClassSupport `json:"support"`
	placement.Capacity
	AllowedRoots []string `json:"allowed_roots,omitempty"`
	Version      string   `json:"version"`
}

// Waiting is the `waiting` event's data.
type Waiting struct {
	Detail string `json:"detail"`
}

// AgentExit is the durable `agent_exit` event's data.
type AgentExit struct {
	Ref        string    `json:"ref"`
	Code       int       `json:"code"`
	ObservedAt time.Time `json:"observed_at"`
}

// Lease is the LEASE payload, sent after each renew or extend.
type Lease struct {
	RunID          uuid.UUID `json:"run_id"`
	TokenRenewedAt time.Time `json:"token_renewed_at"`
	EndsAt         time.Time `json:"ends_at"`
}

// Pending action kinds, shared with runner_pending_actions.
const (
	ActionKill      = "kill"
	ActionEnd       = "end"
	ActionStopProxy = "stop_proxy"
)

// PendingAction is one entry of the PENDING payload, oldest first.
type PendingAction struct {
	ID    uuid.UUID `json:"id"`
	Kind  string    `json:"kind"`
	RunID uuid.UUID `json:"run_id"`
	Ref   string    `json:"ref"`
}

// ActionResult is the `action_result` event's data.
type ActionResult struct {
	ID         uuid.UUID `json:"id"`
	Outcome    string    `json:"outcome"`
	ObservedAt time.Time `json:"observed_at"`
}

// RunState is one run's entry in the STATE payload; all runner_asserted.
type RunState struct {
	RunID          uuid.UUID  `json:"run_id"`
	SandboxPresent bool       `json:"sandbox_present"`
	AgentState     string     `json:"agent_state,omitempty"`
	ExitCode       *int       `json:"exit_code,omitempty"`
	ProxyStoppedAt *time.Time `json:"proxy_stopped_at,omitempty"`
	SpoolDepth     int        `json:"spool_depth"`
	SpoolDrops     int        `json:"spool_drops"`
	ResidentGrants []string   `json:"resident_grants,omitempty"`
}

// Byte-stream kinds carried by OPEN.
const (
	KindPTY        = "pty"
	KindExec       = "exec"
	KindExecStderr = "exec_stderr"
	KindRelay      = "relay"
	KindOutput     = "output"
)

// Open is the OPEN payload.
type Open struct {
	Kind   string `json:"kind"`
	Target string `json:"target,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	// Call, when non-zero, names the in-flight CALL this stream was opened for;
	// the REPLY names the stream and the caller claims it with Peer.TakeStream.
	Call uint32 `json:"call,omitempty"`
}

// Validate refuses a kind outside the closed list.
func (o Open) Validate() error {
	switch o.Kind {
	case KindPTY, KindExec, KindExecStderr, KindRelay, KindOutput:
		return nil
	}
	return fmt.Errorf("%w: unknown stream kind %q", ErrBadFrame, o.Kind)
}

// Reset codes carried by RESET.
const (
	ResetCancelled  uint32 = 1
	ResetReconnect  uint32 = 2
	ResetRefused    uint32 = 3
	ResetInternal   uint32 = 4
	ResetNoSuchPeer uint32 = 5
)

// Fixed-width payloads: WINDOW (u32 increment), RESET (u32 code), ACK (u64
// cumulative seq) and PING/PONG (i64 unix nanoseconds).

func EncodeWindow(n uint32) []byte          { return binary.BigEndian.AppendUint32(nil, n) }
func EncodeReset(code uint32) []byte        { return binary.BigEndian.AppendUint32(nil, code) }
func EncodeAck(seq uint64) []byte           { return binary.BigEndian.AppendUint64(nil, seq) }
func EncodePing(t time.Time) []byte         { return binary.BigEndian.AppendUint64(nil, uint64(t.UnixNano())) }
func DecodeWindow(p []byte) (uint32, error) { return fixed32(p) }
func DecodeReset(p []byte) (uint32, error)  { return fixed32(p) }

func DecodeAck(p []byte) (uint64, error) {
	if len(p) != 8 {
		return 0, fmt.Errorf("%w: ack payload %d bytes", ErrBadFrame, len(p))
	}
	return binary.BigEndian.Uint64(p), nil
}

func DecodePing(p []byte) (time.Time, error) {
	v, err := DecodeAck(p)
	return time.Unix(0, int64(v)), err
}

func fixed32(p []byte) (uint32, error) {
	if len(p) != 4 {
		return 0, fmt.Errorf("%w: payload %d bytes, want 4", ErrBadFrame, len(p))
	}
	return binary.BigEndian.Uint32(p), nil
}

// Marshal and Unmarshal wrap the JSON control payloads.
func Marshal(v any) ([]byte, error) { return json.Marshal(v) }

func Unmarshal(p []byte, v any) error {
	if err := json.Unmarshal(p, v); err != nil {
		return fmt.Errorf("%w: %v", ErrBadFrame, err)
	}
	return nil
}
