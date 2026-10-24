// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"golang.org/x/sys/unix"
)

// fakeToolsAgent is the guest agent's tools mode on the wire: it records
// pushes (checking the SHA-256 the host sent against the body) and passwords.
type fakeToolsAgent struct {
	mu        sync.Mutex
	pushed    map[string][]byte
	passwords []string
}

func (a *fakeToolsAgent) tools() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tools/push", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != r.URL.Query().Get("sha256") {
			http.Error(w, "sha256 mismatch", http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		a.pushed[r.URL.Query().Get("dest")] = b
		a.mu.Unlock()
	})
	mux.HandleFunc("POST /v1/tools/exec", func(w http.ResponseWriter, r *http.Request) {
		var x guestagent.ToolsExec
		_ = json.NewDecoder(r.Body).Decode(&x)
		_ = json.NewEncoder(w).Encode(guestagent.ToolsExecResult{Output: strings.Join(x.Argv, " "), ExitCode: 3})
	})
	mux.HandleFunc("POST /v1/tools/desktop_password", func(w http.ResponseWriter, r *http.Request) {
		var x guestagent.DesktopPassword
		_ = json.NewDecoder(r.Body).Decode(&x)
		a.mu.Lock()
		a.passwords = append(a.passwords, x.Password)
		a.mu.Unlock()
		_ = json.NewEncoder(w).Encode(guestagent.DesktopPasswordResult{Set: true})
	})
	mux.HandleFunc("GET /v1/tools/pointer", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(guestagent.Pointer{X: 960, Y: 540})
	})
	return mux
}

func controlSession() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/dial/tools", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(guestagent.ToolsDial{Class: guestagent.DialConnected, Dials: 1})
	})
	return mux
}

// sendNetFD connects to the boot's ctl.sock once it listens and hands it one
// end of a socket pair, as runnerd does with the vmnet attach stream.
func sendNetFD(t *testing.T, sock string) *macvm.ControlClient {
	t.Helper()
	var cc *macvm.ControlClient
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		c, err := macvm.DialControl(context.Background(), sock)
		if err == nil {
			cc = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stage-2 ctl.sock never answered: %v", err)
		}
	}
	t.Cleanup(func() { _ = cc.Close() })
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	ours, theirs := os.NewFile(uintptr(fds[0]), "attach"), os.NewFile(uintptr(fds[1]), "relay")
	t.Cleanup(func() { _ = theirs.Close() })
	defer ours.Close()
	if err := cc.SendNetFD(context.Background(), ours); err != nil {
		t.Fatal(err)
	}
	return cc
}

// The stage-2 boot serves a ctl.sock in its job's socket dir, starts the VM
// only once runnerd's net_fd arrives, listens on 1024, 1025 and 1026, and
// reaches the guest agent's tools mode through the guestagent client: a push
// carries the SHA-256 of what it sends, even from a reader that cannot be
// read twice. Stopping it removes its socket dir.
func TestStage2BootTakesNetFDAndReachesToolsMode(t *testing.T) {
	eng := &macvmtest.Engine{}
	root := macvmtest.SockRoot(t)
	cfg := macvm.Stage2BootConfig{Engine: eng, SockRoot: root, SockKey: macvm.JobSockKey("0123456789abcdef01234567"), SockFstat: macvmtest.SockFstat, GuestWait: 10 * time.Second}
	dir := t.TempDir()
	for _, f := range []string{macvm.DiskFile, macvm.AuxFile} {
		_ = os.WriteFile(filepath.Join(dir, f), nil, 0o600)
	}
	spec := macvm.MachineSpec{CPUs: 4, MemoryBytes: 8 << 30, MinCPUs: 2, MinMemoryBytes: 4 << 30, HardwareModel: []byte("hw"),
		MachineIdentifier: []byte("id"), DiskPath: filepath.Join(dir, macvm.DiskFile), AuxPath: filepath.Join(dir, macvm.AuxFile)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type booted struct {
		g    macvm.ToolsGuest
		stop func(context.Context) error
		err  error
	}
	done := make(chan booted, 1)
	go func() {
		g, stop, err := cfg.ToolsBoot()(ctx, spec)
		done <- booted{g, stop, err}
	}()
	sockDir := macvm.SockDir(root, "0123456789ab")
	cc := sendNetFD(t, filepath.Join(sockDir, macvm.ControlSock))
	b := <-done
	if b.err != nil {
		t.Fatal(b.err)
	}
	m := eng.Machines()[0]
	if m.State() != macvm.StateRunning {
		t.Fatalf("the VM is %s", m.State())
	}
	st, err := cc.Status(context.Background())
	if err != nil || !slices.Equal(st.Listeners, []uint32{macvm.AgentPort, macvm.DesktopPort, macvm.ToolsPort}) || st.Network != macvm.NetAttached {
		t.Fatalf("status %+v %v", st, err)
	}
	agent := &fakeToolsAgent{pushed: map[string][]byte{}}
	go func() {
		_ = guestagent.ServeVsock(ctx, func(context.Context) (net.Conn, error) { return m.DialGuest(macvm.ToolsPort) }, agent.tools())
	}()
	go func() {
		_ = guestagent.ServeVsock(ctx, func(context.Context) (net.Conn, error) { return m.DialGuest(macvm.AgentPort) }, controlSession())
	}()
	tctx, tcancel := context.WithTimeout(ctx, 10*time.Second)
	defer tcancel()
	if out, exit, err := b.g.Run(tctx, []string{"/usr/bin/touch", "/tmp/x"}); err != nil || exit != 3 || out != "/usr/bin/touch /tmp/x" {
		t.Fatalf("run: %q %d %v", out, exit, err)
	}
	body := []byte("claude 2.1.310")
	if err := b.g.Push(tctx, "/usr/local/bin/claude", 0o755, io.MultiReader(strings.NewReader(string(body))), int64(len(body))); err != nil {
		t.Fatalf("push: %v", err)
	}
	if string(agent.pushed["/usr/local/bin/claude"]) != string(body) {
		t.Fatalf("pushed %q", agent.pushed)
	}
	if set, err := b.g.DesktopPassword(tctx, []byte("Abcd2345")); err != nil || !set || agent.passwords[0] != "Abcd2345" {
		t.Fatalf("desktop_password: %v %v", set, err)
	}
	// The control check's RFB client: VncAuth over the desktop path with the
	// password just set, then one PointerEvent to the set point.
	pointer := make(chan []byte, 1)
	go func() {
		c, err := m.DialGuest(macvm.DesktopPort)
		if err != nil {
			pointer <- nil
			return
		}
		defer c.Close()
		open := make([]byte, 1)
		if _, err := io.ReadFull(c, open); err != nil || open[0] != macvm.DesktopOpenByte {
			pointer <- nil
			return
		}
		appleFake{version: "RFB 003.008\n", offered: []byte{2}, password: "Abcd2345"}.serve(t, c)
		ev := make([]byte, 6)
		_, _ = io.ReadFull(c, ev)
		pointer <- ev
	}()
	if err := b.g.(macvm.DesktopProbe).MovePointer(tctx, []byte("Abcd2345"), 960, 540); err != nil {
		t.Fatalf("move pointer: %v", err)
	}
	if ev := <-pointer; !bytes.Equal(ev, []byte{5, 0, 0x03, 0xc0, 0x02, 0x1c}) {
		t.Fatalf("pointer event %v", ev)
	}
	if x, y, err := b.g.Pointer(tctx); err != nil || x != 960 || y != 540 {
		t.Fatalf("pointer %d,%d %v", x, y, err)
	}
	if d, err := b.g.ToolsDial(tctx); err != nil || d != macvm.ToolsDialConnected {
		t.Fatalf("tools dial %q %v", d, err)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if err := b.stop(sctx); err != nil {
		t.Fatal(err)
	}
	if m.State() != macvm.StateStopped {
		t.Fatalf("after stop the VM is %s", m.State())
	}
	if _, err := os.Lstat(sockDir); !os.IsNotExist(err) {
		t.Fatalf("the stage-2 socket dir outlived the boot: %v", err)
	}
}

// With no net_fd the stage-2 boot never starts a VM.
func TestStage2BootWithoutNetFDStartsNothing(t *testing.T) {
	eng := &macvmtest.Engine{}
	cfg := macvm.Stage2BootConfig{Engine: eng, SockRoot: macvmtest.SockRoot(t), SockKey: "0123456789ab", SockFstat: macvmtest.SockFstat, NetWait: 100 * time.Millisecond}
	if _, _, err := cfg.ToolsBoot()(context.Background(), macvm.MachineSpec{}); err == nil || !strings.Contains(err.Error(), "network descriptor") {
		t.Fatalf("got %v", err)
	}
	if len(eng.Machines()) != 0 {
		t.Fatal("a VM was built without its network")
	}
}
