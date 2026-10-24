// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && vm

// Package vm is the Linux VM sandbox conformance suite: the real Docker driver boots the reference
// guest image under QEMU/KVM in its unprivileged container, behind its network relay and the real
// wardyn-proxy. It builds nothing: set WARDYN_TEST_VM_QEMU_IMAGE (deploy/runner/vm-qemu),
// WARDYN_TEST_VM_GUEST_IMAGE (deploy/images/vm-linux) and WARDYN_TEST_VM_PROXY_IMAGE
// (deploy/compose/Dockerfile.proxy, which carries the relay), on a host with /dev/kvm and a local
// Docker engine. The one allowed host is an origin on the proxy's network; nothing else is reachable.
package vm

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Hermetic addressing for the proxy's network: outside every range the proxy's private-address
// guard refuses (as test/vmconformance's), so the origin is reachable only as an allowed name.
const (
	upSubnet = "11.231.2.0/24"
	originIP = "11.231.2.10"
)

// decisions are the proxy's egress decision rows, from its log.
func decisions(t *testing.T, cli *dockerclient.Client, runID uuid.UUID) []map[string]any {
	t.Helper()
	logs, err := cli.ContainerLogs(context.Background(), "wardyn-proxy-"+runID.String(), dockerclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatalf("proxy logs: %v", err)
	}
	defer logs.Close()
	b, _ := io.ReadAll(logs)
	var out []map[string]any
	for _, l := range strings.Split(string(b), "\n") {
		if i := strings.IndexByte(l, '{'); i >= 0 {
			var d map[string]any
			if json.Unmarshal([]byte(l[i:]), &d) == nil && d["rule_source"] != nil {
				out = append(out, d)
			}
		}
	}
	return out
}

// ipv6Drops counts the IPv6 frames the relay reported dropping: a batched row stands for its repeat count.
func ipv6Drops(rows []map[string]any) int {
	n := 0
	for _, r := range rows {
		if cause, _ := r["cause"].(string); r["rule_source"] == "vmnet:dropped-frame" && strings.Contains(cause, "(ipv6)") {
			rep, _ := r["repeat"].(float64)
			n += max(1, int(rep))
		}
	}
	return n
}

// settledIPv6Drops is ipv6Drops once it has held still for three seconds (at most 30 seconds).
func settledIPv6Drops(t *testing.T, cli *dockerclient.Client, runID uuid.UUID) int {
	t.Helper()
	n, still := ipv6Drops(decisions(t, cli, runID)), 0
	for deadline := time.Now().Add(30 * time.Second); still < 3 && time.Now().Before(deadline); {
		time.Sleep(time.Second)
		if m := ipv6Drops(decisions(t, cli, runID)); m == n {
			still++
		} else {
			n, still = m, 0
		}
	}
	return n
}

// linkLocalReady exits 0 once the guest's link-local addresses have passed duplicate address
// detection: a tentative address (/proc/net/if_inet6 flag 0x40) is no source, so a connection
// from it fails with EADDRNOTAVAIL and puts no frame on the link.
const linkLocalReady = `seen=0; while read a i p s f n; do [ "$n" = lo ] && continue; case $a in fe80*) seen=1; [ $((0x$f & 0x40)) -eq 0 ] || exit 1;; esac; done < /proc/net/if_inet6; [ $seen = 1 ]`

func run(t *testing.T, d *docker.Driver, ref, script string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	es, err := d.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"bash", "-c", script}})
	if err != nil {
		t.Fatalf("exec %q: %v", script, err)
	}
	_ = es.Stdin.Close()
	var so, se bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = se.ReadFrom(es.Stderr); close(done) }()
	_, _ = so.ReadFrom(es.Stdout)
	<-done
	code, err := es.Wait()
	if err != nil {
		t.Fatalf("exec %q wait: %v", script, err)
	}
	return code, so.String() + se.String()
}

func TestLinuxVMSandbox(t *testing.T) {
	qemuImage, guestImage, proxyImage := os.Getenv("WARDYN_TEST_VM_QEMU_IMAGE"), os.Getenv("WARDYN_TEST_VM_GUEST_IMAGE"), os.Getenv("WARDYN_TEST_VM_PROXY_IMAGE")
	if qemuImage == "" || guestImage == "" || proxyImage == "" {
		t.Skip("WARDYN_TEST_VM_QEMU_IMAGE, WARDYN_TEST_VM_GUEST_IMAGE and WARDYN_TEST_VM_PROXY_IMAGE not set")
	}
	ctx := context.Background()
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	// The proxy's own network, internal, with the origin on it: the allowed host, and the only one.
	upNet := "wardyn-vmconf-up-" + uuid.NewString()[:8]
	if _, err := cli.NetworkCreate(ctx, upNet, dockerclient.NetworkCreateOptions{Driver: "bridge", Internal: true,
		IPAM: &network.IPAM{Config: []network.IPAMConfig{{Subnet: netip.MustParsePrefix(upSubnet)}}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = cli.NetworkRemove(context.Background(), upNet, dockerclient.NetworkRemoveOptions{}) })
	origin, err := cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name: upNet + "-origin",
		Config: &container.Config{Image: "busybox:latest", Cmd: []string{"sh", "-c",
			"mkdir -p /www && echo origin-ok > /www/index.html && exec httpd -f -p 80 -h /www"}},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(upNet)},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{upNet: {
			Aliases: []string{"origin.test"}, IPAMConfig: &network.EndpointIPAMConfig{IPv4Address: netip.MustParseAddr(originIP)}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = cli.ContainerRemove(context.Background(), origin.ID, dockerclient.ContainerRemoveOptions{Force: true})
	})
	if _, err := cli.ContainerStart(ctx, origin.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	d, err := docker.New(docker.Config{ProxyImage: proxyImage, InternalNetwork: upNet})
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("", "wvm") // short: unix socket paths are bounded
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })

	// The start-up probe: KVM in an unprivileged container, the CPU model, the machine.
	vc, err := d.ProbeVM(ctx, docker.VMConfig{StateDir: state, QEMUImage: qemuImage, CPUModel: cmp.Or(os.Getenv("WARDYN_TEST_VM_CPU_MODEL"), "Haswell-v4")})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.HasPrefix(vc.MachineType, "pc-q35-") || vc.QEMUVersion == "" || !vc.Virtiofs {
		t.Fatalf("probe found %+v", vc)
	}
	if err := d.EnableVM(&vc); err != nil {
		t.Fatal(err)
	}

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "hello"), []byte("from the host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	rw := t.TempDir() // a writable folder: the guest's agent user is not the host owner
	if err := os.Chmod(rw, 0o777); err != nil {
		t.Fatal(err)
	}
	out := &lockedBuffer{}
	spec := runner.SandboxSpec{
		RunID: uuid.New(), Image: guestImage, MachineKind: types.MachineLinuxVM, ConfinementClass: types.CC1,
		Env: map[string]string{"WARDYN_CONFORMANCE": "vm"},
		ProxyConfig: runner.ProxyConfig{RunToken: "tok", ControlPlaneURL: "http://127.0.0.1:8080", Policy: types.RunPolicySpec{
			AllowedDomains: []string{"origin.test"}, FirstUseApproval: types.FirstUseAlwaysDeny, MinConfinementClass: types.CC1}},
		Resources:  runner.Resources{CPUMillis: 2000, MemoryMiB: 2048},
		Mounts:     []runner.Mount{{Source: ws, Target: "/home/agent/work", ReadOnly: true}, {Source: rw, Target: "/home/agent/rw"}},
		ExecOutput: out,
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute) // a first image conversion included
	defer cancel()
	sb, err := d.CreateSandbox(cctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	torn := false
	t.Cleanup(func() {
		if !torn {
			_ = d.KillSandbox(context.Background(), sb.Ref)
		}
	})
	if !sb.Machine.Complete() || sb.Machine.MachineType != vc.MachineType || sb.Machine.CPUModel != vc.CPUModel || sb.EnforcedClass != types.CC3 {
		t.Fatalf("sandbox %+v machine %+v", sb, sb.Machine)
	}
	dir := filepath.Join(state, "runs", spec.RunID.String())

	// The relay: from the proxy image, non-root, no capability, read-only, on the run's network only.
	relay, err := cli.ContainerInspect(ctx, "wardyn-vmnet-"+spec.RunID.String(), dockerclient.ContainerInspectOptions{})
	if err != nil {
		t.Fatalf("the VM's network relay: %v", err)
	}
	rc, rh := relay.Container.Config, relay.Container.HostConfig
	if rc.Image != proxyImage || rc.User == "" || rc.User == "0" || strings.HasPrefix(rc.User, "0:") || !rh.ReadonlyRootfs ||
		len(rh.CapDrop) != 1 || rh.CapDrop[0] != "ALL" || len(rh.PortBindings) != 0 || len(relay.Container.NetworkSettings.Networks) != 1 ||
		relay.Container.NetworkSettings.Networks["wardyn-int-"+spec.RunID.String()] == nil {
		t.Fatalf("relay %s as %q, read-only %v, caps %v, ports %v, networks %v", rc.Image, rc.User, rh.ReadonlyRootfs, rh.CapDrop,
			rh.PortBindings, relay.Container.NetworkSettings.Networks)
	}

	// The run directory's mode and owner are the control over QMP and the unauthenticated VNC socket.
	for _, p := range []string{"", "ctl", "net", "fs", "disk", "fwcfg"} {
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("run dir part %q: %v %v", p, fi.Mode(), err)
		}
	}
	for _, s := range []string{qemu.VNCSocket, qemu.QMPSocket, qemu.QGASocket, qemu.AgentSocket} {
		if fi, err := os.Stat(filepath.Join(dir, "ctl", s)); err != nil || fi.Mode()&os.ModeSocket == 0 {
			t.Errorf("ctl/%s: %v", s, err)
		}
	}

	// The harness path: a detached exec through the guest agent, its output in the run's output.
	if _, err := d.Exec(ctx, sb.Ref, []string{"sh", "-c", "uname -r; id -u; echo $WARDYN_CONFORMANCE; cat /home/agent/work/hello"}); err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithTimeout(ctx, 2*time.Minute)
	defer wcancel()
	if code, err := d.Wait(wctx, sb.Ref); err != nil || code != 0 {
		t.Fatalf("wait = %d, %v; output %q", code, err, out.String())
	}
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(out.String(), "from the host"); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("agent output %q", out.String())
		}
	}
	if o := out.String(); !strings.Contains(o, "deb13") || !strings.Contains(o, "\n1000") || !strings.Contains(o, "vm") {
		t.Fatalf("agent output %q: the guest's kernel, the agent user, the sandbox env, the shared folder", o)
	}

	// The folders keep their modes in the guest: the writable one takes a write the host sees, the
	// read-only one refuses one. The image has the sftp-server a folder sync writes through.
	if code, o := run(t, d, sb.Ref, "echo from-the-guest > /home/agent/rw/made && ! touch /home/agent/work/nope 2>/dev/null && test -x /usr/lib/openssh/sftp-server"); code != 0 {
		t.Fatalf("folder modes in the guest = %d %q", code, o)
	}
	if b, err := os.ReadFile(filepath.Join(rw, "made")); err != nil || string(b) != "from-the-guest\n" {
		t.Fatalf("the guest's write to the writable folder: %q %v", b, err)
	}
	// virtiofsd maps the agent user onto the runner's: the write lands as the folder's own owner.
	for _, p := range []string{rw, filepath.Join(rw, "made")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
			t.Fatalf("%s is not owned by the runner's %d:%d: %+v", p, os.Getuid(), os.Getgid(), fi.Sys())
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "nope")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the read-only folder took a write: %v", err)
	}

	// A terminal with a pty.
	sess, err := d.Attach(ctx, sb.Ref, runner.AttachOptions{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = sess.Write([]byte("tty; stty size; exit\n"))
	term, _ := io.ReadAll(io.LimitReader(sess, 1<<16))
	_ = sess.Close()
	if !strings.Contains(string(term), "/dev/pts/") || !strings.Contains(string(term), "30 100") {
		t.Fatalf("terminal %q", term)
	}

	// The desktop: QEMU's display, reached by the runner outside the guest, greets as an RFB server.
	desk, err := d.DialDesktop(ctx, sb.Ref)
	if err != nil {
		t.Fatal(err)
	}
	greeting := make([]byte, 12)
	_, err = io.ReadFull(desk, greeting)
	_ = desk.Close()
	if err != nil || !strings.HasPrefix(string(greeting), "RFB 003.") {
		t.Fatalf("desktop greeting %q, %v", greeting, err)
	}

	// The network: the guest reaches the allowed host through the relay and the proxy, and nothing
	// else. Every name resolves (to the relay's synthetic range); what the policy does not allow
	// fails at the proxy.
	if code, o := run(t, d, sb.Ref, "getent ahosts origin.test | grep -q '^198\\.1[89]\\.' || exit 7"); code != 0 {
		t.Fatalf("origin.test in the guest = %d %q, want a relay address", code, o)
	}
	if code, o := run(t, d, sb.Ref, `exec 3<>/dev/tcp/origin.test/80 && printf 'GET / HTTP/1.0\r\nHost: origin.test\r\n\r\n' >&3 && timeout 20 cat <&3`); code != 0 || !strings.Contains(o, "origin-ok") {
		t.Fatalf("the allowed host from the guest = %d %q", code, o)
	}
	for name, script := range map[string]string{
		"a bare host on port 22":    `timeout 15 bash -c '</dev/tcp/origin.test/22' && exit 0; exit 7`,
		"a host not allowed":        `timeout 15 bash -c '</dev/tcp/example.com/443' && exit 0; exit 7`,
		"the origin by its address": `timeout 15 bash -c '</dev/tcp/` + originIP + `/80' && exit 0; exit 7`,
		"an outside address":        `timeout 15 bash -c '</dev/tcp/1.1.1.1/80' && exit 0; exit 7`,
		// One A query for example.com to an outside resolver: no answer comes back.
		"an outside resolver": `exec 3<>/dev/udp/8.8.8.8/53 && printf '\x12\x34\x01\x00\x00\x01\x00\x00\x00\x00\x00\x00\x07example\x03com\x00\x00\x01\x00\x01' >&3 && timeout 5 head -c1 <&3 >/dev/null && exit 0; exit 7`,
	} {
		if code, o := run(t, d, sb.Ref, script); code != 7 {
			t.Errorf("%s from the guest = %d %q, want refused", name, code, o)
		}
	}
	// IPv6: a link-local connect puts frames on the link, which the relay drops and reports
	// (batched, one row a second per kind, with a repeat count); a global one has no route in the
	// guest. The guest's own IPv6 chatter is reported the same way, so the probe must add to a
	// count that has settled, and it must run from an address that has passed duplicate address
	// detection.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if code, _ := run(t, d, sb.Ref, linkLocalReady); code == 0 {
			break
		}
		if time.Now().After(deadline) {
			_, o := run(t, d, sb.Ref, "cat /proc/net/if_inet6")
			t.Fatalf("the guest's link-local address is still tentative: %q", o)
		}
	}
	before := settledIPv6Drops(t, cli, spec.RunID)
	if code, o := run(t, d, sb.Ref, `for i in $(ls /sys/class/net); do if [ "$i" != lo ] && timeout 5 bash -c "</dev/tcp/fe80::1%$i/80"; then exit 0; fi; done; timeout 10 bash -c '</dev/tcp/2001:4860:4860::8888/443' && exit 0; exit 7`); code != 7 {
		t.Errorf("IPv6 from the guest = %d %q, want refused", code, o)
	}
	for deadline := time.Now().Add(15 * time.Second); ipv6Drops(decisions(t, cli, spec.RunID)) <= before; time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Errorf("no vmnet:dropped-frame (ipv6) row after the IPv6 probe: %d dropped before it, none since", before)
			break
		}
	}
	t.Logf("the relay counted %d IPv6 frames dropped, %d of them before the IPv6 probe", ipv6Drops(decisions(t, cli, spec.RunID)), before)
	// The proxy's rows: the port rule refused port 22 (the run's proxy is marked vm, as nothing else
	// turns that rule on here), the relay reported what only it sees, and every allow is the origin.
	want := map[string]bool{"policy:port": false, "policy:default-deny": false, "vmnet:literal-ip": false, "vmnet:udp": false, "vmnet:dropped-frame": false}
	var rows []map[string]any
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(time.Second) {
		rows = decisions(t, cli, spec.RunID)
		for k := range want {
			want[k] = false
		}
		for _, r := range rows {
			if src, _ := r["rule_source"].(string); r["decision"] == "deny" {
				if _, ok := want[src]; ok {
					want[src] = true
				}
			}
		}
		if !strings.Contains(fmt.Sprint(want), "false") || time.Now().After(deadline) {
			break
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("no %s deny row in the proxy's log", k)
		}
	}
	for _, r := range rows {
		req, _ := r["request"].(map[string]any)
		host, _ := req["Host"].(string)
		src, _ := r["rule_source"].(string)
		flow, _ := r["flow"].(string)
		t.Logf("%v %s host=%q port=%v flow=%q repeat=%v cause=%q", r["decision"], src, host, req["Port"], flow, r["repeat"], r["cause"])
		switch {
		case r["decision"] == "allow" && host != "origin.test":
			t.Errorf("allow row for %q: only the origin is allowed", host)
		case r["decision"] == "deny" && !strings.HasPrefix(src, "vmnet:") && flow == "":
			t.Errorf("deny row %v: not from a relay flow", r)
		}
	}

	// The CA is not readable by the agent user (fw_cfg is root's), and the guest trusts no run CA here.
	if code, _ := run(t, d, sb.Ref, "cat /sys/firmware/qemu_fw_cfg/by_name/opt/org.wardyn/run-id/raw"); code == 0 {
		t.Fatal("the agent user read fw_cfg")
	}
	// The agent user can neither open the guest agent's port nor make its init marker. EACCES, not
	// EBUSY: EBUSY shows only that the agent holds the port.
	for name, redirect := range map[string]string{"AgentPortReadIsEACCES": "<", "AgentPortWriteIsEACCES": ">"} {
		t.Run(name, func(t *testing.T) {
			code, o := run(t, d, sb.Ref, "exec 3"+redirect+"/dev/virtio-ports/"+guestagent.PortName)
			if code == 0 || !strings.Contains(o, "Permission denied") {
				t.Errorf("the agent user opening the port = %d %q, want EACCES", code, o)
			}
		})
	}
	t.Run("RunMarkerNotCreatable", func(t *testing.T) {
		if code, o := run(t, d, sb.Ref, "touch /run/wardyn-guest-init.probe && exit 3; [ -w /run ] && exit 4; exit 0"); code != 0 {
			t.Errorf("as the agent user = %d %q: 3 created a file in /run, 4 /run is writable", code, o)
		}
	})

	// The sign-in store (#2972): the guest runs no swap and no zram, and each keychain and browser profile
	// path is a tmpfs the agent user may write, so what is kept there is in the guest's memory only.
	t.Run("NoSwapOrZram", func(t *testing.T) {
		if code, o := run(t, d, sb.Ref, "cat /proc/swaps"); code != 0 || len(strings.Split(strings.TrimSpace(o), "\n")) != 1 {
			t.Errorf("/proc/swaps = %d %q, want its header only", code, o)
		}
	})
	t.Run("SignInStoreIsTmpfs", func(t *testing.T) {
		for _, rel := range guestagent.SignInPaths {
			p := guestagent.AgentHome + "/" + rel
			if code, o := run(t, d, sb.Ref, "[ \"$(stat -f -c %T "+p+")\" = tmpfs ] && touch "+p+"/probe"); code != 0 {
				t.Errorf("%s = %d %q, want a tmpfs the agent user writes", p, code, o)
			}
		}
	})
	// "Pause the agent to sign in" stops the agent's processes in the guest, and the guest (and so its
	// desktop) keeps running: a terminal exec still answers while the agent is stopped.
	t.Run("PauseAgentStopsOnlyTheAgent", func(t *testing.T) {
		if _, err := d.Exec(ctx, sb.Ref, []string{"sleep", "600"}); err != nil {
			t.Fatal(err)
		}
		state := func() string {
			_, o := run(t, d, sb.Ref, "sleep 0.5; ps -eo stat=,args=")
			for _, l := range strings.Split(o, "\n") {
				if f := strings.Fields(l); len(f) == 3 && f[1] == "sleep" && f[2] == "600" {
					return f[0]
				}
			}
			t.Logf("no sleep 600 in the guest's processes:\n%s", o)
			return ""
		}
		if err := d.PauseAgent(ctx, sb.Ref); err != nil {
			t.Fatal(err)
		}
		if st := state(); !strings.HasPrefix(st, "T") {
			t.Errorf("the paused agent's state = %q, want T", st)
		}
		if err := d.ResumeAgent(ctx, sb.Ref); err != nil {
			t.Fatal(err)
		}
		if st := state(); strings.HasPrefix(st, "T") || st == "" {
			t.Errorf("the resumed agent's state = %q, want running or sleeping", st)
		}
	})

	// Stop, then nothing of the run is left.
	if err := d.StopSandbox(ctx, sb.Ref); err != nil {
		t.Fatal(err)
	}
	torn = true
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the run directory survived teardown: %v", err)
	}
	list, err := cli.ContainerList(ctx, dockerclient.ContainerListOptions{All: true, Filters: dockerclient.Filters{}.Add("label", "wardyn.run-id="+spec.RunID.String())})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("teardown left %d containers", len(list.Items))
	}
}
