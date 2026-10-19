// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// CALL method names. The list mirrors substrate.Substrate and the optional
// interfaces implemented in 0.9; Classes has no call (it is served from the
// last `caps` event) and ExecOutput is a runner-opened `output` stream.
const (
	MethodCreateSandbox   = "create_sandbox"
	MethodExec            = "exec"
	MethodExecutionResult = "execution_result"
	MethodWait            = "wait"
	MethodAttach          = "attach"
	MethodResize          = "resize"
	MethodExecStream      = "exec_stream"
	MethodExecWait        = "exec_wait"
	MethodExecClose       = "exec_close"
	MethodStatus          = "status"
	MethodAgentStatus     = "agent_status"
	MethodStop            = "stop"
	MethodKill            = "kill"
	MethodEnd             = "end"
	MethodStopProxy       = "stop_proxy"
	MethodCanReplace      = "can_replace_proxy"
	MethodReplaceProxy    = "replace_proxy"
	MethodEnsureProxyImg  = "ensure_proxy_image"
	MethodStart           = "start"
	MethodProbeDrive      = "probe_drive"
	MethodRecoverOutput   = "recover_output"
	MethodOutputAck       = "output_ack"
	MethodSweep           = "sweep_orphans"
	MethodDeliverResident = "deliver_resident"
	MethodEraseResident   = "erase_resident"
	// MethodSelftest runs a BYOI image's contract check on a runner that
	// advertises Caps.BYOISelftest: never an Exec, so it spends no generation.
	MethodSelftest = "byoi_selftest"
	// The local attach calls are sent only to a runner that advertises Caps.LocalAttach.
	// MethodLocalAttach opens a pty_tee stream and answers its id; MethodLocalAttachMode changes the
	// session's writer/observer mode and is answered once the runner has applied it;
	// MethodLocalAttachTeeAck acknowledges the tee bytes the org has handed to its recorder.
	MethodLocalAttach       = "local_attach"
	MethodLocalAttachMode   = "local_attach_mode"
	MethodLocalAttachTeeAck = "local_attach_tee_ack"
)

// The tee window of a local attach session whose Tee is true. The runner shows at most
// LocalAttachTeeWindowBytes of output ahead of the org's last acknowledgement, stops writing to the
// CLI while that window is full, and ends the session when no acknowledgement arrives within
// LocalAttachTeeWindowTimeout, so a stalled recorder cannot let unrecorded output reach the screen.
// With Tee false there are no acknowledgements and no window. The timeout matches the api package's
// attachLeaseCallTimeout, which this package cannot import.
const (
	LocalAttachTeeWindowBytes                = 64 << 10
	LocalAttachTeeWindowTimeout time.Duration = 2 * time.Second
)

// CreateSandboxArgs carries the spec minus OnWaiting and ExecOutput (both are
// json:"-"). SECURITY: it never carries a runner_resident value; the spec holds
// a placeholder naming the grant, and the value travels only in DeliverResident.
type CreateSandboxArgs struct {
	Spec runner.SandboxSpec `json:"spec"`
	// ExecOutput says the org wants the agent's output: the runner opens an
	// `output` stream named by the run id (the writer itself cannot cross the wire).
	ExecOutput bool `json:"exec_output,omitempty"`
}

type CreateSandboxResult struct {
	Sandbox runner.Sandbox `json:"sandbox"`
}

type RefArgs struct {
	ExecutionGeneration uuid.UUID `json:"execution_generation,omitempty"`
	Ref                 string    `json:"ref"`
}

type ExecArgs struct {
	ExecutionGeneration uuid.UUID `json:"execution_generation"`
	Ref                 string    `json:"ref"`
	Argv                []string  `json:"argv"`
}

// SelftestArgs carries no argv: the runner execs only its own selftest. The
// generation is the task's issued, unattempted one, which the call must match
// and never spends.
type SelftestArgs struct {
	ExecutionGeneration uuid.UUID `json:"execution_generation"`
	Ref                 string    `json:"ref"`
}

type SelftestResult struct {
	ExitCode int `json:"exit_code"`
}

type ExecResult struct {
	ExecID string `json:"exec_id"`
}

type WaitResult struct {
	ExitCode int `json:"exit_code"`
}

type AttachArgs struct {
	Ref     string               `json:"ref"`
	Options runner.AttachOptions `json:"options"`
}

// StreamResult names the byte stream a call opened (the pty stream of Attach).
type StreamResult struct {
	Stream uint32 `json:"stream"`
}

type ResizeArgs struct {
	Stream uint32 `json:"stream"`
	Cols   uint16 `json:"cols"`
	Rows   uint16 `json:"rows"`
}

type ExecStreamArgs struct {
	Ref  string          `json:"ref"`
	Spec runner.ExecSpec `json:"spec"`
}

// ExecStreamResult names the two streams of one exec; stdin half-close is CLOSE on Exec.
type ExecStreamResult struct {
	Exec   uint32 `json:"exec"`
	Stderr uint32 `json:"stderr,omitempty"`
}

type ExecWaitArgs struct {
	Stream uint32 `json:"stream"`
}

type AgentStatusArgs struct {
	Ref         string `json:"ref"`
	AgentExecID string `json:"agent_exec_id"`
}

type ReplaceProxyArgs struct {
	Ref     string `json:"ref"`
	CfgJSON []byte `json:"cfg_json"`
}

// SweepArgs is the crash-orphan sweep. A func cannot cross the wire, so it runs
// in two calls: with Remove nil the runner answers the run ids older than
// MinAge it holds a sandbox for; the org keeps those it no longer tracks and
// sends them back as Remove, and the runner removes exactly those.
type SweepArgs struct {
	MinAge time.Duration `json:"min_age"`
	Remove []uuid.UUID   `json:"remove,omitempty"`
}

type SweepResult struct {
	Candidates []uuid.UUID `json:"candidates,omitempty"`
	Removed    int         `json:"removed"`
}

// RecoverOutputArgs are recover_output's arguments. Gap says the org takes a copy whose first bytes
// the runner has already discarded; without it (an older org) such a copy is Unrecoverable.
type RecoverOutputArgs struct {
	Ref string `json:"ref"`
	Gap bool   `json:"gap,omitempty"`
}

// RecoverResult names the `output` stream a recover_output call opened; the
// runner sets Unrecoverable instead when it holds no copy. Gap is the offset
// the stream starts at when the runner had already discarded the bytes before
// it; it is set only when the call asked for Gap.
type RecoverResult struct {
	Stream        uint32 `json:"stream,omitempty"`
	Unrecoverable bool   `json:"unrecoverable,omitempty"`
	Gap           int64  `json:"gap,omitempty"`
}

// DeliverResidentArgs is the runner-only call that hands one runner_resident
// value to the runner's keystore, after claim and before CreateSandbox.
type DeliverResidentArgs struct {
	RunID    uuid.UUID `json:"run_id"`
	GrantID  uuid.UUID `json:"grant_id"`
	Kind     string    `json:"kind"`
	Value    string    `json:"value"`
	NotAfter time.Time `json:"not_after"`
	// Sandbox lets the runner place the value in SecretEnv or ManagedFiles;
	// false keeps it in the runner's own proxy configuration only.
	Sandbox bool `json:"sandbox,omitempty"`
}

type EraseResidentArgs struct {
	RunID uuid.UUID `json:"run_id"`
}

// ResidentErase is the `resident_erase` event's data: one grant's erase at run end.
type ResidentErase struct {
	GrantID uuid.UUID `json:"grant_id"`
	Outcome string    `json:"outcome"`
	// ReportID identifies this outcome of the grant: a report resent after a
	// lost acknowledgement carries the same one, and each new outcome a fresh one.
	ReportID uuid.UUID `json:"report_id,omitzero"`
}

// ResidentErase outcomes.
const (
	ResidentErased      = "erased"
	ResidentEraseFailed = "failed"
)

// OutputAckArgs acknowledges bytes accepted by the org's output writer, never
// merely bytes received in the transport's buffer. Offset is the next byte.
type OutputAckArgs struct {
	RunID  uuid.UUID `json:"run_id"`
	Offset int64     `json:"offset"`
}

// ExecutionResultArgs requests reconciliation of one exact org-issued intent.
type ExecutionResultArgs struct {
	Ref        string    `json:"ref"`
	Generation uuid.UUID `json:"generation"`
}

// ExecutionResult reports durable local state; it never starts or retries work.
type ExecutionResult struct {
	Containment     *types.RunnerMutationFailure `json:"containment,omitempty"`
	MutationFailure *types.RunnerMutationFailure `json:"mutation_failure,omitempty"`
	Generation      uuid.UUID                    `json:"generation"`
	Operation       string                       `json:"operation"`
	ExecID          string                       `json:"exec_id"`
	Attempted       bool                         `json:"attempted"`
}

// LocalAttachArgs asks the runner to attach the CLI that holds Nonce (granted on the runner's local
// socket) to ref's sandbox. Tee says the org records the run: the runner then sends every output
// chunk on the pty_tee stream before it shows it. With Tee false the stream carries no bytes and only
// lets the org end the session.
type LocalAttachArgs struct {
	Ref     string               `json:"ref"`
	Nonce   string               `json:"nonce"`
	Options runner.AttachOptions `json:"options"`
	Tee     bool                 `json:"tee,omitempty"`
}

// LocalAttachModeArgs sets the writer or observer mode of the session whose pty_tee stream is Stream.
// Cols and Rows are the window size when the session becomes a writer.
type LocalAttachModeArgs struct {
	Stream   uint32 `json:"stream"`
	Observer bool   `json:"observer"`
	Cols     uint16 `json:"cols,omitempty"`
	Rows     uint16 `json:"rows,omitempty"`
}

// LocalAttachTeeAckArgs acknowledges that the org has handed Offset bytes (cumulative) of the
// session's pty_tee stream to its recorder.
type LocalAttachTeeAckArgs struct {
	Stream uint32 `json:"stream"`
	Offset int64  `json:"offset"`
}
