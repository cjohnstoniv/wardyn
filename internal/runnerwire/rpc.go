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
	// MethodListFolders, MethodSyncFolder and MethodSyncFolderWait are a
	// folder-only runner's (Caps.Folders): the folders under its roots, and one
	// `wardyn sync` pass between one of them and a run's sandbox.
	MethodListFolders    = "list_folders"
	MethodSyncFolder     = "sync_folder"
	MethodSyncFolderWait = "sync_folder_wait"
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

// ListFoldersArgs asks for the folders in Path, an absolute path under one of
// the runner's roots; empty lists the roots themselves. Survey also asks what a
// pass from Path would send, under the organisation's Deny patterns.
type ListFoldersArgs struct {
	Path   string   `json:"path,omitempty"`
	Survey bool     `json:"survey,omitempty"`
	Deny   []string `json:"deny,omitempty"`
}

// FolderList is list_folders' result. Only folder names, counts and sizes
// cross, and the names Survey.LeftOut lists: never another file name.
type FolderList struct {
	Path    string        `json:"path"`
	Folders []FolderEntry `json:"folders"`
	Survey  *FolderSurvey `json:"survey,omitempty"`
}

// FolderEntry is one folder. More says the count stopped at its bound, so
// Files and Bytes are at least what they say.
type FolderEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
	More  bool   `json:"more,omitempty"`
}

// FolderSurvey is what a pass from a folder would send, after every rule:
// LeftOut names the top-level paths the rules always leave out (.git and the
// like), and Ignored counts what the ignore files leave out.
type FolderSurvey struct {
	Files   int      `json:"files"`
	Bytes   int64    `json:"bytes"`
	LeftOut []string `json:"left_out"`
	Ignored int      `json:"ignored"`
}

// SyncFolderArgs starts one pass between Path on the runner and RemoteDir in
// run RunID's sandbox. The runner opens a `folder` stream for the call and
// speaks sftp over it as the client; the org joins that stream to the
// sandbox's own sftp-server. Pull also brings the sandbox's changes back,
// under the `wardyn sync --pull` rules; Deny is the organisation's patterns.
type SyncFolderArgs struct {
	RunID     uuid.UUID `json:"run_id"`
	Path      string    `json:"path"`
	RemoteDir string    `json:"remote_dir"`
	Pull      bool      `json:"pull,omitempty"`
	Deny      []string  `json:"deny,omitempty"`
}

// SyncFolderWaitArgs waits for the pass on Stream to end.
type SyncFolderWaitArgs struct {
	Stream uint32 `json:"stream"`
}

// FolderPass is one pass's outcome, as the runner counts it. Refused holds at
// most MaxFolderRefusals paths. Error is set when the pass did not complete.
type FolderPass struct {
	Pushed    int             `json:"pushed"`
	Pulled    int             `json:"pulled"`
	Conflicts int             `json:"conflicts"`
	Refused   []FolderRefusal `json:"refused"`
	Ignored   int             `json:"ignored"`
	BytesOut  int64           `json:"bytes_out"`
	BytesIn   int64           `json:"bytes_in"`
	Error     string          `json:"error,omitempty"`
}

// FolderRefusal is one path a pass refused and why.
type FolderRefusal struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// MaxFolderRefusals bounds FolderPass.Refused.
const MaxFolderRefusals = 50

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
