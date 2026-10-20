// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

const vmTestImage = "vm-linux-test"

// vmRig is a driver with VMs on over the fake Docker API, whose QEMU container start brings up a
// fake QEMU: the real guest agent handler on ctl/agent.sock (running execs as this test's user),
// and canned qemu-ga and QMP answers on their sockets.
type vmRig struct {
	f      *fakeDocker
	d      *Driver
	vc     *VMConfig
	mu     sync.Mutex
	qmp    []string // QMP commands, in order
	qga    []string // qemu-ga commands, in order
	mounts []guestagent.Mount
	init   *guestagent.Init
	lns    []net.Listener
}

func newVMRig(t *testing.T) *vmRig {
	t.Helper()
	state, err := os.MkdirTemp("", "vmt") // short: unix socket paths are bounded
	if err != nil {
		t.Fatal(err)
	}
	r := &vmRig{f: newFakeDocker()}
	t.Cleanup(func() {
		r.mu.Lock()
		for _, ln := range r.lns {
			_ = ln.Close()
		}
		r.mu.Unlock()
		_ = os.RemoveAll(state)
	})
	r.f.images["wardyn/vm-qemu:test"] = true
	r.f.images[vmTestImage] = true
	r.f.exportTar = []byte("a tar")
	r.d = newTestDriver(r.f)
	r.vc = &VMConfig{StateDir: state, QEMUImage: "wardyn/vm-qemu:test", CPUModel: "Haswell-v4", MachineType: "pc-q35-10.0",
		QEMUVersion: "10.0.13", UID: os.Getuid(), GID: os.Getgid(), KVMGID: "992", BootTimeout: 20 * time.Second}
	if r.vc.UID == 0 {
		r.vc.UID, r.vc.GID = 65532, 65532
	}
	if err := r.d.EnableVM(r.vc); err != nil {
		t.Fatal(err)
	}
	r.f.onStart = func(c *createdContainer) { r.start(t, c) }
	return r
}

func mountSource(c *createdContainer, target string) string {
	for _, m := range c.host.Mounts {
		if m.Target == target {
			return m.Source
		}
	}
	return ""
}

func (r *vmRig) listen(t *testing.T, path string) net.Listener {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Errorf("listen %s: %v", path, err)
		return nil
	}
	r.mu.Lock()
	r.lns = append(r.lns, ln)
	r.mu.Unlock()
	return ln
}

// start plays each container's part: prep steps write their outputs, virtiofsd its socket, and
// QEMU its sockets.
func (r *vmRig) start(t *testing.T, c *createdContainer) {
	switch {
	case strings.HasSuffix(c.name, "-image") && strings.HasPrefix(c.name, "wardyn-vmprep-"):
		out := mountSource(c, "/out")
		if b, _ := os.ReadFile(filepath.Join(out, "rootfs.tar")); string(b) != "a tar" {
			t.Errorf("the image step got %q, not the export", b)
		}
		for _, f := range []string{qemu.RootFS, qemu.Kernel, qemu.Initrd} {
			_ = os.WriteFile(filepath.Join(out, f), []byte(f), 0o600)
		}
	case strings.HasSuffix(c.name, "-overlay"):
		_ = os.WriteFile(filepath.Join(mountSource(c, qemu.DiskDir), qemu.Overlay), nil, 0o600)
	case strings.HasPrefix(c.name, "wardyn-vmfs-"):
		n := c.name[strings.LastIndex(c.name, "-")+1:]
		r.listen(t, filepath.Join(mountSource(c, qemu.FSDir), n+".sock"))
	case strings.HasPrefix(c.name, agentNamePrefix) && c.cfg.Labels[labelMachineKind] == "linux_vm":
		ctl := mountSource(c, qemu.CtlDir)
		r.serveAgent(t, filepath.Join(ctl, qemu.AgentSocket))
		r.serveLines(t, filepath.Join(ctl, qemu.QGASocket), false)
		r.serveLines(t, filepath.Join(ctl, qemu.QMPSocket), true)
	}
}

func (r *vmRig) serveAgent(t *testing.T, path string) {
	home := t.TempDir()
	srv := &guestagent.Server{Version: "test", Home: home, OutDir: t.TempDir(), Mount: func(m guestagent.Mount) error {
		r.mu.Lock()
		r.mounts = append(r.mounts, m)
		r.mu.Unlock()
		return nil
	}}
	h := srv.Handler()
	spy := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/init" {
			var in guestagent.Init
			b := new(bytes.Buffer)
			_, _ = b.ReadFrom(req.Body)
			_ = json.Unmarshal(b.Bytes(), &in)
			r.mu.Lock()
			r.init = &in
			r.mu.Unlock()
			req.Body = readCloser{bytes.NewReader(b.Bytes())}
		}
		h.ServeHTTP(w, req)
	})
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	hs := &http.Server{Handler: spy, Protocols: &p, ReadHeaderTimeout: 5 * time.Second}
	if ln := r.listen(t, path); ln != nil {
		go func() { _ = hs.Serve(ln) }()
	}
}

type readCloser struct{ *bytes.Reader }

func (readCloser) Close() error { return nil }

// serveLines answers qemu-ga (echoing guest-sync's id) or QMP (greeting first) line by line.
func (r *vmRig) serveLines(t *testing.T, path string, qmp bool) {
	ln := r.listen(t, path)
	if ln == nil {
		return
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if qmp {
					_, _ = conn.Write([]byte(`{"QMP": {"version": {}, "capabilities": []}}` + "\n"))
				}
				sc := bufio.NewScanner(conn)
				for sc.Scan() {
					var req struct {
						Execute   string          `json:"execute"`
						Arguments json.RawMessage `json:"arguments"`
					}
					_ = json.Unmarshal(sc.Bytes(), &req)
					r.mu.Lock()
					if qmp {
						r.qmp = append(r.qmp, req.Execute)
					} else {
						r.qga = append(r.qga, req.Execute)
					}
					r.mu.Unlock()
					ret := `{}`
					switch req.Execute {
					case "guest-sync":
						var a struct {
							ID int64 `json:"id"`
						}
						_ = json.Unmarshal(req.Arguments, &a)
						b, _ := json.Marshal(a.ID)
						ret = string(b)
					case "query-kvm":
						ret = `{"enabled": true, "present": true}`
					}
					_, _ = conn.Write([]byte(`{"return": ` + ret + "}\n"))
				}
			}()
		}
	}()
}

func vmSpec() runner.SandboxSpec {
	s := testSpec()
	s.MachineKind = types.MachineLinuxVM
	s.Image = vmTestImage
	s.Resources = runner.Resources{CPUMillis: 1500, MemoryMiB: 4096}
	s.ProxyConfig.MITMCACertPEM = "-----BEGIN CERTIFICATE-----\nCA\n-----END CERTIFICATE-----"
	return s
}

func (r *vmRig) container(t *testing.T, name string) *createdContainer {
	t.Helper()
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	c := r.f.containers[name]
	if c == nil {
		t.Fatalf("no container %s", name)
	}
	return c
}

func TestVMCreateHardensEveryContainerAndRecordsTheMachine(t *testing.T) {
	r := newVMRig(t)
	ws := t.TempDir()
	spec := vmSpec()
	spec.Mounts = []runner.Mount{{Source: ws, Target: "/home/agent/work"}, {Source: t.TempDir(), Target: "/workspace/ref", ReadOnly: true}}
	out := &syncWriter{w: &bytes.Buffer{}}
	spec.ExecOutput = out
	ctx := context.Background()
	sb, err := r.d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Ref != agentContainerName(spec.RunID) || sb.EnforcedClass != types.CC3 {
		t.Fatalf("sandbox %+v: the VM is the run's agent ref, a hardware (CC3-family) boundary", sb)
	}
	if !sb.Machine.Complete() || *sb.Machine != (types.VMMachine{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", Accelerator: "kvm", QEMUVersion: "10.0.13"}) {
		t.Fatalf("machine %+v: the run records what QEMU was started with", sb.Machine)
	}

	dir := r.vc.runDir(spec.RunID)
	user := r.vc.user()
	q := r.container(t, sb.Ref)
	for name, c := range map[string]*createdContainer{"qemu": q, "vmfs0": r.container(t, vmfsName(spec.RunID, 0)), "vmfs1": r.container(t, vmfsName(spec.RunID, 1)),
		"overlay": r.container(t, vmPrepName(spec.RunID, "overlay")), "image": r.container(t, vmPrepName(spec.RunID, "image"))} {
		h := c.host
		if c.cfg.User != user || c.cfg.User == "0" || strings.HasPrefix(c.cfg.User, "0:") {
			t.Errorf("%s runs as %q, not the runner's own uid", name, c.cfg.User)
		}
		if h.Privileged || !slices.Equal(h.CapDrop, []string{"ALL"}) || !slices.Contains(h.SecurityOpt, "no-new-privileges") ||
			string(h.NetworkMode) != "none" || !h.ReadonlyRootfs || h.PidsLimit == nil || h.Memory == 0 {
			t.Errorf("%s host config not hardened: %+v", name, h)
		}
		for _, m := range h.Mounts {
			if m.Source == filepath.Join(dir, "ctl") && name != "qemu" {
				t.Errorf("%s mounts ctl/ (QMP and the unauthenticated VNC socket)", name)
			}
		}
		if name != "qemu" && (len(h.Devices) != 0 || len(h.GroupAdd) != 0) {
			t.Errorf("%s got a device or group: only QEMU opens /dev/kvm", name)
		}
	}
	if len(q.host.Devices) != 1 || q.host.Devices[0].PathOnHost != "/dev/kvm" || !slices.Equal(q.host.GroupAdd, []string{"992"}) {
		t.Fatalf("QEMU devices %+v groups %v: /dev/kvm and its group, nothing else", q.host.Devices, q.host.GroupAdd)
	}
	if q.host.Memory != 4096<<20 || q.cfg.Labels[labelMachineKind] != "linux_vm" || q.cfg.Labels[labelRun] != spec.RunID.String() {
		t.Fatalf("QEMU memory %d labels %v: the container's limit is the run's cap", q.host.Memory, q.cfg.Labels)
	}
	cmd := strings.Join(q.cfg.Cmd, " ")
	for _, want := range []string{"-machine pc-q35-10.0,accel=kvm", "-cpu Haswell-v4,enforce", "-smp 2", "-m 3277",
		"tag=wardyn0", "tag=wardyn1", "name=" + qemu.FWCfgCA} {
		if !strings.Contains(cmd, want) {
			t.Errorf("QEMU argv lacks %q: %s", want, cmd)
		}
	}
	ro := map[string]bool{}
	for _, m := range q.host.Mounts {
		ro[m.Target] = m.ReadOnly
	}
	if !ro[qemu.BaseDir] || !ro[qemu.FWCfgDir] || ro[qemu.CtlDir] || ro[qemu.DiskDir] {
		t.Fatalf("QEMU mounts %v: the base and fw_cfg are read-only", ro)
	}
	if fs := r.container(t, vmfsName(spec.RunID, 1)); !slices.Contains(fs.cfg.Cmd, "--readonly") || mountSource(fs, "/share") == "" {
		t.Fatalf("a read-only mount is served read-only: %v", fs.cfg.Cmd)
	}

	// The run directory's mode is the control over VNC and QMP.
	for _, p := range append([]string{""}, runParts...) {
		fi, err := os.Stat(filepath.Join(dir, p))
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("run dir part %q: %v %v, want 0700", p, fi.Mode(), err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "fwcfg", qemu.CAFile)); err != nil || fi.Mode().Perm() != 0o400 {
		t.Fatalf("ca.pem: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "fwcfg", qemu.CAFile)); strings.Contains(string(b), "PRIVATE") || !strings.Contains(string(b), "CERTIFICATE") {
		t.Fatalf("fw_cfg carries %q: the certificate only", b)
	}

	r.mu.Lock()
	if !slices.Contains(r.qga, "guest-set-time") || !slices.Contains(r.qmp, "query-kvm") || r.init == nil ||
		r.init.Env["FOO"] != "bar" || len(r.mounts) != 2 || r.mounts[0] != (guestagent.Mount{Tag: "wardyn0", Target: "/home/agent/work"}) || !r.mounts[1].ReadOnly {
		t.Fatalf("boot: qga %v qmp %v init %+v mounts %+v", r.qga, r.qmp, r.init, r.mounts)
	}
	r.mu.Unlock()

	// The agent process runs through the guest agent, never docker exec.
	id, err := r.d.Exec(ctx, sb.Ref, []string{"sh", "-c", "echo from-the-guest; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if code, err := r.d.Wait(wctx, sb.Ref); err != nil || code != 3 {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	if st, err := r.d.AgentStatus(ctx, sb.Ref, id); err != nil || st.State != types.RunStopped || st.ExitCode == nil || *st.ExitCode != 3 {
		t.Fatalf("AgentStatus = %+v, %v", st, err)
	}
	if r.f.lastExecCmd != nil {
		t.Fatalf("a VM ref got a docker exec: %v", r.f.lastExecCmd)
	}
	waitFor(t, func() bool { return strings.Contains(out.String(), "from-the-guest") })

	es, err := r.d.ExecStream(ctx, sb.Ref, runner.ExecSpec{Argv: []string{"sh", "-c", "cat; echo done >&2"}})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = es.Stdin.Write([]byte("ping\n")); _ = es.Stdin.Close() }()
	var so, se bytes.Buffer
	done := make(chan struct{})
	go func() { _, _ = se.ReadFrom(es.Stderr); close(done) }()
	_, _ = so.ReadFrom(es.Stdout)
	<-done
	if code, err := es.Wait(); err != nil || code != 0 || so.String() != "ping\n" || se.String() != "done\n" {
		t.Fatalf("exec stream: %d %v %q %q", code, err, so.String(), se.String())
	}

	if err := r.d.StopSandbox(ctx, sb.Ref); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	powered := slices.Contains(r.qmp, "system_powerdown")
	r.mu.Unlock()
	if !powered {
		t.Fatal("stop pulled the plug without asking the guest to power down")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("teardown left the run directory: %v", err)
	}
	for _, n := range []string{sb.Ref, vmfsName(spec.RunID, 0), vmfsName(spec.RunID, 1), proxyContainerName(spec.RunID)} {
		if c := r.container(t, n); !c.removed {
			t.Errorf("teardown left %s", n)
		}
	}
	if _, ok := r.f.networks[internalNetName(spec.RunID)]; ok {
		t.Error("teardown left the run's network")
	}
}

func TestVMImageIsConvertedOncePerImage(t *testing.T) {
	r := newVMRig(t)
	ctx := context.Background()
	for i := range 2 {
		spec := vmSpec()
		spec.RunID = uuid.New()
		sb, err := r.d.CreateSandbox(ctx, spec)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if err := r.d.KillSandbox(ctx, sb.Ref); err != nil {
			t.Fatal(err)
		}
	}
	if r.f.exports != 1 {
		t.Fatalf("%d exports: the converted base is cached by image id", r.f.exports)
	}
	if _, err := os.Stat(filepath.Join(r.vc.imageDir(vmTestImage), qemu.RootFS)); err != nil {
		t.Fatal(err)
	}
}

// agentStart is an interactive run that opens its terminal in the agent CLI.
func agentStart(s *runner.SandboxSpec) {
	s.Interactive, s.Env = true, map[string]string{envInteractiveStart: "agent"}
}

func TestVMSpecRefusalsCreateNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		off  bool
		rec  bool // the runner records sessions
		mut  func(*runner.SandboxSpec)
		want string
	}{
		"VMs off":      {off: true, want: "does not offer Linux VM"},
		"unknown kind": {mut: func(s *runner.SandboxSpec) { s.MachineKind = "windows_vm" }, want: "unknown machine kind"},
		"drives":       {mut: func(s *runner.SandboxSpec) { s.Drives = []types.DriveMount{{ObjectName: "d"}} }, want: "no drives"},
		"record":       {rec: true, want: "not recorded"},
		"agent start":  {mut: agentStart, want: "not the agent CLI"},
		"tiny":         {mut: func(s *runner.SandboxSpec) { s.Resources.MemoryMiB = 512 }, want: "256 MiB"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDocker()
			d := newTestDriver(f)
			d.cfg.Record = tc.rec
			if !tc.off {
				if err := d.EnableVM(&VMConfig{StateDir: "/tmp/vmx", QEMUImage: "q", CPUModel: "Haswell-v4", MachineType: "pc-q35-10.0", UID: 1000, GID: 1000, KVMGID: "1"}); err != nil {
					t.Fatal(err)
				}
			}
			spec := vmSpec()
			if tc.mut != nil {
				tc.mut(&spec)
			}
			if _, err := d.CreateSandbox(context.Background(), spec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if len(f.containers) != 0 || len(f.networks) != 0 {
				t.Fatalf("a refused VM created %d containers, %d networks", len(f.containers), len(f.networks))
			}
		})
	}
}

func TestVMConfigRefusesRootAndLongPaths(t *testing.T) {
	ok := VMConfig{StateDir: "/var/lib/wardyn-runner/vm", QEMUImage: "q", CPUModel: "Haswell-v4", MachineType: "pc-q35-10.0", UID: 1000, GID: 1000, KVMGID: "1"}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*VMConfig){
		"root":     func(c *VMConfig) { c.UID = 0 },
		"relative": func(c *VMConfig) { c.StateDir = "vm" },
		"long":     func(c *VMConfig) { c.StateDir = "/" + strings.Repeat("x", 60) },
		"root gid": func(c *VMConfig) { c.KVMGID = "0" },
		"cpu":      func(c *VMConfig) { c.CPUModel = "host" + ",+x" },
	} {
		c := ok
		mut(&c)
		if err := c.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// After a runner restart the driver finds the VM again by its container's label, and refuses the
// verbs a VM does not serve.
func TestVMRefFoundAgainAndUnsupportedVerbsRefused(t *testing.T) {
	r := newVMRig(t)
	ctx := context.Background()
	spec := vmSpec()
	sb, err := r.d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	d2 := newTestDriver(r.f)
	if err := d2.EnableVM(r.vc); err != nil {
		t.Fatal(err)
	}
	if v := d2.vmFor(ctx, sb.Ref); v == nil || v.runID != spec.RunID {
		t.Fatal("a restarted driver did not find the VM by its label")
	}
	for name, err := range map[string]error{
		"start":             d2.StartSandbox(ctx, sb.Ref),
		"can_replace_proxy": d2.CanReplaceProxy(ctx, sb.Ref),
		"replace_proxy":     d2.ReplaceProxy(ctx, sb.Ref, []byte("{}")),
		"recover_output":    d2.RecoverOutput(ctx, sb.Ref, &bytes.Buffer{}),
	} {
		if !errors.Is(err, errVMUnsupported) {
			t.Errorf("%s on a VM = %v", name, err)
		}
	}
	// A container ref on the same driver is untouched by the VM path.
	if d2.vmFor(ctx, proxyContainerName(spec.RunID)) != nil {
		t.Fatal("a container that is not a VM was taken for one")
	}
	if err := r.d.KillSandbox(ctx, sb.Ref); err != nil {
		t.Fatal(err)
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.String()
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A probe VM that opened its QMP socket and then exited (QEMU checks the CPU model after it creates
// its sockets) is reported with the fix its log names, not as a bare QMP read error.
func TestVMProbeQMPFailureNamesTheFixFromQEMUsLog(t *testing.T) {
	f := newFakeDocker()
	d := newTestDriver(f)
	created, err := f.ContainerCreate(context.Background(), client.ContainerCreateOptions{Name: "wardyn-vmprobe-x", Config: &container.Config{Image: "q"}})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.logs = map[string][]byte{created.ID: muxFrame(2, "qemu-system-x86_64: Host doesn't support requested features: CPUID.07H:EDX.spec-ctrl [bit 26]\nqemu-system-x86_64: host doesn't support requested feature")}
	f.mu.Unlock()
	qmpErr := errors.New("qemu: QMP qmp_capabilities: read: connection reset by peer")
	if err := d.probeExited(context.Background(), created.ID, qmpErr); err == nil || !strings.Contains(err.Error(), "WARDYN_RUNNER_VM_CPU_MODEL") {
		t.Fatalf("probe error = %v, want the CPU model fix", err)
	}
}

// A teardown that fails part-way (here the proxy's removal) still removes the VM's virtiofsd
// sidecars, which hold the member's folders, and its run directory.
func TestVMTeardownRemovesSidecarsAndDirWhenProxyRemovalFails(t *testing.T) {
	r := newVMRig(t)
	spec := vmSpec()
	spec.Mounts = []runner.Mount{{Source: t.TempDir(), Target: "/home/agent/work"}}
	ctx := context.Background()
	sb, err := r.d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	r.f.mu.Lock()
	r.f.failRemove = map[string]int{proxyContainerName(spec.RunID): 1}
	r.f.mu.Unlock()
	if err := r.d.KillSandbox(ctx, sb.Ref); err == nil || !strings.Contains(err.Error(), "remove proxy") {
		t.Fatalf("kill = %v, want the proxy removal's failure", err)
	}
	if c := r.container(t, vmfsName(spec.RunID, 0)); !c.removed {
		t.Fatal("the virtiofsd sidecar survived a teardown whose proxy removal failed")
	}
	if _, err := os.Stat(r.vc.runDir(spec.RunID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the run directory survived a teardown whose proxy removal failed: %v", err)
	}
}

// The sweep removes a crashed VM's sidecars and run directory, labelled as createVM labels them,
// once its run has no QEMU container and isOrphan says the run owns nothing; never while the run
// is live (a create in flight has residue and no QEMU container yet), however old, and never
// another runner's.
func TestVMSweepRemovesOrphanedSidecarsAndRunDirs(t *testing.T) {
	r := newVMRig(t)
	rid := uuid.NewString()
	r.d.cfg.RunnerID, r.d.cfg.DecisionSink, r.d.cfg.Evidence = rid, newFakeSink(), &memEvidence{m: map[uuid.UUID][]byte{}}
	spec := vmSpec()
	spec.Labels = map[string]string{runner.LabelRunnerID: rid} // as runneragent fills them
	spec.Mounts = []runner.Mount{{Source: t.TempDir(), Target: "/home/agent/work"}}
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	// The runner crashed: the sidecars createVM made are all still there, the QEMU container is not.
	past := time.Now().Add(-time.Hour)
	theirs := uuid.New()
	r.f.mu.Lock()
	var items []container.Summary
	comps := map[string]bool{}
	seen := map[*createdContainer]bool{}
	for name, c := range r.f.containers {
		comp := c.cfg.Labels[labelComponent]
		if seen[c] || name != c.name || (comp != componentVMFS && comp != componentVMPrep) {
			continue
		}
		seen[c], c.removed, comps[comp] = true, false, true
		items = append(items, container.Summary{ID: name, Created: past.Unix(), Labels: c.cfg.Labels})
	}
	other := maps.Clone(r.f.containers[vmfsName(spec.RunID, 0)].cfg.Labels)
	other[labelRun], other[runner.LabelRunnerID] = theirs.String(), uuid.NewString()
	r.f.containers[vmfsName(theirs, 0)] = &createdContainer{name: vmfsName(theirs, 0)}
	items = append(items, container.Summary{ID: vmfsName(theirs, 0), Created: past.Unix(), Labels: other})
	r.f.containers[sb.Ref].removed = true
	r.f.listItems = items
	r.f.mu.Unlock()
	if !comps[componentVMFS] || !comps[componentVMPrep] {
		t.Fatalf("createVM made sidecars of %v, want virtiofsd and prep", comps)
	}
	dir := r.vc.runDir(spec.RunID)
	if err := os.Chtimes(dir, past, past); err != nil {
		t.Fatal(err)
	}
	removed := func() (n int) {
		r.f.mu.Lock()
		defer r.f.mu.Unlock()
		for _, it := range items {
			if r.f.containers[it.ID].removed {
				n++
			}
		}
		return n
	}

	// A live run (a create in flight, or one whose QEMU container is being made) keeps everything,
	// even with no minimum age, as the on-connect sweep passes.
	if n, err := r.d.SweepOrphanedSandboxes(context.Background(), 0, func(uuid.UUID) bool { return false }); err != nil || n != 0 || removed() != 0 {
		t.Fatalf("sweep of a live run = %d, %v, %d removed; want nothing", n, err, removed())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a live run's directory was removed: %v", err)
	}

	n, err := r.d.SweepOrphanedSandboxes(context.Background(), 10*time.Minute, func(uuid.UUID) bool { return true })
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want the one orphaned run", n, err)
	}
	r.f.mu.Lock()
	for _, it := range items {
		if r.f.containers[it.ID].removed != (it.Labels[labelRun] == spec.RunID.String()) {
			t.Errorf("%s removed = %v", it.ID, r.f.containers[it.ID].removed)
		}
	}
	r.f.mu.Unlock()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the orphaned run's directory survived: %v", err)
	}
}

// A container's ref is inspected once while VMs are on, not on every per-ref call, and forgotten
// at its teardown.
func TestVMForRemembersAContainerUntilItsTeardown(t *testing.T) {
	f := newFakeDocker()
	d := newTestDriver(f)
	if err := d.EnableVM(&VMConfig{StateDir: "/tmp/vmx", QEMUImage: "q", CPUModel: "Haswell-v4", MachineType: "pc-q35-10.0", UID: 1000, GID: 1000, KVMGID: "1"}); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	seedSandbox(f, id)
	ref := agentContainerName(id)
	for range 3 {
		if v := d.vmFor(context.Background(), ref); v != nil {
			t.Fatal("a container was taken for a VM")
		}
	}
	f.mu.Lock()
	calls := f.inspectCalls[ref]
	f.mu.Unlock()
	if calls != 1 {
		t.Fatalf("%d inspects of a container's ref, want 1", calls)
	}
	if err := d.KillSandbox(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.notVMs.Load(ref); ok {
		t.Fatal("a torn-down ref is still remembered as a container")
	}
}

// A VM's terminal is its own shell: a watcher would get a shell, not a view of the run's terminal,
// so a watching attach is refused.
func TestVMAttachRefusesAWatcher(t *testing.T) {
	r := newVMRig(t)
	sb, err := r.d.CreateSandbox(context.Background(), vmSpec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.d.Attach(context.Background(), sb.Ref, runner.AttachOptions{Observer: true}); !errors.Is(err, errVMUnsupported) || !errors.Is(err, runner.ErrObserveUnsupported) {
		t.Fatalf("watching attach = %v, want refused", err)
	}
}

// One sidecar that will not go does not keep the others: each is tried, and every failure is told.
func TestVMSidecarRemovalContinuesPastAFailure(t *testing.T) {
	r := newVMRig(t)
	spec := vmSpec()
	spec.Mounts = []runner.Mount{{Source: t.TempDir(), Target: "/home/agent/work"}, {Source: t.TempDir(), Target: "/workspace/ref", ReadOnly: true}}
	if _, err := r.d.CreateSandbox(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	r.f.mu.Lock()
	r.f.failRemove = map[string]int{vmfsName(spec.RunID, 0): 1}
	r.f.mu.Unlock()
	err := r.d.removeVMSidecars(context.Background(), spec.RunID)
	if err == nil || !strings.Contains(err.Error(), vmfsName(spec.RunID, 0)) {
		t.Fatalf("removal = %v, want the first share's failure", err)
	}
	if !r.container(t, vmfsName(spec.RunID, 1)).removed || !r.container(t, vmPrepName(spec.RunID, "overlay")).removed {
		t.Fatal("a failed share removal stopped the rest")
	}
}
