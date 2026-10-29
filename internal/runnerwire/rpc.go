// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
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
	// MethodCreateVM creates a Linux VM sandbox on a runner whose caps carry LinuxVM. It is its own
	// method so a runner that predates VMs refuses it instead of reading it as create_sandbox and
	// starting a container; every later verb on the VM's ref is the container one.
	MethodCreateVM = "create_vm"
	// MethodListFolders, MethodSyncFolder and MethodSyncFolderWait are a
	// folder-only runner's (Caps.Folders): the folders under its roots, and one
	// `wardyn sync` pass between one of them and a run's sandbox.
	MethodListFolders    = "list_folders"
	MethodSyncFolder     = "sync_folder"
	MethodSyncFolderWait = "sync_folder_wait"
	// MethodDialDesktop opens a `desktop` stream to a VM sandbox's display (QEMU's RFB socket) on a
	// runner whose caps carry LinuxVM.Desktop; an older runner never gets the call.
	MethodDialDesktop = "dial_desktop"
	// MethodPauseAgent and MethodResumeAgent stop and continue a VM sandbox's agent inside the guest
	// (runner.AgentPauser, AgentPauseArgs), on a runner whose caps carry LinuxVM.SignIns; an older one
	// never gets them.
	MethodPauseAgent  = "pause_agent"
	MethodResumeAgent = "resume_agent"
	// The local attach calls are sent only to a runner that advertises Caps.LocalAttach.
	// MethodLocalAttach opens a pty_tee stream and answers its id; MethodLocalAttachMode changes the
	// session's writer/observer mode and is answered once the runner has applied it;
	// MethodLocalAttachTeeAck acknowledges the tee bytes the org has handed to its recorder.
	MethodLocalAttach       = "local_attach"
	MethodLocalAttachMode   = "local_attach_mode"
	MethodLocalAttachTeeAck = "local_attach_tee_ack"
	// MethodWorkloadStatus patches the status of one WardynWorkload custom resource from the org's
	// answer (WorkloadStatusArgs). It is sent only to a runner that reported a workload.
	MethodWorkloadStatus = "workload_status"
)

// The tee window of a local attach session whose Tee is true. The runner shows at most
// LocalAttachTeeWindowBytes of output ahead of the org's last acknowledgement, stops writing to the
// CLI while that window is full, and ends the session when no acknowledgement covers the oldest
// unacknowledged byte within LocalAttachTeeWindowTimeout of it being sent, so a stalled recorder
// cannot let unrecorded output reach the screen. An idle session with nothing unacknowledged never
// stalls. With Tee false there are no acknowledgements and no window. The timeout matches the api package's
// attachLeaseCallTimeout, which this package cannot import.
const (
	LocalAttachTeeWindowBytes                 = 64 << 10
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

// AgentPauseArgs is pause_agent's and resume_agent's. Generation is the run's pause generation, which
// the runner hands the guest agent (runner.AgentPauser). A runner that predates it reads only Ref, and a
// wardynd that predates it sends none, which the guest agent reads as 0 and signals as before.
type AgentPauseArgs struct {
	Ref        string `json:"ref"`
	Generation int64  `json:"generation,omitempty"`
}

// AgentPauseResult is their answer: FencedPause is the guest agent's. A runner that predates it
// answers nothing, which reads as false.
type AgentPauseResult struct {
	FencedPause bool `json:"fenced_pause,omitempty"`
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

// DialDesktopArgs names the VM sandbox whose display dial_desktop opens.
type DialDesktopArgs struct {
	Ref string `json:"ref"`
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
// most MaxFolderRefusals paths. Error is set when the pass did not complete; its
// file counts are then what it finished, and saved in its state, before it stopped.
type FolderPass struct {
	Pushed    int             `json:"pushed"`
	Pulled    int             `json:"pulled"`
	Conflicts int             `json:"conflicts"`
	Refused   []FolderRefusal `json:"refused"`
	Ignored   int             `json:"ignored"`
	BytesOut  int64           `json:"bytes_out"`
	BytesIn   int64           `json:"bytes_in"`
	// Unsynced is, on a pass back cut short, the sandbox's changed files it had not brought back.
	Unsynced int    `json:"unsynced,omitempty"`
	Error    string `json:"error,omitempty"`
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
	// MacOSVMDisks is every macOS VM disk report the runner still keeps; an older runner sends none.
	MacOSVMDisks []runner.MacOSVMDisk `json:"macos_vm_disks,omitempty"`
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

// LocalAttachTeeAckArgs acknowledges that the org has handed Offset bytes of the session's pty_tee
// stream to its recorder. Cumulative; an offset at or below the last ack is a no-op; above the bytes
// sent is refused.
type LocalAttachTeeAckArgs struct {
	Stream uint32 `json:"stream"`
	Offset int64  `json:"offset"`
}

// The WorkloadReport ops.
const (
	WorkloadOpApply  = "apply"
	WorkloadOpDelete = "delete"
	WorkloadOpSync   = "sync"
)

// Workload report bounds. MaxWorkloadSpecBytes is the most a custom resource's spec may take; the
// org refuses a larger one. MaxWorkloadSyncNames bounds a sync's name list.
const (
	MaxWorkloadSpecBytes = 256 << 10
	MaxWorkloadSyncNames = 1000
	MaxWorkloadMessage   = 512
)

// WorkloadReport is the `workload` event's data. Namespace is where the custom resource lives; the
// org reads it only as the key of a binding, never as the namespace a run is placed in.
//
//   - apply: Name, UID, Generation (metadata.generation) and Spec (the resource's spec, as written).
//   - delete: Name and UID.
//   - sync: Names, every workload the runner saw in Namespace after its informer synced; the org
//     deletes the workloads it holds for the namespace that the list omits.
type WorkloadReport struct {
	Op         string          `json:"op"`
	Namespace  string          `json:"namespace"`
	Name       string          `json:"name,omitempty"`
	UID        string          `json:"uid,omitempty"`
	Generation int64           `json:"generation,omitempty"`
	Spec       json.RawMessage `json:"spec,omitempty"`
	Names      []string        `json:"names,omitempty"`
}

var workloadLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Validate refuses a report outside the shape of its op.
func (r WorkloadReport) Validate() error {
	if !workloadLabel.MatchString(r.Namespace) {
		return fmt.Errorf("%w: workload namespace %q", ErrBadFrame, r.Namespace)
	}
	switch r.Op {
	case WorkloadOpApply, WorkloadOpDelete:
		if !workloadLabel.MatchString(r.Name) || r.UID == "" || len(r.Names) > 0 {
			return fmt.Errorf("%w: workload %s needs a name and uid, and no names", ErrBadFrame, r.Op)
		}
		if r.Op == WorkloadOpDelete {
			if r.Generation != 0 || len(r.Spec) > 0 {
				return fmt.Errorf("%w: workload delete carries no generation or spec", ErrBadFrame)
			}
			return nil
		}
		if r.Generation < 1 || len(r.Spec) == 0 || len(r.Spec) > MaxWorkloadSpecBytes {
			return fmt.Errorf("%w: workload apply needs a generation and a spec of at most %d bytes", ErrBadFrame, MaxWorkloadSpecBytes)
		}
	case WorkloadOpSync:
		if r.Name != "" || r.UID != "" || r.Generation != 0 || len(r.Spec) > 0 || len(r.Names) > MaxWorkloadSyncNames {
			return fmt.Errorf("%w: workload sync carries only a namespace and at most %d names", ErrBadFrame, MaxWorkloadSyncNames)
		}
		for _, n := range r.Names {
			if !workloadLabel.MatchString(n) {
				return fmt.Errorf("%w: workload name %q", ErrBadFrame, n)
			}
		}
	default:
		return fmt.Errorf("%w: unknown workload op %q", ErrBadFrame, r.Op)
	}
	return nil
}

// The phases of WorkloadStatusArgs, as a WardynWorkload's status.phase shows them.
const (
	WorkloadPhasePending         = "Pending"
	WorkloadPhaseQueued          = "Queued"
	WorkloadPhaseRunning         = "Running"
	WorkloadPhaseUnhealthy       = "Unhealthy"
	WorkloadPhaseCompleted       = "Completed"
	WorkloadPhaseEnded           = "Ended"
	WorkloadPhaseRefused         = "Refused"
	WorkloadPhaseInvalid         = "Invalid"
	WorkloadPhaseNotBound        = "NotBound"
	WorkloadPhaseBindingInactive = "BindingInactive"
)

// WorkloadPhases is the closed list.
var WorkloadPhases = []string{WorkloadPhasePending, WorkloadPhaseQueued, WorkloadPhaseRunning, WorkloadPhaseUnhealthy, WorkloadPhaseCompleted,
	WorkloadPhaseEnded, WorkloadPhaseRefused, WorkloadPhaseInvalid, WorkloadPhaseNotBound, WorkloadPhaseBindingInactive}

// WorkloadStatusArgs is workload_status's. The runner writes it to the resource's status
// subresource and nowhere else; the org ignores the status of every resource it reads.
type WorkloadStatusArgs struct {
	Namespace          string `json:"namespace"`
	Name               string `json:"name"`
	ObservedGeneration int64  `json:"observed_generation"`
	Phase              string `json:"phase"`
	RunID              string `json:"run_id,omitempty"`
	// Reason is a Wardyn reason token on Refused, Invalid and the like.
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Validate refuses a status outside the closed phase list or past its bounds.
func (a WorkloadStatusArgs) Validate() error {
	if !workloadLabel.MatchString(a.Namespace) || !workloadLabel.MatchString(a.Name) {
		return fmt.Errorf("%w: workload status for %q/%q", ErrBadFrame, a.Namespace, a.Name)
	}
	if !slices.Contains(WorkloadPhases, a.Phase) {
		return fmt.Errorf("%w: unknown workload phase %q", ErrBadFrame, a.Phase)
	}
	if a.ObservedGeneration < 0 || len(a.Reason) > MaxRefusalReason || len(a.Message) > MaxWorkloadMessage {
		return fmt.Errorf("%w: workload status out of bounds", ErrBadFrame)
	}
	return nil
}
