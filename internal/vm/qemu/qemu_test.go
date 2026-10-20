// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testConfig() Config {
	return Config{RunID: uuid.MustParse("6f0c0d8e-4b1a-4c9e-9a51-0d1f2e3a4b5c"), MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", VCPUs: 2, MemoryMiB: 2048}
}

// argAfter is the value following flag's first occurrence.
func argAfter(t *testing.T, a []string, flag string) string {
	t.Helper()
	i := slices.Index(a, flag)
	if i < 0 || i+1 >= len(a) {
		t.Fatalf("%s missing from %q", flag, a)
	}
	return a[i+1]
}

func TestArgsPinTheMachineAndListenOnlyOnUnixSockets(t *testing.T) {
	a, err := Args(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := argAfter(t, a, "-machine"); got != "pc-q35-10.0,accel=kvm" {
		t.Fatalf("-machine %q: the versioned machine, KVM only (no TCG fallback)", got)
	}
	if got := argAfter(t, a, "-cpu"); got != "Haswell-v4,enforce" {
		t.Fatalf("-cpu %q: the named model with enforce", got)
	}
	if got := argAfter(t, a, "-append"); !strings.Contains(got, "noresume") || !strings.Contains(got, "wardyn.run=6f0c0d8e-4b1a-4c9e-9a51-0d1f2e3a4b5c") {
		t.Fatalf("-append %q: noresume (S1: 31 s resume probe) and the run id", got)
	}
	if got := argAfter(t, a, "-vnc"); got != "unix:"+CtlDir+"/"+VNCSocket {
		t.Fatalf("-vnc %q: VNC has no auth, so it lives only in ctl/", got)
	}
	if got := argAfter(t, a, "-qmp"); !strings.HasPrefix(got, "unix:"+CtlDir+"/") {
		t.Fatalf("-qmp %q is not under ctl/", got)
	}
	if got := argAfter(t, a, "-netdev"); got != "stream,id=n0,server=off,reconnect-ms=1000,addr.type=unix,addr.path="+NetDir+"/"+NetSocket {
		t.Fatalf("-netdev %q: the VM-B contract is a stream client to net/net.sock", got)
	}
	if got := argAfter(t, a, "-sandbox"); !strings.Contains(got, "spawn=deny") || !strings.HasPrefix(got, "on,") {
		t.Fatalf("-sandbox %q", got)
	}
	joined := strings.Join(a, " ")
	for _, banned := range []string{"tcp:", "-netdev user", "user,", "hostfwd", "telnet", "websocket", ":5900", "-enable-kvm,", "tcg"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("argv carries %q: %s", banned, joined)
		}
	}
	for i, arg := range a {
		if arg == "-chardev" && !strings.Contains(a[i+1], "path="+CtlDir+"/") && !strings.Contains(a[i+1], "path="+FSDir+"/") {
			t.Fatalf("chardev %q outside ctl/ and fs/", a[i+1])
		}
	}
	if slices.Contains(a, "name="+FWCfgCA+",file="+FWCfgDir+"/"+CAFile) {
		t.Fatal("a run with no CA got a CA fw_cfg entry")
	}
	if got := argAfter(t, a, "-fw_cfg"); got != "name="+FWCfgRunID+",string=6f0c0d8e-4b1a-4c9e-9a51-0d1f2e3a4b5c" {
		t.Fatalf("fw_cfg %q: the run id, and nothing else without a CA", got)
	}
}

func TestArgsCAAndShares(t *testing.T) {
	c := testConfig()
	c.CA, c.Shares = true, 2
	a, err := Args(c)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(a, " ")
	for _, want := range []string{
		"-fw_cfg name=" + FWCfgCA + ",file=" + FWCfgDir + "/" + CAFile,
		"-chardev socket,id=fs0,path=" + FSDir + "/0.sock -device vhost-user-fs-pci,chardev=fs0,tag=wardyn0",
		"-chardev socket,id=fs1,path=" + FSDir + "/1.sock -device vhost-user-fs-pci,chardev=fs1,tag=wardyn1",
		"memory-backend-memfd,id=mem,size=2048M,share=on",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv lacks %q: %s", want, joined)
		}
	}
}

func TestConfigRefusesWhatCannotBeRecorded(t *testing.T) {
	for name, mut := range map[string]func(*Config){
		"no run":          func(c *Config) { c.RunID = uuid.Nil },
		"unversioned q35": func(c *Config) { c.MachineType = "q35" },
		"cpu with flags":  func(c *Config) { c.CPUModel = "Haswell-v4,+vmx" },
		"host cpu":        func(c *Config) { c.CPUModel = "" },
		"tiny":            func(c *Config) { c.MemoryMiB = 64 },
	} {
		c := testConfig()
		mut(&c)
		if _, err := Args(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSizing(t *testing.T) {
	if VCPUs(0) != 2 || VCPUs(1) != 1 || VCPUs(1000) != 1 || VCPUs(1500) != 2 {
		t.Fatal("vCPUs round up per started thousand millicores, two by default")
	}
	for _, tc := range []struct{ cap, guest, container int64 }{{0, 2048, 2560}, {4096, 3277, 4096}, {1024, 512, 1024}} {
		g, c, err := Memory(tc.cap)
		if err != nil || g != tc.guest || c != tc.container {
			t.Errorf("Memory(%d) = %d, %d, %v; want %d, %d", tc.cap, g, c, err, tc.guest, tc.container)
		}
	}
	if _, _, err := Memory(700); err == nil {
		t.Fatal("a cap too small for the guest was accepted")
	}
}

func TestVirtiofsdReadOnly(t *testing.T) {
	if !slices.Contains(VirtiofsdArgs(0, true), "--readonly") || slices.Contains(VirtiofsdArgs(1, false), "--readonly") {
		t.Fatal("a read-only mount must be served read-only")
	}
}

// fakeQEMU answers QMP on one end of a pipe from a table of canned returns.
func fakeQEMU(t *testing.T, conn net.Conn, returns map[string]string) {
	t.Helper()
	go func() {
		defer conn.Close()
		_, _ = conn.Write([]byte(`{"QMP": {"version": {"qemu": {"micro": 13, "minor": 0, "major": 10}}, "capabilities": ["oob"]}}` + "\n"))
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var req struct {
				Execute string `json:"execute"`
			}
			_ = json.Unmarshal(sc.Bytes(), &req)
			ret, ok := returns[req.Execute]
			if !ok {
				ret = `{}`
			}
			// An async event first: the client must skip it.
			_, _ = conn.Write([]byte(`{"event": "RTC_CHANGE", "data": {}}` + "\n"))
			if strings.HasPrefix(ret, "ERR ") {
				_, _ = conn.Write([]byte(`{"error": {"class": "GenericError", "desc": "` + ret[4:] + `"}}` + "\n"))
				continue
			}
			_, _ = conn.Write([]byte(`{"return": ` + ret + "}\n"))
		}
	}()
}

func TestQMPProbe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	fakeQEMU(t, b, map[string]string{
		"query-kvm":      `{"enabled": true, "present": true}`,
		"query-version":  `{"qemu": {"major": 10, "minor": 0, "micro": 13}, "package": "Debian"}`,
		"query-machines": `[{"name": "pc-i440fx-10.0", "alias": "pc"}, {"name": "pc-q35-10.0", "alias": "q35"}, {"name": "pc-q35-9.2"}]`,
	})
	q, err := NewQMP(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	p, err := q.Probe(ctx)
	if err != nil || p.MachineType != "pc-q35-10.0" || p.QEMUVersion != "10.0.13" {
		t.Fatalf("probe = %+v, %v", p, err)
	}
}

func TestQMPProbeRefusesWithoutKVM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	fakeQEMU(t, b, map[string]string{"query-kvm": `{"enabled": false, "present": true}`})
	q, err := NewQMP(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if _, err := q.Probe(ctx); err == nil || !strings.Contains(err.Error(), "KVM is not in use") {
		t.Fatalf("probe without KVM = %v: a TCG VM must never be offered", err)
	}
	if err := q.Execute(ctx, "x", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestQGASyncSkipsStaleAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		sc := bufio.NewScanner(b)
		sc.Scan()
		var req struct {
			Arguments struct {
				ID int64 `json:"id"`
			} `json:"arguments"`
		}
		_ = json.Unmarshal(sc.Bytes(), &req)
		// A stale answer from an earlier session, then ours.
		_, _ = b.Write([]byte(`{"return": 1}` + "\n"))
		out, _ := json.Marshal(map[string]int64{"return": req.Arguments.ID})
		_, _ = b.Write(append(out, '\n'))
		sc.Scan()
		if !strings.Contains(sc.Text(), `"guest-set-time"`) || !strings.Contains(sc.Text(), `"time":`) {
			_, _ = b.Write([]byte(`{"error": {"class": "x", "desc": "unexpected"}}` + "\n"))
			return
		}
		_, _ = b.Write([]byte(`{"return": {}}` + "\n"))
	}()
	g, err := NewQGA(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.SetTime(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeReplyCapsAndQuotesGuestText(t *testing.T) {
	var rep reply
	if err := json.Unmarshal([]byte(`{"error": {"class": "GenericError", "desc": "`+strings.Repeat(`\u001b[31m\n`, 8192)+`"}}`), &rep); err != nil {
		t.Fatal(err)
	}
	msg := decodeReply("QGA x", rep, nil).Error()
	if len(msg) > 4*maxGuestText+100 || strings.ContainsAny(msg, "\x1b\n") || !strings.Contains(msg, `\x1b[31m\n`) {
		t.Fatalf("error text is not capped and quoted (%d bytes): %.80q", len(msg), msg)
	}
}
