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
// We deliberately define only the fields we read, against Tetragon's JSON
// export shape (the top-level GetEventsResponse, one JSON object per line). This
// avoids pulling github.com/cilium/tetragon (and its gRPC/protobuf graph) into
// go.mod — the dependency choice documented in the package doc. The field names
// and nesting below match Tetragon's protojson output for process_exec and
// process_kprobe events.
//
// FINDING (medium, fixed): an earlier version mapped network connects off a
// fictional top-level "process_connect" event kind. Tetragon has NO such kind:
// the GetEventsResponse oneof is process_exec / process_exit / process_kprobe /
// process_tracepoint / process_uprobe / process_lsm / process_loader / ...
// (https://tetragon.io/docs/reference/grpc-api/). A TCP connect is observed via
// a process_kprobe on a connect kprobe (tcp_connect / security_socket_connect /
// __sys_connect) whose socket argument is a sock_arg (KprobeSock:
// family/protocol/saddr/daddr/sport/dport). Because the old code keyed on a kind
// the kernel never emits, live escape/connect detection NEVER fired. Connects
// are now routed through the kprobe handler against the real shape.

// TetragonEvent is one line of the Tetragon JSON export. Exactly one of the
// event-kind fields is set per line. We map process_exec and process_kprobe;
// the kprobe handler multiplexes file-writes (security_file_permission /
// fd_install) and network connects (tcp_connect / security_socket_connect /
// __sys_connect) by function name + argument shape. Other kinds are ignored by
// the mapper (returns ok=false).
type TetragonEvent struct {
	ProcessExec   *TetragonProcessExec   `json:"process_exec,omitempty"`
	ProcessKprobe *TetragonProcessKprobe `json:"process_kprobe,omitempty"`
}

// TetragonProcessExec carries a process execve.
type TetragonProcessExec struct {
	Process *TetragonProcess `json:"process,omitempty"`
}

// TetragonProcessKprobe carries a kprobe hit — the workhorse event kind for
// everything other than exec. The FunctionName identifies the hooked kernel
// function; the Args carry the typed argument objects. We use it for two things:
//   - sensitive file writes (security_file_permission / __x64_sys_write-style
//     TracingPolicy): the path is in a file_arg / path_arg / string_arg.
//   - outbound network connects (tcp_connect / security_socket_connect /
//     __sys_connect TracingPolicy): the destination is in a sock_arg.
type TetragonProcessKprobe struct {
	Process      *TetragonProcess `json:"process,omitempty"`
	FunctionName string           `json:"function_name,omitempty"`
	Args         []TetragonArg    `json:"args,omitempty"`
}

// TetragonArg is one kprobe argument. Tetragon emits typed argument objects; we
// read the path-bearing shapes (file_arg.path, path_arg.path, string_arg) and
// the socket shape (sock_arg, a KprobeSock) used for connect detection.
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

// TetragonSockArg is Tetragon's KprobeSock argument, emitted by a connect
// kprobe. Field names match Tetragon's protojson output. We read only the
// destination tuple (daddr/dport) used for connect detection.
type TetragonSockArg struct {
	DAddr string `json:"daddr,omitempty"` // destination address
	DPort int    `json:"dport,omitempty"` // destination port
}

// TetragonProcess is the common process descriptor across event kinds. We read
// the binary, arguments, and container/cgroup correlation handles.
type TetragonProcess struct {
	Binary    string `json:"binary,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	CgroupID  uint64 `json:"cgroup_id,string,omitempty"`
	Docker    string `json:"docker,omitempty"` // container id (truncated)
}

// ── Mapper ──────────────────────────────────────────────────────────────────

// Mapper converts a Tetragon JSON event into a Wardyn AuditEvent. It is
// stateless apart from the injected Correlator and a clock-free design (the
// ingest sidecar / control plane stamp the time). It is safe for concurrent use
// when the Correlator is.
type Mapper struct {
	corr Correlator
}

// NewMapper builds a Mapper over a Correlator. A nil Correlator treats every
// event as unmapped (run_id NULL, correlation="unmapped") — never a panic, so a
// mis-wired sidecar degrades to visible-blindness rather than crashing.
func NewMapper(corr Correlator) *Mapper {
	return &Mapper{corr: corr}
}

// Map converts ev into an AuditEvent. ok is false when the event kind is one we
// do not record (so the caller skips it) or when a file_write does not touch a
// sensitive path (filtered noise). When ok is true the returned event always
// has Action with the kernel. prefix and Data with stream="ebpf".
//
// Correlation: events that cannot be bound to a run are STILL returned (ok=true)
// with RunID nil and correlation="unmapped" — blindness must be visible, never a
// silent drop. The ONLY ok=false outcomes are: unknown event kind, and a
// non-sensitive file write.
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
	// An ld-linux invocation hides the real program in argv[1]; if argv[0] is a
	// loader, also flag it (the loader can be invoked by basename).
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

// mapKprobe multiplexes a process_kprobe into either a network-connect or a
// file-write audit event. A connect kprobe is recognised by its function name
// (tcp_connect / security_socket_connect / __sys_connect) OR by carrying a
// sock_arg; everything else is treated as a (sensitive) file write. This is the
// real Tetragon shape — there is no separate "process_connect" event kind.
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
	// Outcome: this target-agnostic mapper CANNOT know the run's proxy address,
	// so it does NOT infer escape-ness from the destination's IP class. Under the
	// primary L0 topology (Internal + gatewayless) the SOLE reachable dst is
	// wardyn-proxy on a PRIVATE bridge IP — so an IP-class guess would stamp every
	// legitimate agent->proxy connect "failure" (alert fatigue) while a direct
	// public-C2 connect (the real escape) has a public dst and gets "success":
	// inverted for the shipped topology. Default to "success" (the connect
	// happened); the raw dst is preserved in the event so a proxy-address-aware
	// comparer CAN flag escapes later (not built yet — future work). ACCEPTED
	// CEILING until then: a private-IP lateral connect (e.g. 10.0.0.5:22) that the
	// old heuristic stamped "failure" is now "success" and unflagged. The ONE
	// exception kept: a reach to the cloud metadata IP is a credential-theft blind
	// spot worth flagging regardless of topology.
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

// connectKprobes are the kernel functions a connect TracingPolicy hooks. A
// kprobe naming one of these is a network connect even if the sock_arg is
// (unexpectedly) absent.
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

// correlate resolves a process to a run id via authoritative container-id
// correlation. Returns (nil, unmapped) when it does not resolve OR when no
// Correlator is wired.
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

// auditFor builds a kernel.* AuditEvent. ID/Time are left zero for the recorder
// to stamp (mirrors recordAudit's defaulting). ActorType/Actor are FIXED to the
// system sensor — attribution can never be spoofed by event content.
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

// containerID returns the process's docker container id.
func containerID(p *TetragonProcess) string {
	if p == nil {
		return ""
	}
	return p.Docker
}

// buildArgv splits a process binary + arguments string into an argv slice.
// Tetragon emits arguments as a single space-separated string; we keep it
// simple (split on whitespace) — exact tokenisation of quoted args is not
// load-bearing for the audit record (the raw binary is the authoritative field).
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

// connectDst extracts the destination ip+port from a connect kprobe's sock_arg
// (KprobeSock.daddr / KprobeSock.dport). A nil sock (a connect kprobe that fired
// without a sock argument) yields an empty destination — the event is still
// recorded (visible) with an empty dst rather than dropped.
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
// (169.254.169.254). A kernel-observed connect there is a credential-theft
// blind spot worth a "failure" flag regardless of topology. This mapper
// deliberately does NOT flag other IP classes (private/loopback/link-local):
// under the primary L0 topology the agent's only legal destination is the proxy
// on a PRIVATE bridge IP, so an IP-class guess is inverted (see mapConnect).
// Empty/unparseable IPs are not flagged.
func isMetadataIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	return ip != nil && ip.Equal(net.ParseIP("169.254.169.254"))
}
