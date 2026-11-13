// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package groundtruth

import (
	"encoding/json"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/ipguard"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ── Minimal Tetragon JSON-export structs ────────────────────────────────────
//
// Only the fields we read are defined, against Tetragon's JSON export shape
// (one JSON object per line) — avoids pulling github.com/cilium/tetragon and
// its gRPC/protobuf graph into go.mod.
//
// Tetragon has no top-level "process_connect" kind: a TCP connect is a
// process_kprobe on a connect kprobe whose socket argument is a sock_arg
// (KprobeSock). Connects are routed through the kprobe handler accordingly.

// TetragonEvent is one line of the Tetragon JSON export; exactly one
// event-kind field is set. process_exec and process_kprobe are mapped; the
// kprobe handler maps file-writes and network connects by the hooked
// function and its arguments, and drops a probe from any other hook. Other
// kinds are ignored (ok=false).
type TetragonEvent struct {
	ProcessExec   *TetragonProcessExec   `json:"process_exec,omitempty"`
	ProcessKprobe *TetragonProcessKprobe `json:"process_kprobe,omitempty"`
	// NodeName is the node the Tetragon agent runs on (top level of each export line).
	NodeName string `json:"node_name,omitempty"`
}

// TetragonProcessExec carries a process execve.
type TetragonProcessExec struct {
	Process *TetragonProcess `json:"process,omitempty"`
}

// TetragonProcessKprobe carries a kprobe hit — the workhorse event kind for
// everything other than exec. FunctionName identifies the hooked kernel
// function; Args carry the typed argument objects, used for sensitive file
// writes (file_arg/path_arg/string_arg) and outbound connects (sock_arg).
type TetragonProcessKprobe struct {
	Process      *TetragonProcess `json:"process,omitempty"`
	FunctionName string           `json:"function_name,omitempty"`
	Args         []TetragonArg    `json:"args,omitempty"`
	// Return is the hooked function's return value, present when the policy
	// asks for it (return: true).
	Return *TetragonArg `json:"return,omitempty"`
}

// TetragonArg is one kprobe argument: path-bearing shapes (file_arg.path,
// path_arg.path, string_arg), the socket shape (sock_arg) used for connect
// detection, and an int (int_arg): a file-permission hook's access mask, or a
// return value.
type TetragonArg struct {
	FileArg   *TetragonFileArg `json:"file_arg,omitempty"`
	PathArg   *TetragonFileArg `json:"path_arg,omitempty"`
	StringArg string           `json:"string_arg,omitempty"`
	SockArg   *TetragonSockArg `json:"sock_arg,omitempty"`
	IntArg    *int32           `json:"int_arg,omitempty"`
}

// TetragonFileArg carries a path-bearing argument.
type TetragonFileArg struct {
	Path string `json:"path,omitempty"`
}

// TetragonSockArg is Tetragon's KprobeSock, emitted by a connect kprobe;
// only the destination tuple (daddr/dport) is read.
type TetragonSockArg struct {
	DAddr string `json:"daddr,omitempty"`
	DPort int    `json:"dport,omitempty"`
}

// TetragonProcess is the common process descriptor across event kinds:
// binary, arguments, and container/cgroup correlation handles.
type TetragonProcess struct {
	Binary    string `json:"binary,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	CgroupID  uint64 `json:"cgroup_id,string,omitempty"`
	Docker    string `json:"docker,omitempty"` // container id (truncated)
	// Pod is set on Kubernetes nodes: the pod and container the process runs in.
	Pod *TetragonPod `json:"pod,omitempty"`
}

// TetragonPod is the Kubernetes pod block of a process descriptor. Only the
// immutable identity is read; pod labels are mutable and never a key.
type TetragonPod struct {
	Namespace string                `json:"namespace,omitempty"`
	Name      string                `json:"name,omitempty"`
	Container *TetragonPodContainer `json:"container,omitempty"`
}

// TetragonPodContainer names the container inside the pod.
type TetragonPodContainer struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// ── Mapper ──────────────────────────────────────────────────────────────────

// Mapper converts a Tetragon JSON event into a Wardyn AuditEvent. Stateless
// apart from the injected Correlator; clock-free (the sidecar stamps time).
// Safe for concurrent use when the Correlator is.
type Mapper struct {
	corr Correlator
	// node stamps events whose export line carries no node_name.
	node string
}

// NewMapper builds a Mapper over a Correlator. A nil Correlator treats every
// event as unmapped — never a panic, so a mis-wired sidecar degrades to
// visible blindness rather than crashing.
func NewMapper(corr Correlator) *Mapper {
	return &Mapper{corr: corr}
}

// WithNode sets the node recorded on events whose export line has no
// node_name, and returns m. Call it before the Mapper is shared.
func (m *Mapper) WithNode(node string) *Mapper {
	m.node = node
	return m
}

// Map converts ev into an AuditEvent. ok is false for an unrecorded event
// kind, a probe from a hook it does not map, or a non-sensitive file write
// (filtered noise). When ok is true, the event has Action with the kernel.
// prefix and Data with stream="ebpf".
//
// Events that can't be bound to a run are still returned (ok=true) with
// RunID nil and correlation="unmapped" — blindness must be visible, never a
// silent drop.
func (m *Mapper) Map(ev TetragonEvent) (types.AuditEvent, bool) {
	switch {
	case ev.ProcessExec != nil:
		return m.mapExec(ev.ProcessExec, m.nodeOf(ev))
	case ev.ProcessKprobe != nil:
		return m.mapKprobe(ev.ProcessKprobe, m.nodeOf(ev))
	default:
		return types.AuditEvent{}, false
	}
}

// MapLine parses one JSON line of the Tetragon export and maps it. ok is false
// on a parse error or an unrecorded/filtered event.
func (m *Mapper) MapLine(line []byte) (types.AuditEvent, bool) {
	var ev TetragonEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return types.AuditEvent{}, false
	}
	return m.Map(ev)
}

// nodeOf is the node an event is stamped with: the Mapper's own node when it is
// set, else the line's node_name. The sensor's heartbeat is keyed by the same
// node, so events and beats join on one value even when Tetragon's node_name is
// its hostname rather than the Kubernetes node name.
func (m *Mapper) nodeOf(ev TetragonEvent) string {
	if m.node != "" {
		return m.node
	}
	return ev.NodeName
}

func (m *Mapper) mapExec(e *TetragonProcessExec, node string) (types.AuditEvent, bool) {
	p := e.Process
	if p == nil {
		return types.AuditEvent{}, false
	}
	runID, correlation := m.correlate(p)
	// argv carries the binary only. The raw arguments travel whole in
	// Arguments: splitting them here would cut a secret containing a space
	// into words the control plane's masking no longer matches, so it masks
	// them first and splits them after (MaskData).
	var argv []string
	if p.Binary != "" {
		argv = []string{p.Binary}
	}
	loader := IsDynamicLinker(p.Binary)
	// ld-linux hides the real program in argv[1]; with no binary field argv[0]
	// is the first argument, and a loader there (invoked by basename) is
	// flagged too.
	if p.Binary == "" {
		if args := strings.Fields(p.Arguments); len(args) > 0 {
			loader = IsDynamicLinker(args[0])
		}
	}
	data := EventData{
		Stream:      Stream,
		Subtype:     SubtypeProcessExec,
		CgroupID:    p.CgroupID,
		ContainerID: containerID(p),
		Node:        node,
		Pod:         podOf(p),
		Argv:        argv,
		Arguments:   p.Arguments,
		Loader:      loader,
		Correlation: correlation,
	}
	return auditFor(runID, ActionProcessExec, p.Binary, "success", data), true
}

// mapKprobe maps a process_kprobe from a hook whose meaning it knows: a
// connect hook to a connect, the file-permission hook to a file write. A probe
// from any other hook is dropped, never guessed at: an operator's own policy
// may hook reads, opens or receives, and a socket or a path alone does not say
// which.
func (m *Mapper) mapKprobe(e *TetragonProcessKprobe, node string) (types.AuditEvent, bool) {
	p := e.Process
	if p == nil {
		return types.AuditEvent{}, false
	}
	switch {
	case isConnectKprobe(e.FunctionName):
		return m.mapConnect(p, kprobeSock(e), node)
	case strings.TrimSpace(e.FunctionName) == filePermissionKprobe:
		return m.mapFileWrite(p, e, node)
	}
	return types.AuditEvent{}, false
}

// mapConnect emits a kernel.network.connect from a connect kprobe's sock_arg.
func (m *Mapper) mapConnect(p *TetragonProcess, sock *TetragonSockArg, node string) (types.AuditEvent, bool) {
	runID, correlation := m.correlate(p)
	ip, port := connectDst(sock)
	dst := ""
	if ip != "" {
		dst = net.JoinHostPort(ip, strconv.Itoa(port))
	}
	// This target-agnostic mapper can't know the run's proxy address, so it
	// does NOT infer escape-ness from destination IP class (inverted under
	// the primary L0 topology, where the sole legitimate dst is a private
	// bridge IP). ACCEPTED CEILING: a private-IP lateral connect is unflagged
	// until a proxy-address-aware comparer exists. Exception: cloud metadata
	// IP is always flagged as a credential-theft blind spot.
	outcome := "success"
	if isMetadataIP(ip) {
		outcome = "failure"
	}
	data := EventData{
		Stream:      Stream,
		Subtype:     SubtypeNetworkConnect,
		CgroupID:    p.CgroupID,
		ContainerID: containerID(p),
		Node:        node,
		Pod:         podOf(p),
		Dst:         dst,
		Correlation: correlation,
	}
	return auditFor(runID, ActionNetworkConnect, dst, outcome, data), true
}

// mapFileWrite emits a kernel.file.write for a write the kernel let through to
// a sensitive path. A read, a refused write, or a probe without the access mask
// or the return value is dropped: a pathname alone is no evidence of a write.
func (m *Mapper) mapFileWrite(p *TetragonProcess, e *TetragonProcessKprobe, node string) (types.AuditEvent, bool) {
	if !permittedWrite(e) {
		return types.AuditEvent{}, false
	}
	path := kprobePath(e)
	if !IsSensitivePath(path) {
		// Non-sensitive write: filtered noise, not recorded.
		return types.AuditEvent{}, false
	}
	runID, correlation := m.correlate(p)
	data := EventData{
		Stream:      Stream,
		Subtype:     SubtypeFileWrite,
		CgroupID:    p.CgroupID,
		ContainerID: containerID(p),
		Node:        node,
		Pod:         podOf(p),
		Path:        path,
		Correlation: correlation,
	}
	return auditFor(runID, ActionFileWrite, path, "success", data), true
}

// connectKprobes are the kernel functions a connect TracingPolicy hooks; a
// kprobe naming one is a connect even if sock_arg is (unexpectedly) absent.
var connectKprobes = map[string]bool{
	"tcp_connect":             true,
	"security_socket_connect": true,
	"__sys_connect":           true,
	"__x64_sys_connect":       true,
	"sys_connect":             true,
}

// isConnectKprobe reports whether fn is a known connect-hook kernel function.
func isConnectKprobe(fn string) bool {
	return connectKprobes[strings.TrimSpace(fn)]
}

// filePermissionKprobe is the file-write hook, security_file_permission(file,
// mask), as the shipped policy hooks it
// (deploy/compose/tetragon-policies/wardyn-groundtruth.yaml): the file, the
// access mask, and the return value.
const filePermissionKprobe = "security_file_permission"

// mayWrite is the kernel's MAY_WRITE bit in an access mask.
const mayWrite = 2

// permittedWrite reports whether a file-permission probe is a write the kernel
// allowed: its access mask (the first int argument) has MAY_WRITE, and the hook
// returned 0.
func permittedWrite(e *TetragonProcessKprobe) bool {
	var mask *int32
	for i := range e.Args {
		if e.Args[i].IntArg != nil {
			mask = e.Args[i].IntArg
			break
		}
	}
	return mask != nil && *mask&mayWrite != 0 && e.Return != nil && e.Return.IntArg != nil && *e.Return.IntArg == 0
}

// kprobeSock returns the first sock_arg in a kprobe event, or nil.
func kprobeSock(e *TetragonProcessKprobe) *TetragonSockArg {
	for i := range e.Args {
		if e.Args[i].SockArg != nil {
			return e.Args[i].SockArg
		}
	}
	return nil
}

// correlate resolves a process to a run id: by pod when the event carries a
// pod block and the Correlator is a PodCorrelator, then by container id.
// Returns (nil, unmapped) if it doesn't resolve or no Correlator is wired.
func (m *Mapper) correlate(p *TetragonProcess) (*uuid.UUID, Correlation) {
	if m.corr == nil {
		return nil, CorrelationUnmapped
	}
	if pc, ok := m.corr.(PodCorrelator); ok && p != nil && p.Pod != nil {
		container := ""
		if p.Pod.Container != nil {
			container = p.Pod.Container.Name
		}
		if id, ok := pc.RunForPod(p.Pod.Namespace, p.Pod.Name, container); ok {
			rid := id
			return &rid, CorrelationMapped
		}
	}
	if cid := containerID(p); cid != "" {
		if id, ok := m.corr.RunForContainer(cid); ok {
			rid := id
			return &rid, CorrelationMapped
		}
	}
	return nil, CorrelationUnmapped
}

// auditFor builds a kernel.* AuditEvent. ID/Time are left zero for the
// recorder to stamp. ActorType/Actor are FIXED to the system sensor —
// attribution can never be spoofed by event content.
func auditFor(runID *uuid.UUID, action, target, outcome string, data EventData) types.AuditEvent {
	return types.AuditEvent{
		RunID:     runID,
		ActorType: types.ActorSystem,
		Actor:     SensorActor,
		Action:    action,
		Target:    target,
		Outcome:   outcome,
		Data:      data.marshal(),
	}
}

// podOf is the "namespace/name" of the event's pod, or "" off Kubernetes.
func podOf(p *TetragonProcess) string {
	if p == nil || p.Pod == nil || p.Pod.Name == "" {
		return ""
	}
	return p.Pod.Namespace + "/" + p.Pod.Name
}

func containerID(p *TetragonProcess) string {
	if p == nil {
		return ""
	}
	return p.Docker
}

// connectDst extracts ip+port from a connect kprobe's sock_arg. A nil sock
// yields an empty destination — still recorded, not dropped.
func connectDst(sock *TetragonSockArg) (string, int) {
	if sock == nil {
		return "", 0
	}
	return sock.DAddr, sock.DPort
}

// kprobePath pulls the first path-bearing argument from a kprobe event.
func kprobePath(e *TetragonProcessKprobe) string {
	for _, a := range e.Args {
		switch {
		case a.FileArg != nil && a.FileArg.Path != "":
			return a.FileArg.Path
		case a.PathArg != nil && a.PathArg.Path != "":
			return a.PathArg.Path
		case a.StringArg != "":
			return a.StringArg
		}
	}
	return ""
}

// linkLocalCredentialIPs are the link-local cloud credential endpoints: the
// instance-metadata address and the ECS task-role / EKS Pod Identity agents.
var linkLocalCredentialIPs = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"),
	netip.MustParseAddr("169.254.170.2"),
	netip.MustParseAddr("169.254.170.23"),
}

// isMetadataIP reports whether ipStr is a cloud instance-metadata or credential
// endpoint (the link-local hosts above, or one of ipguard.NeverLift's), flagged
// regardless of topology (see mapConnect).
func isMetadataIP(ipStr string) bool {
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return slices.Contains(linkLocalCredentialIPs, addr) ||
		slices.ContainsFunc(ipguard.NeverLift, func(p netip.Prefix) bool { return p.Contains(addr) })
}
