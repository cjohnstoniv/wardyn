// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// makeBase writes a ready base by hand.
func makeBase(t *testing.T, l macvm.Layout) string {
	t.Helper()
	base := l.BaseDir(0)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{macvm.DiskFile: "base-disk", macvm.AuxFile: "base-aux", macvm.MachineIDFile: "base-machine-id"} {
		if err := os.WriteFile(filepath.Join(base, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m := `{"format":1,"state":"ready","min_cpus":2,"min_memory_mib":4096,"hardware_model":"aHc=","desktop":"control","harnesses":["none"]}`
	if err := os.WriteFile(filepath.Join(base, macvm.ManifestFile), []byte(m), 0o600); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestCloneRunIdentityStrategies(t *testing.T) {
	l := macvm.Layout{Root: t.TempDir()}
	base := makeBase(t, l)
	if err := os.Chmod(base, 0o500); err != nil { // the clone never writes the base
		t.Fatal(err)
	}
	defer os.Chmod(base, 0o700)
	eng := &macvmtest.Engine{}
	ids := map[string]macvm.Identity{}
	for _, run := range []struct {
		id string
		s  macvm.IdentityStrategy
	}{{"t1", macvm.IdentityTravel}, {"t2", macvm.IdentityTravel}, {"f1", macvm.IdentityFresh}, {"f2", macvm.IdentityFresh}} {
		id, err := macvm.CloneRun(eng, base, l.RunDir(run.id), run.s)
		if err != nil {
			t.Fatal(err)
		}
		ids[run.id] = id
		got, _ := os.ReadFile(filepath.Join(l.RunDir(run.id), macvm.MachineIDFile))
		if !bytes.Equal(got, id.MachineIdentifier) {
			t.Fatalf("%s: machine-id file %q, configured %q", run.id, got, id.MachineIdentifier)
		}
		if d, _ := os.ReadFile(filepath.Join(l.RunDir(run.id), macvm.DiskFile)); string(d) != "base-disk" {
			t.Fatalf("%s: clone disk %q", run.id, d)
		}
	}
	if string(ids["t1"].MachineIdentifier) != "base-machine-id" || macvm.IDHash(ids["t1"].MachineIdentifier) != macvm.IDHash(ids["t2"].MachineIdentifier) {
		t.Fatal("travel: the base's identifier should travel with every clone")
	}
	if bytes.Equal(ids["f1"].MachineIdentifier, ids["f2"].MachineIdentifier) || string(ids["f1"].MachineIdentifier) == "base-machine-id" {
		t.Fatal("fresh: each run needs its own identifier")
	}
	if _, err := macvm.CloneRun(eng, base, l.RunDir("t1"), macvm.IdentityFresh); err == nil {
		t.Fatal("cloned twice into one run dir")
	}
	if _, err := macvm.CloneRun(eng, base, l.RunDir("x"), "other"); err == nil {
		t.Fatal("accepted an unknown strategy")
	}
	if macvm.HostCapFor(macvm.IdentityFresh, 1) != 2 || macvm.HostCapFor(macvm.IdentityTravel, 1) != 1 || macvm.HostCapFor(macvm.IdentityTravel, 2) != 2 {
		t.Fatal("HostCapFor")
	}
}

// testRunID is the run id startRun uses; its run key is 6f1c2a3b4d5e.
const testRunID = "6f1c2a3b-4d5e-4f60-8a71-9b0c1d2e3f40"

// runRig runs one helper on the fake engine.
type runRig struct {
	eng    *macvmtest.Engine
	layout macvm.Layout
	dir    string
	sock   string // the run's socket directory
	done   chan error
	cancel context.CancelFunc
	cc     *macvm.ControlClient
	relay  net.Conn // the vmnet container's side of the attach stream
}

func startRun(t *testing.T, eng *macvmtest.Engine, mod func(*macvm.RunConfig, *macvm.RunOptions)) *runRig {
	t.Helper()
	r := &runRig{eng: eng, layout: macvm.Layout{Root: t.TempDir()}, done: make(chan error, 1)}
	base := makeBase(t, r.layout)
	cfg := macvm.RunConfig{RunID: testRunID, BaseDir: base, CPUs: 4, MemoryMiB: 8192, Identity: macvm.IdentityFresh,
		SockRoot: macvmtest.SockRoot(t)}
	listening := make(chan struct{})
	r.dir = r.layout.RunDir(cfg.RunID)
	o := macvm.RunOptions{Engine: eng, RunDir: r.dir, NetWait: 5 * time.Second, Listening: func() { close(listening) }, SockFstat: macvmtest.SockFstat}
	if mod != nil {
		mod(&cfg, &o)
	}
	r.sock = macvm.SockDir(cfg.SockRoot, cfg.RunID)
	if err := macvm.WriteRunConfig(r.dir, cfg); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	t.Cleanup(cancel)
	go func() { r.done <- macvm.Run(ctx, o) }()
	select {
	case <-listening:
	case err := <-r.done:
		r.done <- err
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("ctl.sock never listened")
	}
	cc, err := macvm.DialControl(ctx, filepath.Join(r.sock, macvm.ControlSock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	r.cc = cc
	return r
}

// attach sends one end of a stream socket pair as the network descriptor.
func (r *runRig) attach(t *testing.T) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	helperSide := os.NewFile(uintptr(fds[0]), "attach")
	relayFile := os.NewFile(uintptr(fds[1]), "relay")
	defer helperSide.Close()
	c, err := net.FileConn(relayFile)
	_ = relayFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	r.relay = c
	if err := r.cc.SendNetFD(context.Background(), helperSide); err != nil {
		t.Fatal(err)
	}
}

func waitState(t *testing.T, cc *macvm.ControlClient, want macvm.MachineState) macvm.Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := cc.Status(context.Background())
		if err == nil && st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %q, want %q (%v)", st.State, want, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunLifecycle(t *testing.T) {
	eng := &macvmtest.Engine{}
	r := startRun(t, eng, nil)
	st, err := r.cc.Status(context.Background())
	if err != nil || st.State != macvm.StateStopped || st.Network != macvm.NetWaiting || st.Proto != macvm.ControlProto {
		t.Fatalf("before net_fd: %+v %v", st, err)
	}
	if len(eng.Machines()) != 1 || eng.Machines()[0].State() != macvm.StateStopped {
		t.Fatal("the VM started before its network was attached")
	}
	evc, err := macvm.DialControl(context.Background(), filepath.Join(r.sock, macvm.ControlSock))
	if err != nil {
		t.Fatal(err)
	}
	defer evc.Close()
	evs, err := evc.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.attach(t)
	st = waitState(t, r.cc, macvm.StateRunning)
	m := eng.Machines()[0]
	if st.Identity != macvm.IdentityFresh || st.MachineIDSHA256 != macvm.IDHash(m.Plan.MachineIdentifier) ||
		st.ReadBackSHA256 != st.MachineIDSHA256 || st.Network != macvm.NetAttached || st.CPUs != 4 || st.MemoryMiB != 8192 ||
		st.Display != (macvm.Display{Width: 1920, Height: 1080, PPI: 80}) {
		t.Fatalf("running status %+v", st)
	}
	if m.Plan.CPUs != 4 || m.Plan.MemoryBytes != 8<<30 {
		t.Fatalf("VM size %d/%d", m.Plan.CPUs, m.Plan.MemoryBytes)
	}

	// Guest NIC -> relay over the bridge.
	frame := bytes.Repeat([]byte{0x42}, 64)
	if _, err := m.Plan.Network[0].File.Write(frame); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4+len(frame))
	_ = r.relay.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(r.relay, got); err != nil || binary.BigEndian.Uint32(got) != 64 || !bytes.Equal(got[4:], frame) {
		t.Fatalf("relay read %x %v", got[:4], err)
	}

	// agent.sock pairs with the guest's idle vsock connection.
	guest, err := m.DialGuest(macvm.AgentPort)
	if err != nil {
		t.Fatal(err)
	}
	host, err := net.Dial("unix", filepath.Join(r.sock, macvm.AgentSock))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = host.Write([]byte("hello guest\n")) }()
	line, err := bufio.NewReader(guest).ReadString('\n')
	if err != nil || line != "hello guest\n" {
		t.Fatalf("guest got %q %v", line, err)
	}
	_ = host.Close()
	if fi, err := os.Stat(filepath.Join(r.sock, macvm.ControlSock)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("ctl.sock mode %v %v", fi, err)
	}
	if m.Listening(macvm.DesktopPort) {
		t.Fatal("desktop served without a shim")
	}

	if err := r.cc.Stop(context.Background(), true, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := <-r.done; err != nil {
		t.Fatalf("Run after a requested stop: %v", err)
	}
	var sawRunning, sawStopped, sawGuest bool
	for ev := range evs {
		sawRunning = sawRunning || ev.State == macvm.StateRunning
		sawStopped = sawStopped || ev.State == macvm.StateStopped
		sawGuest = sawGuest || ev.Type == macvm.EventGuestAgent
	}
	if !sawRunning || !sawStopped || !sawGuest {
		t.Fatalf("events: running %v stopped %v guest %v", sawRunning, sawStopped, sawGuest)
	}
}

// The configured and read-back identifier hashes are both reported,
// so a framework that replaced the identifier would show.
func TestRunReportsReadBackIdentifier(t *testing.T) {
	eng := &macvmtest.Engine{}
	r := startRun(t, eng, func(c *macvm.RunConfig, _ *macvm.RunOptions) { c.Identity = macvm.IdentityTravel })
	eng.Machines()[0].SetReadBackID([]byte("replaced-by-framework"))
	r.attach(t)
	st := waitState(t, r.cc, macvm.StateRunning)
	if st.MachineIDSHA256 != macvm.IDHash([]byte("base-machine-id")) || st.ReadBackSHA256 != macvm.IDHash([]byte("replaced-by-framework")) {
		t.Fatalf("status %+v", st)
	}
	r.cancel()
	<-r.done
}

func TestRunNetworkLost(t *testing.T) {
	eng := &macvmtest.Engine{}
	r := startRun(t, eng, nil)
	r.attach(t)
	waitState(t, r.cc, macvm.StateRunning)
	_ = r.relay.Close()
	select {
	case err := <-r.done:
		if !errors.Is(err, macvm.ErrNetworkLost) {
			t.Fatalf("got %v, want ErrNetworkLost", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not end when its network dropped")
	}
	if eng.Machines()[0].State() != macvm.StateStopped {
		t.Fatal("VM left running")
	}
}

func TestRunGracefulStopFallsBackToHard(t *testing.T) {
	eng := &macvmtest.Engine{IgnoreRequestStop: true}
	r := startRun(t, eng, nil)
	r.attach(t)
	waitState(t, r.cc, macvm.StateRunning)
	start := time.Now()
	if err := r.cc.Stop(context.Background(), true, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second || eng.Machines()[0].State() != macvm.StateStopped {
		t.Fatal("hard stop did not follow the graceful timeout")
	}
	<-r.done
}

func TestRunStopBeforeNetwork(t *testing.T) {
	eng := &macvmtest.Engine{}
	r := startRun(t, eng, nil)
	if err := r.cc.Stop(context.Background(), true, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := <-r.done; err != nil {
		t.Fatal(err)
	}
	if eng.Machines()[0].State() != macvm.StateStopped {
		t.Fatal("VM started")
	}
}

func TestRunRefusals(t *testing.T) {
	hot := &macvmtest.Engine{Thermal: macvm.ThermalCritical}
	r := startRun(t, hot, nil)
	if err := <-r.done; !errors.Is(err, macvm.ErrThermal) {
		t.Fatalf("hot Mac: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, macvm.DiskFile)); !os.IsNotExist(err) {
		t.Fatal("cloned on a hot Mac")
	}
	small := startRun(t, &macvmtest.Engine{}, func(c *macvm.RunConfig, _ *macvm.RunOptions) { c.MemoryMiB = 2048 })
	if err := <-small.done; err == nil {
		t.Fatal("ran below the base's minimum memory")
	}
	unclaimed := &macvmtest.Engine{}
	claimed := startRun(t, unclaimed, func(_ *macvm.RunConfig, o *macvm.RunOptions) {
		o.Claim = func(macvm.RunConfig) error { return errors.New("tombstoned") }
	})
	if err := <-claimed.done; err == nil || !strings.Contains(err.Error(), "tombstoned") || len(unclaimed.Machines()) != 0 {
		t.Fatalf("built a VM without its admission claim: %v", err)
	}
}

func TestDialControlProtoMismatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ctl.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = bufio.NewReader(c).ReadString('\n')
		_, _ = c.Write([]byte(`{"ok":true,"proto":99}` + "\n"))
	}()
	_, err = macvm.DialControl(context.Background(), p)
	var pe *macvm.ErrProto
	if !errors.As(err, &pe) || pe.Helper != 99 {
		t.Fatalf("got %v, want ErrProto{99}", err)
	}
}
