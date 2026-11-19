// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

const (
	driveOpenEntry  = "0123456789abcdef"
	driveOpenObject = "wardyn-drive-abc123-home"
)

// driveOpenBody is a formatted drive's image as far as the runner reads one: 4 KiB or more that do not start
// with zeros.
var driveOpenBody = strings.Repeat("the drive's bytes\n", 256)

// driveOpenGuest plays the guest agent: it records Init's raw body and counts execs.
type driveOpenGuest struct {
	mu      sync.Mutex
	init    []byte
	execs   atomic.Int32
	refuse  bool
	handler http.Handler
}

func newDriveOpenGuest(refuse bool) *driveOpenGuest {
	g := &driveOpenGuest{refuse: refuse}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ready", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(guestagent.Ready{Version: "test"})
	})
	mux.HandleFunc("POST /v1/init", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.init = b
		g.mu.Unlock()
		if g.refuse {
			http.Error(w, "its volume was not mounted", http.StatusUnprocessableEntity)
			return
		}
		_ = json.NewEncoder(w).Encode(guestagent.InitResult{})
	})
	mux.HandleFunc("POST /v1/exec", func(http.ResponseWriter, *http.Request) { g.execs.Add(1) })
	g.handler = mux
	return g
}

// driveOpenRig is a ready base and one drive's image on the fake engine.
type driveOpenRig struct {
	eng      *macvmtest.Engine
	layout   macvm.Layout
	img      string
	sockRoot string
}

func newDriveOpenRig(t *testing.T) *driveOpenRig {
	t.Helper()
	r := &driveOpenRig{eng: &macvmtest.Engine{}, layout: macvm.Layout{Root: t.TempDir()}, sockRoot: macvmtest.SockRoot(t)}
	makeBase(t, r.layout)
	var err error
	if r.img, err = r.layout.DriveImage(driveOpenObject); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(r.img), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.img, []byte(driveOpenBody), 0o600); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *driveOpenRig) options(ready func(macvm.DriveOpenReady)) macvm.DriveOpenOptions {
	return macvm.DriveOpenOptions{Engine: r.eng, Layout: r.layout, Object: driveOpenObject, Label: "Wardyn-abc12345", CPUs: 4, MemoryMiB: 8192,
		Entry: driveOpenEntry, SockRoot: r.sockRoot, SockFstat: macvmtest.SockFstat, Ready: ready}
}

// serve plays g on the machine n once it listens, until ctx ends.
func (r *driveOpenRig) serve(ctx context.Context, t *testing.T, n int, g *driveOpenGuest) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ms := r.eng.Machines(); len(ms) > n && ms[n].Listening(macvm.AgentPort) {
			m := ms[n]
			go func() {
				_ = guestagent.ServeVsock(ctx, func(context.Context) (net.Conn, error) { return m.DialAgent(macvm.AgentPort) }, g.handler)
			}()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the VM never listened on the agent port")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The whole boot: the lock is held from before the VM until it ends, a second start and a run's lock are
// refused, the VM listens on no tools port and no desktop port, its network takes nothing from ctl.sock,
// and Init holds the drive alone.
func TestDriveOpenHoldsTheLockAndHandsTheGuestOnlyTheDrive(t *testing.T) {
	r := newDriveOpenRig(t)
	g := newDriveOpenGuest(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan macvm.DriveOpenReady, 1)
	done := make(chan error, 1)
	go func() { done <- macvm.DriveOpen(ctx, r.options(func(d macvm.DriveOpenReady) { ready <- d })) }()
	r.serve(ctx, t, 0, g)
	var got macvm.DriveOpenReady
	select {
	case got = <-ready:
	case err := <-done:
		t.Fatalf("drive open ended before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("drive open was never ready")
	}
	sockDir := macvm.SockDir(r.sockRoot, macvm.JobSockKey(driveOpenEntry))
	if got.Target != macvm.DriveOpenTarget || got.AgentSock != filepath.Join(sockDir, macvm.AgentSock) {
		t.Fatalf("ready line: %+v", got)
	}

	if _, err := macvm.LockDisk(r.img); !errors.Is(err, macvm.ErrDiskInUse) {
		t.Fatalf("the lock is not held while open: %v", err)
	}
	second := r.options(func(macvm.DriveOpenReady) { t.Error("a second start was ready") })
	second.Entry = "fedcba9876543210"
	if err := macvm.DriveOpen(ctx, second); !errors.Is(err, macvm.ErrDiskInUse) {
		t.Fatalf("a second start: %v, want ErrDiskInUse", err)
	}
	if n := len(r.eng.Machines()); n != 1 {
		t.Fatalf("%d VMs built, want 1: a refused start builds none", n)
	}

	m := r.eng.Machines()[0]
	for _, port := range []uint32{macvm.ToolsPort, macvm.DesktopPort} {
		if m.Listening(port) {
			t.Errorf("the VM listens on vsock %d", port)
		}
	}
	if len(m.Plan.Storage) != 2 || m.Plan.Storage[1].Identifier != macvm.DiskID(0) || m.Plan.Storage[1].Path != r.img {
		t.Fatalf("the VM's storage: %+v", m.Plan.Storage)
	}
	if len(m.Plan.Shares) != 0 {
		t.Fatalf("the VM shares folders: %+v", m.Plan.Shares)
	}
	cc, err := macvm.DialControl(ctx, filepath.Join(sockDir, macvm.ControlSock))
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	st, err := cc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.Listeners, []uint32{macvm.AgentPort}) {
		t.Errorf("listeners %v, want the agent port alone", st.Listeners)
	}
	if f, err := os.Open(os.DevNull); err != nil {
		t.Fatal(err)
	} else {
		defer f.Close()
		if err := cc.SendNetFD(ctx, f); err == nil {
			t.Error("ctl.sock gave the VM a network")
		}
	}

	g.mu.Lock()
	raw := g.init
	g.mu.Unlock()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("Init %q: %v", raw, err)
	}
	if len(fields) != 2 || string(fields["sign_ins_off"]) != "true" || fields["disks"] == nil {
		t.Errorf("Init holds more than the drive: %s", raw)
	}
	var in guestagent.Init
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	want := guestagent.Disk{ID: macvm.DiskID(0), Label: "Wardyn-abc12345", Target: macvm.DriveOpenTarget, SizeBytes: int64(len(driveOpenBody))}
	if len(in.Disks) != 1 || in.Disks[0] != want {
		t.Errorf("Init's disks %+v, want %+v", in.Disks, want)
	}
	if n := g.execs.Load(); n != 0 {
		t.Errorf("%d execs in the guest: the boot starts nothing", n)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("after its stop: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("drive open did not end")
	}
	unlock, err := macvm.LockDisk(r.img)
	if err != nil {
		t.Fatalf("the lock outlived the VM: %v", err)
	}
	unlock()
	if ents, err := os.ReadDir(filepath.Join(r.layout.Root, "drive-open")); err != nil || len(ents) != 0 {
		t.Errorf("the clones left behind: %v %v", ents, err)
	}
	if b, err := os.ReadFile(r.img); err != nil || string(b) != driveOpenBody {
		t.Errorf("the image changed: %q %v", b, err)
	}
}

// A drive this Mac does not hold is refused before anything exists, and no blank one is made for the guest
// to format. So is a name that isn't a drive's, and an entry that couldn't name a directory.
func TestDriveOpenRefusals(t *testing.T) {
	for name, mod := range map[string]func(*macvm.DriveOpenOptions){
		"no such drive": func(o *macvm.DriveOpenOptions) { o.Object = "wardyn-drive-nothere" },
		"not a name":    func(o *macvm.DriveOpenOptions) { o.Object = "../bases/0/disk" },
		"no label":      func(o *macvm.DriveOpenOptions) { o.Label = "" },
		"path entry":    func(o *macvm.DriveOpenOptions) { o.Entry = "../bases" },
		"no entry":      func(o *macvm.DriveOpenOptions) { o.Entry = "" },
	} {
		t.Run(name, func(t *testing.T) {
			r := newDriveOpenRig(t)
			o := r.options(func(macvm.DriveOpenReady) { t.Error("ready") })
			mod(&o)
			if err := macvm.DriveOpen(context.Background(), o); err == nil {
				t.Fatal("not refused")
			}
			if len(r.eng.Machines()) != 0 {
				t.Fatal("a refused open built a VM")
			}
			if _, err := os.Lstat(filepath.Join(r.layout.DrivesDir(), "wardyn-drive-nothere.img")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("an image was made: %v", err)
			}
			if _, err := os.Stat(filepath.Join(r.layout.BaseDir(0), macvm.DiskFile)); err != nil {
				t.Fatalf("the base: %v", err)
			}
		})
	}
}

// An image that starts with 4 KiB of zeros and has no mark is an existing drive damaged there, not a new
// one: drive open refuses it before any VM is built, says it was not formatted and how to start again, and
// leaves the image as it was.
func TestDriveOpenRefusesABlankDriveItDidNotMakeNew(t *testing.T) {
	r := newDriveOpenRig(t)
	blank := make([]byte, 1<<20)
	if err := os.WriteFile(r.img, blank, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // a boot would wait for a guest
	defer cancel()
	err := macvm.DriveOpen(ctx, r.options(func(macvm.DriveOpenReady) { t.Error("ready") }))
	if err == nil || !strings.Contains(err.Error(), "Wardyn did not format this disk drive") ||
		!strings.Contains(err.Error(), "delete this drive and create a new one") {
		t.Fatalf("drive open of a blank image with no mark: %v", err)
	}
	if n := len(r.eng.Machines()); n != 0 {
		t.Fatalf("%d VMs built", n)
	}
	if b, err := os.ReadFile(r.img); err != nil || !bytes.Equal(b, blank) {
		t.Fatalf("the image changed: %v", err)
	}
	unlock, err := macvm.LockDisk(r.img)
	if err != nil {
		t.Fatalf("the lock outlived the refusal: %v", err)
	}
	unlock()
}

// A drive no run has formatted yet still has its mark, so drive open lets the guest have it, and the mark is
// used up before the drive is ready: what the person writes back is never on a drive that reads as new.
func TestDriveOpenUsesUpANewDrivesMark(t *testing.T) {
	r := newDriveOpenRig(t)
	if err := os.Remove(r.img); err != nil {
		t.Fatal(err)
	}
	if err := macvm.MakeDiskImage(r.img, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.img + ".new"); err != nil {
		t.Fatalf("a new image has no mark: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	marked := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- macvm.DriveOpen(ctx, r.options(func(macvm.DriveOpenReady) {
			_, err := os.Stat(r.img + ".new")
			marked <- err
		}))
	}()
	r.serve(ctx, t, 0, newDriveOpenGuest(false))
	select {
	case err := <-marked:
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the mark is there once the drive is ready: %v", err)
		}
	case err := <-done:
		t.Fatalf("drive open ended before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("drive open was never ready")
	}
	cancel()
	<-done
}

// A guest that refuses the drive ends the boot with the reason, and frees the lock and the clone.
func TestDriveOpenGuestRefusalEndsTheBoot(t *testing.T) {
	r := newDriveOpenRig(t)
	// The drive is new and the guest refuses it, so nothing formatted it: its mark stays for the next open.
	if err := os.Remove(r.img); err != nil {
		t.Fatal(err)
	}
	if err := macvm.MakeDiskImage(r.img, 1<<20); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- macvm.DriveOpen(ctx, r.options(func(macvm.DriveOpenReady) { t.Error("ready") })) }()
	r.serve(ctx, t, 0, newDriveOpenGuest(true))
	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("a refused Init: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("the boot did not end")
	}
	if st := r.eng.Machines()[0].State(); st != macvm.StateStopped {
		t.Errorf("the VM is %s", st)
	}
	unlock, err := macvm.LockDisk(r.img)
	if err != nil {
		t.Fatalf("the lock outlived the VM: %v", err)
	}
	unlock()
	if _, err := os.Stat(r.img + ".new"); err != nil {
		t.Fatalf("a refused Init used up the new drive's mark: %v", err)
	}
}

// A clone a dead helper left goes at the next open; one too new to judge stays.
func TestDriveOpenSweepsDeadHelpersClones(t *testing.T) {
	r := newDriveOpenRig(t)
	root := filepath.Join(r.layout.Root, "drive-open")
	dead, fresh := filepath.Join(root, "aaaaaaaaaaaa"), filepath.Join(root, "bbbbbbbbbbbb")
	for _, d := range []string{dead, fresh} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dead, old, old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- macvm.DriveOpen(ctx, r.options(func(macvm.DriveOpenReady) { close(ready) })) }()
	r.serve(ctx, t, 0, newDriveOpenGuest(false))
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("never ready")
	}
	if _, err := os.Lstat(dead); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dead helper's clone stayed: %v", err)
	}
	if _, err := os.Lstat(fresh); err != nil {
		t.Errorf("a new clone was swept: %v", err)
	}
	cancel()
	<-done
}
