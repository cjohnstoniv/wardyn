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

// RecoverResult names the `output` stream a recover_output call opened; the
// runner sets Unrecoverable instead when it holds no copy.
type RecoverResult struct {
	Stream        uint32 `json:"stream,omitempty"`
	Unrecoverable bool   `json:"unrecoverable,omitempty"`
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
	// ReportID is the erasure's stable identity: a report resent after a lost
	// acknowledgement carries the same one. Set only on an erased outcome.
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
