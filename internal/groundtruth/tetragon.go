// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package groundtruth

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"

	"github.com/google/uuid"

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
// kprobe handler multiplexes file-writes and network connects by function
// name + argument shape. Other kinds are ignored (ok=false).
type TetragonEvent struct {
	ProcessExec   *TetragonProcessExec   `json:"process_exec,omitempty"`
	ProcessKprobe *TetragonProcessKprobe `json:"process_kprobe,omitempty"`
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
}

// TetragonArg is one kprobe argument: path-bearing shapes (file_arg.path,
// path_arg.path, string_arg) and the socket shape (sock_arg) used for
// connect detection.
type TetragonArg struct {
	FileArg   *TetragonFileArg `json:"file_arg,omitempty"`
	PathArg   *TetragonFileArg `json:"path_arg,omitempty"`
	StringArg string           `json:"string_arg,omitempty"`
	SockArg   *TetragonSockArg `json:"sock_arg,omitempty"`
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
}

// ── Mapper ──────────────────────────────────────────────────────────────────

// Mapper converts a Tetragon JSON event into a Wardyn AuditEvent. Stateless
// apart from the injected Correlator; clock-free (the sidecar stamps time).
// Safe for concurrent use when the Correlator is.
type Mapper struct {
	corr Correlator
}

// NewMapper builds a Mapper over a Correlator. A nil Correlator treats every
// event as unmapped — never a panic, so a mis-wired sidecar degrades to
// visible blindness rather than crashing.
func NewMapper(corr Correlator) *Mapper {
	return &Mapper{corr: corr}
}

// Map converts ev into an AuditEvent. ok is false for an unrecorded event
// kind or a non-sensitive file write (filtered noise). When ok is true, the
// event has Action with the kernel. prefix and Data with stream="ebpf".
//
// Events that can't be bound to a run are still returned (ok=true) with
// RunID nil and correlation="unmapped" — blindness must be visible, never a
// silent drop.
func (m *Mapper) Map(ev TetragonEvent) (types.AuditEvent, bool) {
	switch {
	case ev.ProcessExec != nil:
		return m.mapExec(ev.ProcessExec)
	case ev.ProcessKprobe != nil:
		return m.mapKprobe(ev.ProcessKprobe)
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

func (m *Mapper) mapExec(e *TetragonProcessExec) (types.AuditEvent, bool) {
	p := e.Process
	if p == nil {
		return types.AuditEvent{}, false
	}
	runID, correlation := m.correlate(p)
	argv := buildArgv(p)
	loader := IsDynamicLinker(p.Binary)
	// ld-linux hides the real program in argv[1]; if argv[0] is itself a
	// loader (invoked by basename), flag that too.
	if !loader && len(argv) > 0 {
		loader = IsDynamicLinker(argv[0])
	}
	data := EventData{
		Stream:      Stream,
		Subtype:     SubtypeProcessExec,
		CgroupID:    p.CgroupID,
		ContainerID: containerID(p),
		Argv:        argv,
		Loader:      loader,
		Correlation: correlation,
	}
	return auditFor(runID, ActionProcessExec, p.Binary, "success", data), true
}

// mapKprobe multiplexes a process_kprobe into a connect or file-write audit
// event. A connect kprobe is recognised by function name or by carrying a
// sock_arg; everything else is a file write.
func (m *Mapper) mapKprobe(e *TetragonProcessKprobe) (types.AuditEvent, bool) {
	p := e.Process
	if p == nil {
		return types.AuditEvent{}, false
	}
	if sock := kprobeSock(e); sock != nil || isConnectKprobe(e.FunctionName) {
		return m.mapConnect(p, sock)
	}
	return m.mapFileWrite(p, e)
}

// mapConnect emits a kernel.network.connect from a connect kprobe's sock_arg.
func (m *Mapper) mapConnect(p *TetragonProcess, sock *TetragonSockArg) (types.AuditEvent, bool) {
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
		Dst:         dst,
		Correlation: correlation,
	}
	return auditFor(runID, ActionNetworkConnect, dst, outcome, data), true
}

// mapFileWrite emits a kernel.file.write for a sensitive-path kprobe.
func (m *Mapper) mapFileWrite(p *TetragonProcess, e *TetragonProcessKprobe) (types.AuditEvent, bool) {
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

// kprobeSock returns the first sock_arg in a kprobe event, or nil.
func kprobeSock(e *TetragonProcessKprobe) *TetragonSockArg {
	for i := range e.Args {
		if e.Args[i].SockArg != nil {
			return e.Args[i].SockArg
		}
	}
	return nil
}

// correlate resolves a process to a run id via container-id correlation.
// Returns (nil, unmapped) if it doesn't resolve or no Correlator is wired.
func (m *Mapper) correlate(p *TetragonProcess) (*uuid.UUID, Correlation) {
	if m.corr == nil {
		return nil, CorrelationUnmapped
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

func containerID(p *TetragonProcess) string {
	if p == nil {
		return ""
	}
	return p.Docker
}

// buildArgv splits binary + arguments into an argv slice. Tetragon emits
// arguments as a single space-separated string; a simple whitespace split is
// fine since exact tokenisation of quoted args isn't load-bearing (the raw
// binary is the authoritative field).
func buildArgv(p *TetragonProcess) []string {
	if p == nil {
		return nil
	}
	argv := []string{}
	if p.Binary != "" {
		argv = append(argv, p.Binary)
	}
	if args := strings.Fields(p.Arguments); len(args) > 0 {
		argv = append(argv, args...)
	}
	if len(argv) == 0 {
		return nil
	}
	return argv
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

// isMetadataIP reports whether ipStr is the cloud instance-metadata address
// (169.254.169.254), flagged regardless of topology (see mapConnect).
func isMetadataIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	return ip != nil && ip.Equal(net.ParseIP("169.254.169.254"))
}
