// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vm/qemu"
)

// standaloneDiskInfo is qemu-img info's answer for a qcow2 with no backing or data file.
const standaloneDiskInfo = `{"virtual-size": 68719476736, "format": "qcow2", "format-specific": {"type": "qcow2", "data": {"compat": "1.1"}}}`

// winRig is a VM rig with Windows VMs on, whose fake engine plays the Windows prep steps (the
// converter's info and convert, the overlay), the swtpm sidecar's socket and QEMU's QMP and VNC sockets.
type winRig struct {
	*vmRig
	info     string // what the converter's info step writes as info.json
	vnc      net.Listener
	converts atomic.Int32 // the converter's convert steps started
}

func newWinRig(t *testing.T) *winRig {
	t.Helper()
	r := &winRig{vmRig: newVMRig(t), info: standaloneDiskInfo}
	if err := r.d.EnableWindowsVM(&WinVMConfig{SWTPMVersion: "test"}); err != nil {
		t.Fatal(err)
	}
	r.f.onStart = func(c *createdContainer) {
		switch {
		case len(c.cfg.Cmd) > 1 && c.cfg.Cmd[0] == "wardyn-winvm-disk":
			out := mountSource(c, "/out")
			if c.cfg.Cmd[1] == "info" {
				_ = os.WriteFile(filepath.Join(out, "src.qcow2"), []byte("qcow2"), 0o600)
				_ = os.WriteFile(filepath.Join(out, "info.json"), []byte(r.info), 0o600)
			} else {
				r.converts.Add(1)
				_ = os.WriteFile(filepath.Join(out, qemu.WinDisk), []byte("qcow2"), 0o600)
			}
		case strings.HasSuffix(c.name, "-overlay") && c.cfg.Cmd[0] == "sh":
			disk := mountSource(c, qemu.DiskDir)
			_ = os.WriteFile(filepath.Join(disk, qemu.Overlay), nil, 0o600)
			_ = os.WriteFile(filepath.Join(disk, qemu.WinVars), nil, 0o600)
		case c.name == vmtpmName(winSpec().RunID):
			r.listen(t, filepath.Join(mountSource(c, qemu.CtlDir), qemu.TPMSocket))
		case strings.HasPrefix(c.name, agentNamePrefix) && c.cfg.Labels[labelMachineKind] == "windows_vm":
			ctl := mountSource(c, qemu.CtlDir)
			r.serveLines(t, filepath.Join(ctl, qemu.QMPSocket), true)
			r.vnc = r.listen(t, filepath.Join(ctl, qemu.VNCSocket))
		default:
			r.start(t, c)
		}
	}
	return r
}

func winSpec() runner.SandboxSpec {
	s := testSpec()
	s.MachineKind = types.MachineWindowsVM
	s.Image = vmTestImage
	s.Interactive = true
	s.Resources = runner.Resources{CPUMillis: 4000, MemoryMiB: 8192}
	return s
}

func TestCreateWinVMHostConfig(t *testing.T) {
	r := newWinRig(t)
	spec := winSpec()
	ctx := context.Background()
	sb, err := r.d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Ref != agentContainerName(spec.RunID) || sb.EnforcedClass != types.CC3 || sb.Machine == nil || sb.Machine.Accelerator != "kvm" {
		t.Fatalf("sandbox %+v: the VM is the run's agent ref, a hardware boundary, on KVM", sb)
	}
	q := r.container(t, sb.Ref)
	tpm := r.container(t, vmtpmName(spec.RunID))
	user := r.vc.user()
	for name, c := range map[string]*createdContainer{"qemu": q, "swtpm": tpm, "overlay": r.container(t, vmPrepName(spec.RunID, "overlay")),
		"image": r.container(t, vmPrepName(spec.RunID, "image"))} {
		h := c.host
		if c.cfg.User != user || c.cfg.User == "0" || strings.HasPrefix(c.cfg.User, "0:") {
			t.Errorf("%s runs as %q, not the runner's own uid", name, c.cfg.User)
		}
		if h.Privileged || !slices.Equal(h.CapDrop, []string{"ALL"}) || !slices.Contains(h.SecurityOpt, "no-new-privileges") ||
			string(h.NetworkMode) != "none" || !h.ReadonlyRootfs || h.PidsLimit == nil || h.Memory == 0 {
			t.Errorf("%s host config not hardened: %+v", name, h)
		}
		if name != "qemu" && (len(h.Devices) != 0 || len(h.GroupAdd) != 0) {
			t.Errorf("%s got a device or group: only QEMU opens /dev/kvm", name)
		}
		if c.cfg.Labels[labelRun] != spec.RunID.String() {
			t.Errorf("%s carries no run label: the teardown and the sweep cannot find it", name)
		}
	}
	if !slices.Equal([]string(tpm.cfg.Cmd), qemu.SWTPMArgs()) {
		t.Fatalf("swtpm runs %q, want %q", tpm.cfg.Cmd, qemu.SWTPMArgs())
	}
	if len(tpm.host.Mounts) != 1 || tpm.host.Mounts[0].Target != qemu.CtlDir || tpm.cfg.Labels[labelComponent] != componentVMTPM {
		t.Fatalf("swtpm mounts %+v labels %v: the run's ctl/ only, labelled for the sweep", tpm.host.Mounts, tpm.cfg.Labels)
	}
	if fi, err := os.Stat(filepath.Join(r.vc.runDir(spec.RunID), "ctl", qemu.TPMStateDir)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the TPM state directory: %v", err)
	}
	if len(q.host.Devices) != 1 || q.host.Devices[0].PathOnHost != "/dev/kvm" || !slices.Equal(q.host.GroupAdd, []string{"992"}) {
		t.Fatalf("QEMU devices %+v groups %v: /dev/kvm and its group, nothing else", q.host.Devices, q.host.GroupAdd)
	}
	if q.host.Memory != 8192<<20 || q.cfg.Labels[labelMachineKind] != "windows_vm" {
		t.Fatalf("QEMU memory %d labels %v", q.host.Memory, q.cfg.Labels)
	}
	ro := map[string]bool{}
	for _, m := range q.host.Mounts {
		ro[m.Target] = m.ReadOnly
	}
	if len(ro) != 4 || !ro[qemu.BaseDir] || ro[qemu.CtlDir] || ro[qemu.DiskDir] || ro[qemu.NetDir] {
		t.Fatalf("QEMU mounts %v: the base read-only, ctl/net/disk, and no share or fw_cfg", ro)
	}
	want, err := qemu.WinArgs(qemu.WinConfig{RunID: spec.RunID, MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", VCPUs: 4, MemoryMiB: 8192 - 8192/5})
	if err != nil || !slices.Equal(q.cfg.Cmd, want) {
		t.Fatalf("QEMU argv %q, want WinArgs %q (%v)", q.cfg.Cmd, want, err)
	}
	r.mu.Lock()
	qmp := slices.Clone(r.qmp)
	r.mu.Unlock()
	if !slices.Contains(qmp, "query-kvm") {
		t.Fatalf("QMP %v: the VM is checked to run under KVM", qmp)
	}
	if v := r.d.vmFor(ctx, sb.Ref); v == nil || v.win == nil {
		t.Fatal("the created Windows VM is not the driver's Windows vmRun")
	}
	if err := r.d.KillSandbox(ctx, sb.Ref); err != nil {
		t.Fatal(err)
	}
	if c := r.container(t, vmtpmName(spec.RunID)); !c.removed {
		t.Fatal("the swtpm sidecar outlived the run")
	}
}

// What a Windows VM cannot honour is refused before anything is created.
func TestRefuseWinVMSpec(t *testing.T) {
	cases := map[string]struct {
		edit func(*runner.SandboxSpec)
		want string
	}{
		"mount":        {func(s *runner.SandboxSpec) { s.Mounts = []runner.Mount{{Source: "/tmp", Target: "/w"}} }, "no folder"},
		"drive":        {func(s *runner.SandboxSpec) { s.Drives = []types.DriveMount{{Backend: types.DriveBackendHostPath}} }, "no drive"},
		"record":       {func(*runner.SandboxSpec) {}, "never recorded"},
		"secret env":   {func(s *runner.SandboxSpec) { s.SecretEnv = map[string]string{"TOKEN": "x"} }, "no secret env"},
		"managed file": {func(s *runner.SandboxSpec) { s.ManagedFiles = []runner.ManagedFile{{Path: "/etc/x"}} }, "no managed file"},
		"record asked": {func(s *runner.SandboxSpec) { s.Record = ptr(true) }, "never recorded"},
		"task":         {func(s *runner.SandboxSpec) { s.Interactive = false }, "desktop-only"},
		"low memory":   {func(s *runner.SandboxSpec) { s.Resources.MemoryMiB = 5118 }, "cap of at least 5119 MiB"},
		"4096 cap":     {func(s *runner.SandboxSpec) { s.Resources.MemoryMiB = 4096 }, "cap of at least 5119 MiB"},
		"no memory":    {func(s *runner.SandboxSpec) { s.Resources.MemoryMiB = 0 }, "cap of at least 5119 MiB"},
		"hard disk":    {func(s *runner.SandboxSpec) { s.Resources.DiskMiB = 1024 }, "disk limit"},
		"linux spec":   {func(s *runner.SandboxSpec) { s.MachineKind = types.MachineLinuxVM }, "unknown machine kind"},
		"windows off":  {nil, "does not offer Windows VM sandboxes"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newWinRig(t)
			r.d.cfg.Record = name == "record"
			spec := winSpec()
			if tc.edit != nil {
				tc.edit(&spec)
			} else if err := r.d.EnableWindowsVM(nil); err != nil {
				t.Fatal(err)
			}
			err := r.d.refuseWinVMSpec(spec)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refuseWinVMSpec = %v, want a refusal naming %q", err, tc.want)
			}
			if tc.edit == nil || spec.MachineKind != types.MachineWindowsVM {
				return
			}
			if _, err := r.d.CreateSandbox(context.Background(), spec); err == nil {
				t.Fatal("CreateSandbox started a refused Windows VM")
			}
			r.f.mu.Lock()
			n := len(r.f.containers)
			r.f.mu.Unlock()
			if n != 0 {
				t.Fatalf("a refused Windows VM created %d containers", n)
			}
		})
	}
	// 5119 MiB is the smallest cap whose guest share is 4096 MiB; one MiB less is refused above.
	if g, _, err := qemu.Memory(5119); err != nil || g != 4096 || winVMMinCapMiB() != 5119 {
		t.Fatalf("qemu.Memory(5119) = %d, %v; winVMMinCapMiB = %d; want a 4096 MiB guest at 5119", g, err, winVMMinCapMiB())
	}
	edge := winSpec()
	edge.Resources.MemoryMiB = 5119
	if err := newWinRig(t).d.refuseWinVMSpec(edge); err != nil {
		t.Fatalf("refuseWinVMSpec refused a 5119 MiB cap (4096 MiB guest): %v", err)
	}
	soft := winSpec()
	soft.Resources.DiskMiB, soft.Resources.DiskMiBSoft = 1024, true
	if err := newWinRig(t).d.refuseWinVMSpec(soft); err != nil {
		t.Fatalf("refuseWinVMSpec refused a soft disk size: %v", err)
	}
	// A disk that names a backing file is refused before it is converted or booted.
	r := newWinRig(t)
	r.info = `{"virtual-size": 1073741824, "format": "qcow2", "backing-filename": "/etc/shadow"}`
	if _, err := r.d.CreateSandbox(context.Background(), winSpec()); err == nil || !strings.Contains(err.Error(), "backing file") {
		t.Fatalf("a backing-file disk = %v, want a refusal", err)
	}
	if _, ok := r.f.containers[agentContainerName(winSpec().RunID)]; ok {
		t.Fatal("a refused disk reached QEMU")
	}
	if n := r.converts.Load(); n != 0 {
		t.Fatalf("a refused disk was converted %d times", n)
	}
}

// A create that fails after the swtpm sidecar started removes it.
func TestWinVMFailedCreateRemovesTheTPM(t *testing.T) {
	r := newWinRig(t)
	spec := winSpec()
	r.f.failStartContainer = agentContainerName(spec.RunID)
	if _, err := r.d.CreateSandbox(context.Background(), spec); err == nil {
		t.Fatal("CreateSandbox succeeded with QEMU failing to start")
	}
	if c := r.container(t, vmtpmName(spec.RunID)); !c.removed {
		t.Fatal("the swtpm sidecar outlived a failed create")
	}
}

// The orphan sweep finds a Windows VM's swtpm sidecar by its component label and removes it.
func TestWinVMSweepRemovesAnOrphanedTPM(t *testing.T) {
	r := newWinRig(t)
	rid := uuid.NewString()
	r.d.cfg.RunnerID, r.d.cfg.DecisionSink, r.d.cfg.Evidence = rid, newFakeSink(), &memEvidence{m: map[uuid.UUID][]byte{}}
	spec := winSpec()
	spec.Labels = map[string]string{runner.LabelRunnerID: rid}
	sb, err := r.d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	// The runner crashed: the swtpm sidecar is still there, the QEMU container is not.
	past := time.Now().Add(-time.Hour)
	name := vmtpmName(spec.RunID)
	r.f.mu.Lock()
	tpm := r.f.containers[name]
	tpm.removed = false
	r.f.containers[sb.Ref].removed = true
	r.f.listItems = []container.Summary{{ID: name, Created: past.Unix(), Labels: tpm.cfg.Labels}}
	r.f.mu.Unlock()
	if n, err := r.d.SweepOrphanedSandboxes(context.Background(), 10*time.Minute, func(uuid.UUID) bool { return true }); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want the one orphaned run", n, err)
	}
	r.f.mu.Lock()
	defer r.f.mu.Unlock()
	if !r.f.containers[name].removed {
		t.Fatal("the sweep left the orphaned swtpm sidecar")
	}
}

func TestWinDesktopNewestClosesOld(t *testing.T) {
	r := newWinRig(t)
	ctx := context.Background()
	sb, err := r.d.CreateSandbox(ctx, winSpec())
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := r.vnc.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c) }() // an echoing display
		}
	}()
	first, err := r.d.DialDesktop(ctx, sb.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	second, err := r.d.DialDesktop(ctx, sb.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = first.(net.Conn).SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 8)
	for {
		if _, err := first.Read(buf); err != nil {
			if !errors.Is(err, net.ErrClosed) {
				t.Fatalf("the replaced stream read %v, want it closed", err)
			}
			break
		}
	}
	if _, err := second.Write([]byte("b")); err != nil {
		t.Fatal(err)
	}
	if n, err := io.ReadFull(second, buf[:1]); err != nil || n != 1 || buf[0] != 'b' {
		t.Fatalf("the newest stream read %q, %v", buf[:n], err)
	}
	// A restarted driver finds the VM by its label and keeps the same rule.
	d2 := newTestDriver(r.f)
	vc := *r.vc
	vc.Windows = &WinVMConfig{}
	if err := d2.EnableVM(&vc); err != nil {
		t.Fatal(err)
	}
	third, err := d2.DialDesktop(ctx, sb.Ref)
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := d2.DialDesktop(ctx, sb.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer fourth.Close()
	if _, err := third.Write([]byte("c")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("after a restart the replaced stream wrote %v, want it closed", err)
	}
}

func TestCoversKindWindowsVM(t *testing.T) {
	r := newWinRig(t)
	if !r.d.CoversKind(types.MachineWindowsVM) {
		t.Fatal("CoversKind(windows_vm) = false with Windows VMs on")
	}
	if err := r.d.EnableWindowsVM(nil); err != nil {
		t.Fatal(err)
	}
	if r.d.CoversKind(types.MachineWindowsVM) {
		t.Fatal("CoversKind(windows_vm) = true with Windows VMs off")
	}
	for _, kind := range []types.MachineKind{"", types.MachineLinuxVM} {
		if !r.d.CoversKind(kind) {
			t.Errorf("CoversKind(%q) = false with Windows VMs off", kind)
		}
	}
	if err := r.d.EnableVM(nil); err != nil {
		t.Fatal(err)
	}
	if r.d.CoversKind(types.MachineWindowsVM) || r.d.EnableWindowsVM(&WinVMConfig{}) == nil {
		t.Fatal("Windows VMs went on without Linux VMs")
	}
}

// An exited or dead Windows QEMU container is gone, with no exit code: QEMU's exit code is never an
// agent's, and the observer then reports the sandbox absent, which ends the run and frees its seat.
func TestWinVMExitedContainerReadsGone(t *testing.T) {
	r := newWinRig(t)
	ctx := context.Background()
	sb, err := r.d.CreateSandbox(ctx, winSpec())
	if err != nil {
		t.Fatal(err)
	}
	set := func(ref string, st *container.State) {
		r.f.mu.Lock()
		r.f.containers[ref].state = st
		r.f.mu.Unlock()
	}
	for name, st := range map[string]*container.State{
		"exited 0":   {Status: "exited", ExitCode: 0},
		"exited 1":   {Status: "exited", ExitCode: 1},
		"dead":       {Status: "dead", ExitCode: 137},
		"OOM killed": {Status: "exited", ExitCode: 137, OOMKilled: true},
	} {
		set(sb.Ref, st)
		for verb, get := range map[string]func() (runner.Status, error){
			"Status":      func() (runner.Status, error) { return r.d.Status(ctx, sb.Ref) },
			"AgentStatus": func() (runner.Status, error) { return r.d.AgentStatus(ctx, sb.Ref, "") },
		} {
			got, err := get()
			if err != nil || got.State != types.RunStopped || got.ExitCode != nil {
				t.Errorf("%s: %s = %+v, %v: want STOPPED with no exit code", name, verb, got, err)
			}
		}
	}
	set(sb.Ref, &container.State{Status: "running", Running: true})
	if got, _ := r.d.Status(ctx, sb.Ref); got.State != types.RunRunning {
		t.Errorf("running: %+v", got)
	}
	set(sb.Ref, &container.State{Status: "created"})
	if got, _ := r.d.Status(ctx, sb.Ref); got.State != types.RunStarting {
		t.Errorf("created: %+v", got)
	}
	// A Linux VM's exit still reads as before.
	lin := &container.InspectResponse{Config: &container.Config{Labels: map[string]string{labelMachineKind: "linux_vm"}},
		State: &container.State{Status: "exited", ExitCode: 1}}
	if got := statusFromInspect(*lin); got.State != types.RunFailed || got.ExitCode == nil || *got.ExitCode != 1 {
		t.Errorf("linux_vm exited 1 = %+v, want FAILED with exit code 1", got)
	}
}

// Every guest agent call on a Windows VM, made directly or through the driver's verbs, fails with
// errWinVMNoGuestAgent; none succeeds silently and none leaves a connection behind.
func TestWinVMAgentRefusesEveryCall(t *testing.T) {
	r := newWinRig(t)
	ctx := context.Background()
	sb, err := r.d.CreateSandbox(ctx, winSpec())
	if err != nil {
		t.Fatal(err)
	}
	d2 := newTestDriver(r.f)
	vc := *r.vc
	vc.Windows = &WinVMConfig{}
	if err := d2.EnableVM(&vc); err != nil {
		t.Fatal(err)
	}
	for how, d := range map[string]*Driver{"stored at create": r.d, "found by label": d2} {
		v := d.vmFor(ctx, sb.Ref)
		if v == nil || v.win == nil {
			t.Fatalf("%s: no Windows vmRun", how)
		}
		callEveryAgentMethod(t, how, v)
		v.agent.Close()
		if _, err := v.agent.Ready(ctx); !errors.Is(err, errWinVMNoGuestAgent) {
			t.Errorf("%s: a call after Close = %v", how, err)
		}
		short, cancel := context.WithTimeout(ctx, 2*time.Second)
		for verb, err := range map[string]error{
			"Exec":        second(d.Exec(short, sb.Ref, []string{"true"})),
			"Attach":      second(d.Attach(short, sb.Ref, runner.AttachOptions{})),
			"execStream":  second(d.execStream(short, sb.Ref, "", runner.ExecSpec{Argv: []string{"true"}})),
			"PauseAgent":  second(d.PauseAgent(short, sb.Ref, 1)),
			"ResumeAgent": second(d.ResumeAgent(short, sb.Ref, 1)),
			"AgentStatus": second(d.AgentStatus(short, sb.Ref, "exec-1")),
		} {
			if !errors.Is(err, errWinVMNoGuestAgent) {
				t.Errorf("%s: %s = %v, want errWinVMNoGuestAgent", how, verb, err)
			}
		}
		cancel()
	}
	// Wait probes through the same agent (Agent and State, refused above) and answers "the VM
	// stopped" once the QEMU container stops.
	r.f.mu.Lock()
	r.f.containers[sb.Ref].state = &container.State{Status: "exited"}
	r.f.mu.Unlock()
	if _, err := r.d.Wait(ctx, sb.Ref); err == nil || !strings.Contains(err.Error(), "the VM stopped") {
		t.Errorf("Wait on a stopped Windows VM = %v", err)
	}
}

func second[T any](_ T, err error) error { return err }

// callEveryAgentMethod calls every exported method of the vmRun's agent but Close, found by
// reflection so a method added later fails here until it is given arguments, with valid arguments,
// and wants errWinVMNoGuestAgent from each.
func callEveryAgentMethod(t *testing.T, how string, v *vmRun) {
	t.Helper()
	ctxT := reflect.TypeOf((*context.Context)(nil)).Elem()
	rv := reflect.ValueOf(v.agent)
	if rv.NumMethod() < 24 {
		t.Fatalf("%s: the agent has %d methods, want the guest agent client's 24 or more", how, rv.NumMethod())
	}
	for i := 0; i < rv.NumMethod(); i++ {
		m := rv.Type().Method(i)
		if m.Name == "Close" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		args := make([]reflect.Value, 0, m.Type.NumIn()-1)
		for j := 1; j < m.Type.NumIn(); j++ {
			in := m.Type.In(j)
			var a reflect.Value
			switch {
			case in == ctxT:
				a = reflect.ValueOf(ctx)
			case in == reflect.TypeOf(time.Duration(0)):
				a = reflect.ValueOf(10 * time.Millisecond)
			case in == reflect.TypeOf(fs.FileMode(0)):
				a = reflect.ValueOf(fs.FileMode(0o600))
			case in.Kind() == reflect.String:
				a = reflect.ValueOf("a1").Convert(in)
			case in.Kind() == reflect.Func && in.NumIn() == 0 && in.NumOut() == 1 && in.Out(0).Kind() == reflect.Bool:
				a = reflect.ValueOf(func() bool { return false })
			case in == reflect.TypeOf((*io.Reader)(nil)).Elem():
				a = reflect.ValueOf(bytes.NewReader([]byte("x")))
			case in == reflect.TypeOf([]string(nil)):
				a = reflect.ValueOf([]string{"true"})
			case in.Kind() == reflect.Int64:
				a = reflect.ValueOf(int64(1)).Convert(in)
			case in.Kind() == reflect.Struct:
				a = reflect.New(in).Elem()
			default:
				cancel()
				t.Fatalf("%s: %s takes a %s this test cannot make: give it a case", how, m.Name, in)
			}
			args = append(args, a)
		}
		out := rv.Method(i).Call(args)
		cancel()
		last := out[len(out)-1]
		var err error
		if !last.IsNil() {
			err, _ = last.Interface().(error)
		}
		if !errors.Is(err, errWinVMNoGuestAgent) {
			t.Errorf("%s: agent.%s = %v, want errWinVMNoGuestAgent", how, m.Name, err)
		}
	}
}
