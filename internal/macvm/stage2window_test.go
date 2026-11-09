// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// editActionLog records the edit actions one guest agent session was asked for.
type editActionLog struct {
	mu    sync.Mutex
	paths []string
}

func (l *editActionLog) serve(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	for _, p := range []string{"/edit/admin-terminal", "/edit/trust-cas", "/edit/full-disk-access", "/edit/shut-down"} {
		mux.HandleFunc("POST "+p, func(w http.ResponseWriter, _ *http.Request) {
			l.mu.Lock()
			l.paths = append(l.paths, p)
			l.mu.Unlock()
			if p == "/edit/shut-down" {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			_ = json.NewEncoder(w).Encode(guestagent.EditActionResult{Opened: true})
		})
	}
	mux.Handle("/", next)
	return mux
}

func (l *editActionLog) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.paths)
}

// editActions is the window guest's edit actions, as the window's buttons call them.
type editActions interface {
	EditAdminTerminal(ctx context.Context) (guestagent.EditActionResult, error)
	EditTrustCAs(ctx context.Context) (guestagent.EditActionResult, error)
	EditFullDiskAccess(ctx context.Context) (guestagent.EditActionResult, error)
}

// The edit window's boot is the stage-2 tools boot on the NIC it is given: it starts with no net_fd, its
// ctl.sock says the network is attached and refuses another, and its guest gives the window the VM to
// draw, asks the guest to shut down and sees it stop. The edit actions go over the tools session only,
// never the control session that a run's exec uses. A window boot with no NIC builds nothing.
func TestStage2WindowBootTakesItsNICAndKeepsEditActionsOnTheToolsSession(t *testing.T) {
	eng := &windowEngine{Engine: &macvmtest.Engine{}}
	root := macvmtest.SockRoot(t)
	cfg := macvm.Stage2BootConfig{Engine: eng, SockRoot: root, SockKey: macvm.JobSockKey("0123456789abcdef01234567"), SockFstat: macvmtest.SockFstat, GuestWait: 10 * time.Second}
	dir := t.TempDir()
	for _, f := range []string{macvm.DiskFile, macvm.AuxFile} {
		_ = os.WriteFile(filepath.Join(dir, f), nil, 0o600)
	}
	_ = os.WriteFile(filepath.Join(dir, macvm.PairKeyFile), pairKey, 0o600)
	spec := macvm.MachineSpec{CPUs: 4, MemoryBytes: 8 << 30, MinCPUs: 2, MinMemoryBytes: 4 << 30, HardwareModel: []byte("hw"),
		MachineIdentifier: []byte("id"), DiskPath: filepath.Join(dir, macvm.DiskFile), AuxPath: filepath.Join(dir, macvm.AuxFile)}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, _, err := cfg.WindowBoot()(ctx, spec, nil); err == nil || len(eng.Machines()) != 0 {
		t.Fatalf("a window boot with no NIC: %v, %d VMs built", err, len(eng.Machines()))
	}

	nic, host, err := macvm.NewNetPair()
	if err != nil {
		t.Fatal(err)
	}
	defer nic.Close()
	defer host.Close()
	g, stop, err := cfg.WindowBoot()(ctx, spec, nic)
	if err != nil {
		t.Fatal(err)
	}
	m := eng.Machines()[0]
	if m.State() != macvm.StateRunning || len(m.Plan.Network) != 1 || m.Plan.Network[0].File != nic {
		t.Fatalf("the VM is %s on %+v, want running on the given NIC", m.State(), m.Plan.Network)
	}
	sockDir := macvm.SockDir(root, "0123456789ab")
	cc, err := macvm.DialControl(ctx, filepath.Join(sockDir, macvm.ControlSock))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	if st, err := cc.Status(ctx); err != nil || st.Network != macvm.NetAttached ||
		!slices.Equal(st.Listeners, []uint32{macvm.AgentPort, macvm.DesktopPort, macvm.ToolsPort}) {
		t.Fatalf("status %+v %v", st, err)
	}
	other, peer, err := macvm.NewNetPair()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := cc.SendNetFD(ctx, other); err == nil {
		t.Error("the window boot took a second network")
	}
	_ = other.Close()

	tools, control := &editActionLog{}, &editActionLog{}
	agent := &fakeToolsAgent{pushed: map[string][]byte{}}
	go func() {
		_ = guestagent.ServeVsock(ctx, func(context.Context) (net.Conn, error) { return m.DialAgent(macvm.ToolsPort) }, tools.serve(agent.tools()))
	}()
	go func() {
		_ = guestagent.ServeVsock(ctx, func(context.Context) (net.Conn, error) { return m.DialAgent(macvm.AgentPort) }, control.serve(controlSession()))
	}()
	acts, ok := g.(editActions)
	if !ok {
		t.Fatalf("the window boot's guest %T has no edit actions", g)
	}
	for _, act := range []func(context.Context) (guestagent.EditActionResult, error){acts.EditAdminTerminal, acts.EditTrustCAs, acts.EditFullDiskAccess} {
		if r, err := act(ctx); err != nil || !r.Opened {
			t.Fatalf("edit action: %+v %v", r, err)
		}
	}
	sg, ok := g.(macvm.ShutDownGuest)
	if !ok {
		t.Fatalf("the window boot's guest %T cannot ask its agent to shut down", g)
	}
	if err := sg.EditShutDown(ctx); err != nil {
		t.Fatalf("shut-down: %v", err)
	}
	if got, want := tools.got(), []string{"/edit/admin-terminal", "/edit/trust-cas", "/edit/full-disk-access", "/edit/shut-down"}; !slices.Equal(got, want) {
		t.Errorf("the tools session served %q, want %q", got, want)
	}
	if got := control.got(); len(got) != 0 {
		t.Errorf("the control session was asked for edit actions %q", got)
	}

	wg, ok := g.(macvm.WindowGuest)
	if !ok {
		t.Fatalf("the window boot's guest %T is not a window guest", g)
	}
	if ptr, err := wg.ViewHandle(); err != nil || ptr != unsafe.Pointer(&vmByte) {
		t.Fatalf("view %v %v", ptr, err)
	}
	if err := wg.RequestStop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wg.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the guest did not stop when asked")
	}
	if wg.State() != macvm.StateStopped {
		t.Fatalf("after the guest's shutdown the VM is %s", wg.State())
	}
	if err := stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sockDir); !os.IsNotExist(err) {
		t.Fatalf("the window boot's socket dir outlived it: %v", err)
	}
}
