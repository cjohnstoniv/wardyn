// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && vm

// Package vm is the Linux VM sandbox conformance suite: the real Docker driver boots the reference
// guest image under QEMU/KVM in its unprivileged container. It builds nothing: set
// WARDYN_TEST_VM_QEMU_IMAGE (deploy/runner/vm-qemu) and WARDYN_TEST_VM_GUEST_IMAGE
// (deploy/images/vm-linux), on a host with /dev/kvm and a local Docker engine.
package vm

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/types"
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

// netStub stands where the network relay will: it accepts QEMU's stream link, reads and counts
// the guest's Ethernet frames (4-byte big-endian length, then the frame) and answers none.
type netStub struct {
	ln     net.Listener
	frames atomic.Int64
}

func startNetStub(t *testing.T, path string) *netStub {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("net stub: %v", err)
	}
	s := &netStub{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				var hdr [4]byte
				for {
					if _, err := io.ReadFull(conn, hdr[:]); err != nil {
						return
					}
					if _, err := io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint32(hdr[:]))); err != nil {
						return
					}
					s.frames.Add(1)
				}
			}()
		}
	}()
	return s
}

func run(t *testing.T, d *docker.Driver, ref, script string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	es, err := d.ExecStream(ctx, ref, runner.ExecSpec{Argv: []string{"sh", "-c", script}})
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
	qemuImage, guestImage := os.Getenv("WARDYN_TEST_VM_QEMU_IMAGE"), os.Getenv("WARDYN_TEST_VM_GUEST_IMAGE")
	if qemuImage == "" || guestImage == "" {
		t.Skip("WARDYN_TEST_VM_QEMU_IMAGE and WARDYN_TEST_VM_GUEST_IMAGE not set")
	}
	ctx := context.Background()
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err := cli.NetworkCreate(ctx, "wardyn-internal", dockerclient.NetworkCreateOptions{Driver: "bridge"}); err == nil {
		t.Cleanup(func() {
			_, _ = cli.NetworkRemove(context.Background(), "wardyn-internal", dockerclient.NetworkRemoveOptions{})
		})
	}
	d, err := docker.New(docker.Config{ProxyImage: "busybox:latest", ProxyCmd: []string{"sleep", "infinity"}})
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
	if !strings.HasPrefix(vc.MachineType, "pc-q35-") || vc.QEMUVersion == "" {
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
	out := &lockedBuffer{}
	spec := runner.SandboxSpec{
		RunID: uuid.New(), Image: guestImage, MachineKind: types.MachineLinuxVM, ConfinementClass: types.CC1,
		Env:         map[string]string{"WARDYN_CONFORMANCE": "vm"},
		ProxyConfig: runner.ProxyConfig{RunToken: "tok", ControlPlaneURL: "http://127.0.0.1:8080"},
		Resources:   runner.Resources{CPUMillis: 2000, MemoryMiB: 2048},
		Mounts:      []runner.Mount{{Source: ws, Target: "/home/agent/work", ReadOnly: true}},
		ExecOutput:  out,
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
	stub := startNetStub(t, filepath.Join(dir, "net", qemu.NetSocket))

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

	// The network: QEMU's link reaches the stub, the guest's frames go nowhere, nothing resolves.
	// The guest's boot-time frames went out before the stub listened, so a connection attempt to a
	// link-local neighbour makes it send more (neighbour solicitations); none is answered.
	if code, o := run(t, d, sb.Ref, `for i in $(ls /sys/class/net); do if [ "$i" != lo ] && timeout 5 bash -c "</dev/tcp/fe80::1%$i/80"; then exit 0; fi; done; exit 7`); code != 7 {
		t.Fatalf("a link-local connection from the guest = %d %q", code, o)
	}
	for deadline := time.Now().Add(30 * time.Second); stub.frames.Load() == 0; time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatal("no guest frame reached net.sock")
		}
	}
	t.Logf("the net stub read %d guest frames and answered none", stub.frames.Load())
	if code, o := run(t, d, sb.Ref, "getent ahosts example.com || exit 7; exit 0"); code != 7 {
		t.Fatalf("name resolution in the guest = %d %q", code, o)
	}
	if code, o := run(t, d, sb.Ref, `timeout 10 bash -c '</dev/tcp/1.1.1.1/80' && exit 0; exit 7`); code != 7 {
		t.Fatalf("a direct connection from the guest = %d %q", code, o)
	}
	// The CA is not readable by the agent user (fw_cfg is root's), and the guest trusts no run CA here.
	if code, _ := run(t, d, sb.Ref, "cat /sys/firmware/qemu_fw_cfg/by_name/opt/org.wardyn/run-id/raw"); code == 0 {
		t.Fatal("the agent user read fw_cfg")
	}

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
