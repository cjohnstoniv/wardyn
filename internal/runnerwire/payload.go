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
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/types"
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
		// LastSeq is the org's received cursor; the runner replays after it.
		LastSeq uint64 `json:"last_seq"`
		// ProxyImage is the egress-proxy image the organisation runs for this runner's architecture, so
		// an upgraded organisation's image reaches the runner without a reinstall. Empty when the
		// organisation names none; an older runner ignores the field.
		ProxyImage string `json:"proxy_image,omitempty"`
		// LocalAttach says the organisation allows a person's CLI on the runner's own computer to attach
		// to a run over the runner's local socket. Absent (an older organisation) or false: the runner
		// opens no socket; an older runner ignores the field.
		LocalAttach bool `json:"local_attach,omitempty"`
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
	// MachineName is the host's own name (os.Hostname), stored as the runner's machine_name.
	MachineName string `json:"machine_name,omitempty"`
	// Arch is the Docker engine's CPU architecture ("amd64" or "arm64"), stored as the runner's arch;
	// omitted when the engine could not be asked.
	Arch string `json:"arch,omitempty"`
	// BYOISelftest says the runner serves MethodSelftest; absent (an older
	// runner) refuses a BYOI task run rather than skip its check.
	BYOISelftest bool `json:"byoi_selftest,omitempty"`
	// Retire says the runner settles a retire_run receipt that carries
	// no_output (0.9.3 on). The org queues retire_run only to a runner that
	// says so; an older runner keeps every run's output, as before.
	Retire bool `json:"retire,omitempty"`
	// Resident says the runner holds runner_resident credentials: it serves
	// deliver_resident and fills a ResidentMints placeholder (0.9.3 on). The org
	// reads and sends a resident value only for a runner that says so; an older
	// runner's run with one is refused before any value is read.
	Resident bool `json:"resident,omitempty"`
	// EndsUnkept says the runner's substrate cannot keep an ended sandbox
	// (it is no runner.SandboxEnder: Kubernetes deletes a stopped pod). Absent
	// (a runner older than 0.9.4) reads as keeps, as before.
	EndsUnkept bool `json:"ends_unkept,omitempty"`
	// LocalAttach says the runner serves MethodLocalAttach, MethodLocalAttachMode and
	// MethodLocalAttachTeeAck. The org sends them only to a runner that says so; an older runner
	// reads as absent and the org refuses a local attach before any call.
	LocalAttach bool `json:"local_attach,omitempty"`
}

// Waiting is the `waiting` event's data.
type Waiting struct {
	Detail string `json:"detail"`
}

// AgentExit is the durable `agent_exit` event's data.
type AgentExit struct {
	Generation uuid.UUID `json:"generation"`
	ExecID     string    `json:"exec_id"`
	Ref        string    `json:"ref"`
	Code       int       `json:"code"`
	ObservedAt time.Time `json:"observed_at"`
}

// Lease is the LEASE payload, sent after each renew or extend.
// Zero EndsAt means authenticated no-end policy; token lapse is always finite.
type Lease struct {
	ExecutionGeneration uuid.UUID `json:"execution_generation"`
	RunID               uuid.UUID `json:"run_id"`
	TokenRenewedAt      time.Time `json:"token_renewed_at"`
	EndsAt              time.Time `json:"ends_at"`
}

// PendingAction is one entry of the PENDING payload, oldest first.
type PendingAction struct {
	Retirement *types.RunnerRetirement      `json:"retirement,omitempty"`
	Quarantine *types.RunnerQuarantine      `json:"quarantine,omitempty"`
	Recovery   *types.RunnerMutationFailure `json:"recovery,omitempty"`
	ID         uuid.UUID                    `json:"id"`
	Kind       types.RunnerActionKind       `json:"kind"`
	RunID      uuid.UUID                    `json:"run_id"`
	Ref        string                       `json:"ref"`
	// NodeRecord, on a stop_proxy action, is a security operator's "node powered off" or "no prior
	// holder" record for a k8s runner's held run (design K1 §3.1 (b)). The runner checks it against
	// its own record and its acceptance list, and refuses it with the reason otherwise.
	NodeRecord *runner.NodeRecord `json:"node_record,omitempty"`
}

// MaxActionResultBytes bounds one encoded action_result body; the org refuses
// a larger one. MaxRefusalReason bounds a refused result's reason.
const (
	MaxActionResultBytes = 1024
	MaxRefusalReason     = 512
)

// ActionResult is the `action_result` event's data.
type ActionResult struct {
	ID         uuid.UUID `json:"id"`
	Outcome    string    `json:"outcome"`
	ObservedAt time.Time `json:"observed_at"`
	Reason     string    `json:"reason,omitempty"` // why a refused action was refused
}

// RunState is one run's entry in the STATE payload; all runner_asserted.
type RunState struct {
	SpoolReceipt    *types.RunnerSpoolReceipt    `json:"spool_receipt,omitempty"`
	Retirement      *types.RunnerRetirement      `json:"retirement,omitempty"`
	Containment     *types.RunnerMutationFailure `json:"containment,omitempty"`
	ContainmentOnly bool                         `json:"containment_only,omitempty"`
	MutationFailure *types.RunnerMutationFailure `json:"mutation_failure,omitempty"`
	Execution       *ExecutionResult             `json:"execution,omitempty"`
	RunID           uuid.UUID                    `json:"run_id"`
	// Ref and Generation are the runner's own identity for the run, asserted
	// evidence only. The org uses them solely to contain a run it has no row
	// for; they never bind or authorize anything for a known run.
	Ref            string     `json:"ref,omitempty"`
	Generation     uuid.UUID  `json:"generation"`
	SandboxPresent bool       `json:"sandbox_present"`
	AgentState     string     `json:"agent_state,omitempty"`
	ExitCode       *int       `json:"exit_code,omitempty"`
	ProxyStoppedAt *time.Time `json:"proxy_stopped_at,omitempty"`
	SpoolDepth     int        `json:"spool_depth"`
	SpoolDrops     int        `json:"spool_drops"`
	ResidentGrants []string   `json:"resident_grants,omitempty"`
	// ContainmentPending is set while a k8s runner holds the run without completion evidence for its
	// proxy (design K1 §3.1 Q2). It is never plain containment: ProxyStoppedAt stays unset.
	ContainmentPending *ContainmentPending `json:"containment_pending,omitempty"`
}

// ContainmentPending is RunState's containment-pending report.
type ContainmentPending struct {
	Kind  string    `json:"kind"` // runner.ContainmentAgentOnly | runner.ContainmentPendingDelete
	Since time.Time `json:"since"`
	// NameReused: run detail shows "node name reused" and only a two-person "node powered off"
	// record releases the run.
	NameReused bool `json:"name_reused,omitempty"`
	// NoCausalAssociation: run detail shows "no causal association"; a record needs two humans.
	NoCausalAssociation bool `json:"no_causal_association,omitempty"`
	// Tuple is what both approvers of a node record are shown and what the record must name.
	Tuple *runner.NodeTuple `json:"tuple,omitempty"`
}

// Byte-stream kinds carried by OPEN.
const (
	KindPTY        = "pty"
	KindExec       = "exec"
	KindExecStderr = "exec_stderr"
	KindRelay      = "relay"
	KindOutput     = "output"
	// KindPTYTee carries a local attach session's output from the runner to the org's recorder. It is
	// opened only by the runner, only as the reply stream of a local_attach call, and carries bytes
	// in that one direction: the org's writes are reset. The org ends the session by resetting it.
	KindPTYTee = "pty_tee"
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
	case KindPTY, KindExec, KindExecStderr, KindRelay, KindOutput, KindPTYTee:
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
	ResetCapacity   uint32 = 6
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
