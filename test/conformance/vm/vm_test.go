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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
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
	// A DNS row (dns:answered, dns:refused) records what the relay told the guest, not traffic.
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
		case strings.HasPrefix(src, "dns:"):
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
		if _, err := d.PauseAgent(ctx, sb.Ref, 1); err != nil {
			t.Fatal(err)
		}
		if st := state(); !strings.HasPrefix(st, "T") {
			t.Errorf("the paused agent's state = %q, want T", st)
		}
		if _, err := d.ResumeAgent(ctx, sb.Ref, 1); err != nil {
			t.Fatal(err)
		}
		if st := state(); strings.HasPrefix(st, "T") || st == "" {
			t.Errorf("the resumed agent's state = %q, want running or sleeping", st)
		}
	})

	// The -L and UI-relay door: the guest has no socat, so a forward execs the agent's own forward mode,
	// which dials 127.0.0.1:<port> in the guest and takes nothing else.
	t.Run("ForwardDialsGuestLoopbackOnly", func(t *testing.T) {
		const agent = guestagent.LinuxAgentPath
		for _, arg := range []string{"127.0.0.1:7777", "::1", "127.0.0.2", "0.0.0.0", "localhost", "0", "65536", "http"} {
			if code, o := run(t, d, sb.Ref, agent+" forward "+arg+" </dev/null"); code != 2 {
				t.Errorf("forward %q = %d %q, want 2 (refused before any dial)", arg, code, o)
			}
		}
		if code, o := run(t, d, sb.Ref, agent+" forward 7778 </dev/null"); code != 1 {
			t.Errorf("forward to a port with no listener = %d %q, want 1", code, o)
		}
		// One-shot listener on the guest's loopback, as the agent user: reads a line, answers, closes.
		const listener = `perl -e 'use Socket; socket(S,PF_INET,SOCK_STREAM,0); setsockopt(S,SOL_SOCKET,SO_REUSEADDR,1); ` +
			`bind(S,sockaddr_in(7777,inet_aton("127.0.0.1"))) or die; listen(S,5); accept(C,S); select(C); $|=1; ` +
			`my $l=<C>; print C "echo:$l"; close C'`
		// A listener on 127.0.0.2 alone is not reached by the same port: forward dials 127.0.0.1 only.
		const otherLoopback = `perl -e 'use Socket; alarm 20; socket(S,PF_INET,SOCK_STREAM,0); setsockopt(S,SOL_SOCKET,SO_REUSEADDR,1); ` +
			`bind(S,sockaddr_in(7779,inet_aton("127.0.0.2"))) or die; listen(S,5); accept(C,S); print C "reached"; close C'`
		other := "(" + otherLoopback + " >/dev/null 2>&1 &); sleep 1; " + agent + " forward 7779 </dev/null"
		if code, o := run(t, d, sb.Ref, other); code != 1 || strings.Contains(o, "reached") {
			t.Errorf("forward with a listener on 127.0.0.2 only = %d %q, want 1 and no reached", code, o)
		}
		script := "(" + listener + " >/dev/null 2>&1 &); sleep 1; printf 'ping\\n' | " + agent + " forward 7777"
		if code, o := run(t, d, sb.Ref, script); code != 0 || !strings.Contains(o, "echo:ping") {
			t.Errorf("forward to the guest's loopback listener = %d %q, want 0 and echo:ping", code, o)
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

// asAgentContainer runs script in a container as the image agent's uid, with dir at /d: what a container
// run on the same drive is, and writes as. It returns the exit code.
func asAgentContainer(t *testing.T, cli *dockerclient.Client, dir, script string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	created, err := cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name:       "wardyn-vmconf-agent-" + uuid.NewString()[:8],
		Config:     &container.Config{Image: "busybox:latest", User: "1000:1000", Cmd: []string{"sh", "-c", script}},
		HostConfig: &container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeBind, Source: dir, Target: "/d"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, dockerclient.ContainerRemoveOptions{Force: true})
	}()
	if _, err := cli.ContainerStart(ctx, created.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	wait := cli.ContainerWait(ctx, created.ID, dockerclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wait.Result:
		return res.StatusCode
	case werr := <-wait.Error:
		t.Fatal(werr)
	}
	return -1
}

// inContainer runs argv in a running container and returns its exit code.
func inContainer(t *testing.T, cli *dockerclient.Client, name string, argv ...string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	created, err := cli.ExecCreate(ctx, name, dockerclient.ExecCreateOptions{Cmd: argv})
	if err != nil {
		t.Fatalf("exec %v in %s: %v", argv, name, err)
	}
	if _, err := cli.ExecStart(ctx, created.ID, dockerclient.ExecStartOptions{}); err != nil {
		t.Fatalf("exec %v in %s: %v", argv, name, err)
	}
	for {
		insp, err := cli.ExecInspect(ctx, created.ID, dockerclient.ExecInspectOptions{})
		if err != nil {
			t.Fatalf("exec %v in %s: %v", argv, name, err)
		}
		if !insp.Running {
			return insp.ExitCode
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startGuest starts script in the guest and returns a function that waits for its end and its exit code.
func startGuest(t *testing.T, d *docker.Driver, ref, script string) func() int {
	t.Helper()
	es, err := d.ExecStream(context.Background(), ref, runner.ExecSpec{Argv: []string{"bash", "-c", script}})
	if err != nil {
		t.Fatalf("exec %q: %v", script, err)
	}
	_ = es.Stdin.Close()
	return func() int {
		t.Helper()
		done := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, es.Stderr); close(done) }()
		_, _ = io.Copy(io.Discard, es.Stdout)
		<-done
		code, err := es.Wait()
		if err != nil {
			t.Fatalf("exec %q wait: %v", script, err)
		}
		return code
	}
}

// seenWithin polls script in the guest until it exits 0 and returns how long that took: an upper
// bound, since each poll is a round trip of its own. Never seeing it within 30 seconds fails the test.
func seenWithin(t *testing.T, d *docker.Driver, ref, script string) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < 30*time.Second {
		if code, _ := run(t, d, ref, script); code == 0 {
			return time.Since(start)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("not seen in the other VM within 30s: %s", script)
	return 0
}

// A drive bound to the runner in a real VM: the share is the person's home and nothing else, a
// read-only drive stays read-only (under a submount too), and two VMs share one drive. The hard link
// is the known residual: a link planted on the host inside a home reaches what it links to, as it
// does in a container, because the walk stops at names and a hard link has no name to refuse. The
// "two VMs on one share" line is the measurement the docs state.
func TestLinuxVMDrives(t *testing.T) {
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
	upNet := "wardyn-vmconf-up-" + uuid.NewString()[:8]
	if _, err := cli.NetworkCreate(ctx, upNet, dockerclient.NetworkCreateOptions{Driver: "bridge", Internal: true,
		IPAM: &network.IPAM{Config: []network.IPAMConfig{{Subnet: netip.MustParsePrefix(upSubnet)}}}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = cli.NetworkRemove(context.Background(), upNet, dockerclient.NetworkRemoveOptions{}) })

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	share, outside := filepath.Join(base, "share"), filepath.Join(base, "outside")
	alice, team, bob := filepath.Join(share, "alice"), filepath.Join(share, "team"), filepath.Join(share, "bob")
	sub := filepath.Join(team, "sub")
	for _, dir := range []string{alice, team, bob, outside, sub} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	const bobSecret, outsideSecret = "bob-secret-marker", "outside-secret-marker"
	for path, body := range map[string]string{
		filepath.Join(bob, "secret"):     bobSecret + "\n",
		filepath.Join(outside, "secret"): outsideSecret + "\n",
		filepath.Join(team, "file"):      "team file\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Planted on the host, as someone with a shell on the share could: links out of the home, and a
	// hard link at a sibling's file.
	for link, to := range map[string]string{
		filepath.Join(alice, "out-abs"): filepath.Join(outside, "secret"),
		filepath.Join(alice, "out-rel"): "../bob/secret",
		filepath.Join(share, "carol"):   bob,
		filepath.Join(share, "dave"):    outside,
	} {
		if err := os.Symlink(to, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(bob, "secret"), filepath.Join(alice, "hard")); err != nil {
		t.Fatal(err)
	}
	// A filesystem mounted inside the read-only home, which only the runner's host can put there. A
	// hosted runner has passwordless sudo; elsewhere this one case is skipped, and CI never skips it.
	submount := true
	if b, err := exec.Command("sudo", "-n", "mount", "-t", "tmpfs", "-o", "mode=0777,size=1m", "tmpfs", sub).CombinedOutput(); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("mount the submount: %v %s", err, b)
		}
		t.Logf("no submount case: %v %s", err, b)
		submount = false
	} else {
		t.Cleanup(func() { _ = exec.Command("sudo", "-n", "umount", "-l", sub).Run() })
		if err := os.WriteFile(filepath.Join(sub, "file"), []byte("submount file\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	d, err := docker.New(docker.Config{ProxyImage: proxyImage, InternalNetwork: upNet, UserDriveHostRoots: []string{share}})
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("", "wvm") // short: unix socket paths are bounded
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	vc, err := d.ProbeVM(ctx, docker.VMConfig{StateDir: state, QEMUImage: qemuImage, CPUModel: cmp.Or(os.Getenv("WARDYN_TEST_VM_CPU_MODEL"), "Haswell-v4")})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !vc.Virtiofs {
		t.Fatalf("probe found %+v", vc)
	}
	if err := d.EnableVM(&vc); err != nil {
		t.Fatal(err)
	}

	drive := func(home, target string, readOnly bool) types.DriveMount {
		return types.DriveMount{DriveID: uuid.New(), Backend: types.DriveBackendHostPath, ObjectName: filepath.Join(share, home), HostRoot: share,
			DriveName: "conformance-share", HomeName: home, Target: target, ReadOnly: readOnly}
	}
	spec := func(drives ...types.DriveMount) runner.SandboxSpec {
		return runner.SandboxSpec{
			RunID: uuid.New(), Image: guestImage, MachineKind: types.MachineLinuxVM, ConfinementClass: types.CC1,
			ProxyConfig: runner.ProxyConfig{RunToken: "tok", ControlPlaneURL: "http://127.0.0.1:8080", Policy: types.RunPolicySpec{
				AllowedDomains: []string{"origin.test"}, FirstUseApproval: types.FirstUseAlwaysDeny, MinConfinementClass: types.CC1}},
			Resources: runner.Resources{CPUMillis: 2000, MemoryMiB: 2048},
			Drives:    drives, ExecOutput: &lockedBuffer{},
		}
	}
	boot := func(s runner.SandboxSpec) string {
		t.Helper()
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute) // a first image conversion included
		defer cancel()
		sb, err := d.CreateSandbox(cctx, s)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })
		return sb.Ref
	}

	// A home that is a link at a sibling's home, or out of the drive's root, never starts.
	for _, home := range []string{"carol", "dave"} {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		sb, err := d.CreateSandbox(cctx, spec(drive(home, runner.DriveTarget, false)))
		cancel()
		if err == nil {
			_ = d.KillSandbox(context.Background(), sb.Ref)
		}
		if err == nil || !strings.Contains(err.Error(), "denied user drive") {
			t.Fatalf("a home linked out (%s): create = %v, want denied user drive", home, err)
		}
	}

	const rw, ro = runner.DriveTarget, runner.SharedRoot + "/team"
	specA := spec(drive("alice", rw, false), drive("team", ro, true))
	a := boot(specA)

	// The share's container holds the home it serves and no more, in the mode the drive has.
	for n := range 2 {
		name := fmt.Sprintf("wardyn-vmfs-%s-%d", specA.RunID, n)
		fs, err := cli.ContainerInspect(ctx, name, dockerclient.ContainerInspectOptions{})
		if err != nil {
			t.Fatalf("the VM's share %d: %v", n, err)
		}
		cmdline := strings.Join(fs.Container.Config.Cmd, " ")
		for _, m := range fs.Container.HostConfig.Mounts {
			if m.Target != "/share" {
				continue
			}
			switch m.Source {
			case alice:
				if m.ReadOnly || strings.Contains(cmdline, "--readonly") {
					t.Fatalf("the writable home's share: %+v, %s", m, cmdline)
				}
				if code := inContainer(t, cli, name, "touch", "/share/from-the-share"); code != 0 {
					t.Fatalf("a write from the writable share's own container = %d: this probe proves nothing", code)
				}
			case team:
				if !m.ReadOnly || !strings.Contains(cmdline, "--readonly") {
					t.Fatalf("the read-only home's share: %+v, %s", m, cmdline)
				}
				// Below the guest: the bind itself refuses a write, whatever a guest could ask.
				paths := []string{"/share/nope"}
				if submount {
					paths = append(paths, "/share/sub/nope")
				}
				for _, p := range paths {
					if code := inContainer(t, cli, name, "touch", p); code == 0 {
						t.Errorf("the read-only share's bind took a write at %s", p)
					}
				}
			default:
				t.Fatalf("the share binds %s, which is neither home", m.Source)
			}
		}
	}

	// The read-only drive in the guest: read, never written, by any route.
	if code, o := run(t, d, a, `while read -r _ t _ o _; do [ "$t" = "`+ro+`" ] && echo "$o"; done </proc/mounts; true`); code != 0 ||
		!strings.HasPrefix(o, "ro,") || !strings.Contains(o, "nosuid") || !strings.Contains(o, "nodev") {
		t.Fatalf("the read-only drive's guest mount = %d %q", code, o)
	}
	if code, o := run(t, d, a, "cat "+ro+"/file"); code != 0 || !strings.Contains(o, "team file") {
		t.Fatalf("reading the read-only drive = %d %q", code, o)
	}
	writes := []string{"touch " + ro + "/nope", "echo x >> " + ro + "/file", "rm " + ro + "/file", "mkdir " + ro + "/nope.d"}
	if submount {
		if code, o := run(t, d, a, "cat "+ro+"/sub/file"); code != 0 || !strings.Contains(o, "submount file") {
			t.Fatalf("reading the submount in the guest = %d %q: its write refusal would prove nothing", code, o)
		}
		writes = append(writes, "touch "+ro+"/sub/nope", "echo x >> "+ro+"/sub/file")
	}
	for _, script := range writes {
		if code, o := run(t, d, a, script); code == 0 {
			t.Errorf("%q on a read-only drive succeeded: %q", script, o)
		}
	}
	if b, err := os.ReadFile(filepath.Join(team, "file")); err != nil || string(b) != "team file\n" {
		t.Fatalf("the read-only drive's file on the host: %q %v", b, err)
	}
	for _, p := range []string{filepath.Join(team, "nope"), filepath.Join(team, "nope.d"), filepath.Join(sub, "nope")} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the read-only drive took a write: %s: %v", p, err)
		}
	}

	// The writable drive: a write lands in the home, as the runner's user.
	if code, o := run(t, d, a, "echo from-the-guest > "+rw+"/made"); code != 0 {
		t.Fatalf("a write to the writable drive = %d %q", code, o)
	}
	fi, err := os.Stat(filepath.Join(alice, "made"))
	if err != nil {
		t.Fatalf("the guest's write on the host: %v", err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != vc.UID || int(st.Gid) != vc.GID {
		t.Fatalf("the guest's write is not the runner's %d:%d: %+v", vc.UID, vc.GID, fi.Sys())
	}

	// Out of the share: a link's target is read in the guest, so it names nothing on the host.
	for name, script := range map[string]string{
		"an absolute link planted on the host": "cat " + rw + "/out-abs",
		"a link at a sibling's home":           "cat " + rw + "/out-rel",
		"a walk up":                            "cat " + rw + "/../bob/secret; ls -a " + rw + "/..",
		"a link the guest made":                "ln -s ../bob/secret " + rw + "/guest-link; cat " + rw + "/guest-link",
		"a link the guest made at a host path": "ln -s " + filepath.Join(bob, "secret") + " " + rw + "/guest-abs; cat " + rw + "/guest-abs",
	} {
		_, o := run(t, d, a, script)
		if strings.Contains(o, bobSecret) || strings.Contains(o, outsideSecret) || strings.Contains(o, "bob\n") {
			t.Errorf("%s reached outside the share: %q", name, o)
		}
	}
	// The only way the sibling's bytes are in this home is the hard link.
	if code, o := run(t, d, a, "grep -rlsF "+bobSecret+" "+rw+" "+ro); code != 0 || strings.TrimSpace(o) != rw+"/hard" {
		t.Errorf("a sweep of the guest's drives for the sibling's secret = %d %q, want only the hard link", code, o)
	}
	// The residual, pinned so a change to it is seen: a hard link planted on the host is that file.
	if code, o := run(t, d, a, "cat "+rw+"/hard && echo via-hard >> "+rw+"/hard"); code != 0 || !strings.Contains(o, bobSecret) {
		t.Errorf("the hard link planted on the host = %d %q, want it to read and write through, as in a container", code, o)
	}
	if b, err := os.ReadFile(filepath.Join(bob, "secret")); err != nil || !strings.Contains(string(b), "via-hard") {
		t.Errorf("the write through the hard link on the host: %q %v", b, err)
	}

	// A container and a VM on one drive, by what the runner's uid is. Equal to the image agent's they
	// are one user; otherwise each reads what the other wrote and neither can change it. A hosted
	// runner has the second, so the first is proved only on a host whose runner is uid 1000.
	sameUser := vc.UID == 1000
	if code := asAgentContainer(t, cli, alice, "echo from-a-container > /d/from-container"); code != 0 {
		t.Fatalf("a container's write to the drive = %d", code)
	}
	if code, o := run(t, d, a, "cat "+rw+"/from-container"); code != 0 || !strings.Contains(o, "from-a-container") {
		t.Errorf("the VM reading a container's write = %d %q", code, o)
	}
	vmWrites, _ := run(t, d, a, "echo x >> "+rw+"/from-container")
	containerWrites := asAgentContainer(t, cli, alice, "echo x >> /d/made")
	if (vmWrites == 0) != sameUser || (containerWrites == 0) != sameUser {
		t.Errorf("runner uid %d: the VM changing a container's file = %d, a container changing the VM's = %d", vc.UID, vmWrites, containerWrites)
	}
	t.Logf("a container and a VM on one share: runner uid %d, VM changes a container's file: %d, container changes the VM's: %d", vc.UID, vmWrites, containerWrites)

	// Two VMs on one share.
	b := boot(spec(drive("alice", rw, false)))
	if code, o := run(t, d, a, "echo one > "+rw+"/new"); code != 0 {
		t.Fatalf("a write in the first VM = %d %q", code, o)
	}
	newFile := seenWithin(t, d, b, "grep -q one "+rw+"/new")
	if code, o := run(t, d, b, "cat "+rw+"/new; echo two > "+rw+"/from-b"); code != 0 {
		t.Fatalf("the second VM on the share = %d %q", code, o)
	}
	otherWay := seenWithin(t, d, a, "grep -q two "+rw+"/from-b")
	// The second VM has the file open in its cache by now: an overwrite, then a delete.
	if code, o := run(t, d, a, "echo edited > "+rw+"/new"); code != 0 {
		t.Fatalf("an overwrite in the first VM = %d %q", code, o)
	}
	overwrite := seenWithin(t, d, b, "grep -q edited "+rw+"/new")
	if code, o := run(t, d, a, "rm "+rw+"/new"); code != 0 {
		t.Fatalf("a delete in the first VM = %d %q", code, o)
	}
	deleted := seenWithin(t, d, b, "! test -e "+rw+"/new")

	// Both append at once: two containers on one folder lose no line.
	const lines = 500
	appendAll := func(who string) string {
		return fmt.Sprintf("for i in $(seq %d); do echo %s$i >> %s/appended; done", lines, who, rw)
	}
	waitA, waitB := startGuest(t, d, a, appendAll("a")), startGuest(t, d, b, appendAll("b"))
	if ca, cb := waitA(), waitB(); ca != 0 || cb != 0 {
		t.Fatalf("appending from both VMs = %d, %d", ca, cb)
	}
	appended, err := os.ReadFile(filepath.Join(alice, "appended"))
	if err != nil {
		t.Fatal(err)
	}
	gotLines := strings.Count(string(appended), "\n")
	if gotLines != 2*lines {
		t.Errorf("two VMs appending %d lines each left %d lines", lines, gotLines)
	}

	// A lock: the first VM holds a flock. Its own second process is the control. On one kernel, as two
	// containers on a folder are, the others would be refused too; whether they are is the measurement.
	hold := startGuest(t, d, a, "flock "+rw+"/lock bash -c 'touch "+rw+"/held; until test -e "+rw+"/release; do sleep 0.2; done'")
	seenWithin(t, d, a, "test -e "+rw+"/held")
	if code, _ := run(t, d, a, "flock -n "+rw+"/lock true"); code == 0 {
		t.Fatal("a second process in the VM that holds the flock got it: the lock proves nothing")
	}
	otherVMLock, _ := run(t, d, b, "flock -n "+rw+"/lock true")
	lockFile, err := os.Open(filepath.Join(alice, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	hostLockErr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	_ = lockFile.Close()
	if code, o := run(t, d, a, "touch "+rw+"/release"); code != 0 {
		t.Fatalf("releasing the holder = %d %q", code, o)
	}
	if code := hold(); code != 0 {
		t.Fatalf("the flock holder = %d", code)
	}
	t.Logf("two VMs on one share: new file seen in %v (other way %v), overwrite seen in %v, delete seen in %v; "+
		"%d of %d lines kept when both append; flock held in one VM: other VM's try exit %d (nonzero = refused), host's try %v (nil = not refused)",
		newFile, otherWay, overwrite, deleted, gotLines, 2*lines, otherVMLock, hostLockErr)
}
