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
		// PerRunCPUMillis and PerRunMemoryMiB are the most CPU (milli-CPU) and memory (MiB) one run may
		// take on this runner, from the limits its owner or administrator declared (wardyn runner set), so
		// a runner can size what it builds for one run (a macOS VM). Re-sent on every connect; 0 or absent
		// (no limits declared, or an older organisation) means the runner sizes as it did before; an older
		// runner ignores them.
		PerRunCPUMillis int64 `json:"per_run_cpu_millis,omitempty"`
		PerRunMemoryMiB int64 `json:"per_run_memory_mib,omitempty"`
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
	// EventWorkload carries a WorkloadReport: a WardynWorkload custom resource the runner saw
	// change, or the names a namespace still holds. Only a runner that lists workload namespaces
	// sends it.
	EventWorkload = "workload"
)

// EventKinds is the closed list.
var EventKinds = []string{EventCaps, EventPosture, EventWaiting, EventAgentExit, EventActionResult, EventSpool, EventResidentErase, EventWorkload}

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
	// LocalModelPorts are the laptop local model ports the person allows on this runner
	// (--local-model-port): a run whose model server is on another port is refused before it is sent.
	LocalModelPorts []uint16 `json:"local_model_ports,omitempty"`
	Version         string   `json:"version"`
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
	// Folders says the runner serves list_folders and sync_folder: a folder-only
	// runner (0.10 on). Absent (an older runner) reads as none, so a person sees
	// "no runner on your computer" rather than a refused call.
	Folders bool `json:"folders,omitempty"`
	// LocalAttach says the runner serves MethodLocalAttach, MethodLocalAttachMode and
	// MethodLocalAttachTeeAck. The org sends them only to a runner that says so; an older runner
	// reads as absent and the org refuses a local attach before any call.
	LocalAttach bool `json:"local_attach,omitempty"`
	// LinuxVM says the runner starts Linux VM sandboxes (create_vm) and names the virtual machine it
	// gives them. Present only when its operator turned VMs on and its start-up probe passed; an older
	// runner never sends it, and the org asks no runner without it for a VM.
	LinuxVM *VMCaps `json:"linux_vm,omitempty"`
	// MacOSVM says the runner starts macOS VM sandboxes (create_vm with machine_kind macos_vm) and what
	// its Mac can serve now. Present only when its operator turned them on and the Mac passed its probe;
	// it is resent when it changes, as Relay is. An older runner never sends it and the org never sends
	// such a runner a macOS VM; an older org ignores it.
	MacOSVM *MacVMCaps `json:"macos_vm,omitempty"`
	// OfflineSummary is why the runner could not connect (or start) since its last session, sent once
	// on the first connect after; Relay is a Docker runner's latest run relay check, sent again when it
	// changes. Both 0.10 on; an older organisation ignores them, and an older runner sends neither.
	OfflineSummary *types.RunnerOfflineSummary `json:"offline_summary,omitempty"`
	Relay          *types.RunnerRelayStatus    `json:"relay,omitempty"`
	// ProxySettings names the run proxy settings (ProxySetting*) the runner carries through to its
	// proxy (0.10 on). An older runner rebuilds the proxy config without them, so the org refuses a
	// create that sets one the runner does not name; absent reads as none.
	ProxySettings []string `json:"proxy_settings,omitempty"`
	// RunsNamespaces are the Kubernetes namespaces a k8s runner places run pods in, the first being
	// its default. The org sends a create naming a namespace only to a runner that lists it, and a
	// pool member pinned to a namespace is eligible only on such a runner. Absent (an older runner,
	// or a runner on another substrate) says nothing: the runner serves its one namespace as before.
	RunsNamespaces []string `json:"runs_namespaces,omitempty"`
}

// The run proxy settings a runner names in Caps.ProxySettings, each its proxy.Config key.
const (
	ProxySettingAWSSigV4       = "aws_sigv4"
	ProxySettingScrubResponses = "scrub_responses"
	ProxySettingVM             = "vm"
	ProxySettingPortRule       = "port_rule"
	ProxySettingLocalModels    = "local_models"
	ProxySettingKubeAPI        = "kube_api"
	ProxySettingCanonicalRepos = "canonical_push_repos"
)

// ProxySettingLocalModelRelay is no proxy.Config field: a Docker runner names it when it relays a
// laptop local model (LocalModelSettings.AllowOnLaptopRunners) from its own computer to its run's proxy.
// The org refuses such a create on a runner that does not name it (LocalModelRelayNeeded); it is not in
// ProxySettings, which every runner substrate carries.
const ProxySettingLocalModelRelay = "local_model_relay"

// LocalModelRelayNeeded reports whether pc has a laptop local model, which only a runner naming
// ProxySettingLocalModelRelay serves.
func LocalModelRelayNeeded(pc runner.ProxyConfig) bool {
	for _, s := range pc.LocalModels {
		if s.AllowOnLaptopRunners {
			return true
		}
	}
	return false
}

// ProxySettings is every run proxy setting this build's runner carries through to its proxy, in the
// order ProxySettingsSet tests them.
var ProxySettings = []string{ProxySettingAWSSigV4, ProxySettingScrubResponses, ProxySettingVM, ProxySettingPortRule, ProxySettingLocalModels, ProxySettingKubeAPI, ProxySettingCanonicalRepos}

// ProxySettingsSet names each of ProxySettings that pc sets, in that order; none for an unset config.
// Nothing sets VM yet (the Docker driver will, where the desktop relay starts), so the org leaves it false; its gate stays.
func ProxySettingsSet(pc runner.ProxyConfig) []string {
	var out []string
	for i, on := range []bool{pc.AWSSigV4 != nil, pc.ScrubResponses, pc.VM, pc.PortRule != "", len(pc.LocalModels) != 0, pc.KubeAPI != nil, pc.CanonicalPushRepos} {
		if on {
			out = append(out, ProxySettings[i])
		}
	}
	return out
}

// VMCaps is the machine a runner's Linux VM sandboxes get: the versioned machine type, the named CPU
// model and the QEMU version its probe measured. Virtiofs says its probe found virtiofsd, so a VM
// can share a run's folder mounts; a runner without it (or older than the flag) takes no VM run that
// carries one.
type VMCaps struct {
	MachineType string `json:"machine_type"`
	CPUModel    string `json:"cpu_model"`
	QEMUVersion string `json:"qemu_version"`
	Virtiofs    bool   `json:"virtiofs,omitempty"`
	// Desktop says the runner opens a VM's display (dial_desktop). A runner without it is never asked.
	Desktop bool `json:"desktop,omitempty"`
	// SignIns says the runner hands each VM its sign-in store and pauses its agent (pause_agent,
	// resume_agent) (#2972). A runner without it is never asked.
	SignIns bool `json:"sign_ins,omitempty"`
}

// MacVMCaps is what a Mac runner's macOS VM sandboxes can be given right now. The org reads it through
// one function (macOSVMOffer), so no surface keeps a readiness story of its own.
type MacVMCaps struct {
	// Max is this runner's allowance: its own live macOS VM runs plus the host's free headroom, the
	// smallest of the licence cap (2) left, the VMs at the per-run limit that fit the memory budget and
	// those that fit the disk reservation. Other logins' VMs, prepare and a leaked slot are already
	// out of the headroom. The org compares it with the runner's own live count (placement refuses at
	// live >= Max); the host ledger, not Max, is the authority at admission.
	Max int `json:"max"`
	// MinCPUs and MinMemoryMiB are the base image's minimum guest size; 0 when it has not said.
	MinCPUs      int `json:"min_cpus,omitempty"`
	MinMemoryMiB int `json:"min_memory_mib,omitempty"`
	// Harnesses are the harness ids the base can serve now.
	Harnesses []string `json:"harnesses,omitempty"`
	// Desktop is MacVMDesktop*: whether the guest's screen can be controlled, only watched, or neither.
	Desktop string `json:"desktop,omitempty"`
	// SignIns says the runner keeps a macOS desktop's sign-ins and pauses its agent (pause_agent,
	// resume_agent). A runner without it is never asked.
	SignIns bool `json:"sign_ins,omitempty"`
	// Folders says the runner shares a run's folders into its macOS VM. A runner without it is never
	// sent a macOS VM run with a mount.
	Folders bool `json:"folders,omitempty"`
	// Drives says the runner carries a run's admin and team drives (a host folder as a second place)
	// into its macOS VM as shared folders. A runner without it is never sent a macOS VM run with a drive.
	Drives bool      `json:"drives,omitempty"`
	Base   MacVMBase `json:"base"`
	// DiskDrives says the runner keeps a person's disk drives and kept work folders as disk images on
	// this Mac, attaches them to its macOS VM, and deletes them (reclaim_drive). A runner without it is
	// never sent a macOS VM run with one.
	DiskDrives bool `json:"disk_drives,omitempty"`
	// Refusal is, in words, why the runner offers no macOS VM; empty when it offers them.
	Refusal string `json:"refusal,omitempty"`
	// Reason is why the runner offers no macOS VM, or can't start or prepare one now: MacVMReason*, the
	// first that holds in the const order. Refusal keeps its words for placement.
	Reason string `json:"reason,omitempty"`
	// NoSession: the runner is not in a GUI login session, where Apple's framework runs. A boolean,
	// nothing else.
	NoSession bool `json:"no_session,omitempty"`
	// HostOSVersion is this Mac's macOS, e.g. "26.4"; with Reason host_os_too_old only.
	HostOSVersion string `json:"host_os_version,omitempty"`
	// HostReason is MacVMHost*; with Reason host_unsupported only.
	HostReason string `json:"host_reason,omitempty"`
	// FreeGB and NeedGB are whole GiB: free disk (disk_short), or free and needed memory (low_memory).
	// Absent is 0.
	FreeGB int `json:"free_gb,omitempty"`
	NeedGB int `json:"need_gb,omitempty"`
}

// MacVMCaps.Reason values, in priority order. An unknown value is a newer runner's, and decodes as itself.
const (
	MacVMReasonLedgerDamaged      = "ledger_damaged"
	MacVMReasonNoHelper           = "no_helper"
	MacVMReasonHostOSTooOld       = "host_os_too_old"
	MacVMReasonHostUnsupported    = "host_unsupported"
	MacVMReasonMemoryBelowMinimum = "memory_below_minimum"
	MacVMReasonCPUBelowMinimum    = "cpu_below_minimum"
	MacVMReasonDiskShort          = "disk_short"
	MacVMReasonSlotLeaked         = "slot_leaked"
	MacVMReasonLowMemory          = "low_memory"
	MacVMReasonTooHot             = "too_hot"
)

// MacVMCaps.HostReason values.
const (
	MacVMHostNoVirtualization = "no_virtualization"
	MacVMHostNoProvisioning   = "no_provisioning"
	MacVMHostHelperBuild      = "helper_build"
)

// MacVMCaps.Desktop values.
const (
	MacVMDesktopControl = "control"
	MacVMDesktopObserve = "observe"
	MacVMDesktopNone    = "none"
)

// MacVMBase is the Mac's one base image: State is MacVMBase*, GuestOS the build it holds, Progress the
// percent done while it prepares, Error the last failure. Error is the one free text: this login's home
// is in it as "~", and with a Failure it holds the cause only.
type MacVMBase struct {
	State    string `json:"state"`
	GuestOS  string `json:"guest_os,omitempty"`
	Progress int    `json:"progress,omitempty"`
	Error    string `json:"error,omitempty"`
	// Step is MacVMStep*; state preparing or failed only.
	Step string `json:"step,omitempty"`
	// StepPercent is 0-100 within Step; download and install only. Absent is 0.
	StepPercent int `json:"step_percent,omitempty"`
	// Wait is MacVMWait*; state preparing only.
	Wait string `json:"wait,omitempty"`
	// Failure is MacVMFailure*: download, install or no_desktop with state failed, tools with state ready.
	Failure string `json:"failure,omitempty"`
	// DownloadBytes is the restore image's size, once this prepare knows it.
	DownloadBytes int64 `json:"download_bytes,omitempty"`
	// ImageVersion is its macOS, e.g. "27.0.1": digits and dots only (^\d+(\.\d+){0,2}$), or absent.
	ImageVersion string `json:"image_version,omitempty"`
	// BytesPerSec is the download rate; Step download with no Wait only.
	BytesPerSec int64 `json:"bytes_per_sec,omitempty"`
}

// MacVMBase.Step, Wait and Failure values. An unknown value is a newer runner's, and decodes as itself.
const (
	MacVMStepCheck    = "check"
	MacVMStepDownload = "download"
	MacVMStepInstall  = "install"
	MacVMStepSetup    = "setup"
	MacVMStepTools    = "tools"

	MacVMWaitACPower       = "ac_power"
	MacVMWaitBattery       = "battery"
	MacVMWaitDownloadRetry = "download_retry"
	MacVMWaitSetupByHand   = "setup_by_hand"
	MacVMWaitDocker        = "docker"

	MacVMFailureDownload = "download"
	MacVMFailureInstall  = "install"
	MacVMFailureTools    = "tools"
	// MacVMFailureNoDesktop is a setup by hand the helper refused because it cannot reach the Mac's desktop.
	MacVMFailureNoDesktop = "no_desktop"
)

// MacVMBase.State values. Only ready serves a run; update_needed is a base whose guest agent no longer
// speaks the runner's session protocol.
const (
	MacVMBaseNone         = "none"
	MacVMBasePreparing    = "preparing"
	MacVMBaseReady        = "ready"
	MacVMBaseUpdateNeeded = "update_needed"
	MacVMBaseFailed       = "failed"
)

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
	KindDesktop    = "desktop"
	// KindFolder is the sftp stream of one sync_folder pass.
	KindFolder = "folder"
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
	case KindPTY, KindExec, KindExecStderr, KindRelay, KindOutput, KindFolder, KindDesktop, KindPTYTee:
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
