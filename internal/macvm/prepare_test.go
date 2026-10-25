// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/macvmtest"
)

// fakePower is a settable power source.
type fakePower struct {
	mu sync.Mutex
	ac bool
}

func (p *fakePower) OnAC(context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ac, nil
}
func (p *fakePower) set(ac bool) { p.mu.Lock(); p.ac = ac; p.mu.Unlock() }

// ipswServer serves a restore image with Range support and counts bytes.
type ipswServer struct {
	*httptest.Server
	body   []byte
	served atomic.Int64
	reqs   atomic.Int64
	// midRange, if set, runs half way through each range (by range index).
	midRange func(i int64)
}

func newIPSWServer(t *testing.T, size int) *ipswServer {
	t.Helper()
	s := &ipswServer{body: bytes.Repeat([]byte("macOS-restore-image!"), size/20)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := s.reqs.Add(1) - 1
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		total := int64(len(s.body))
		if start >= total {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= total {
			end = total - 1
		}
		chunk := s.body[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.Header().Set("Content-Length", fmt.Sprint(len(chunk)))
		w.WriteHeader(http.StatusPartialContent)
		half := len(chunk) / 2
		n, _ := w.Write(chunk[:half])
		s.served.Add(int64(n))
		w.(http.Flusher).Flush()
		if s.midRange != nil {
			s.midRange(i)
		}
		n, _ = w.Write(chunk[half:])
		s.served.Add(int64(n))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *ipswServer) url() string { return s.URL + "/x/UniversalMac_27.0_26A1_Restore.ipsw" }

func (s *ipswServer) sha() string {
	h := sha256.Sum256(s.body)
	return hex.EncodeToString(h[:])
}

// prepNet is a fake in-helper relay: DialGuest reaches the guest unless dialErr.
type prepNet struct {
	dialErr error
	onDial  func() // called on each dial
	closed  chan struct{}
	once    sync.Once
}

func (n *prepNet) Serve(*os.File) error { <-n.closed; return nil }
func (n *prepNet) DHCPRequests() uint64 { return 0 }
func (n *prepNet) DialGuest(context.Context, int) (net.Conn, error) {
	if n.onDial != nil {
		n.onDial()
	}
	if n.dialErr != nil {
		return nil, n.dialErr
	}
	a, b := net.Pipe()
	_ = b.Close()
	return a, nil
}
func (n *prepNet) Close() error { n.once.Do(func() { close(n.closed) }); return nil }

type bootstrapper struct {
	mu sync.Mutex
	in macvm.BootstrapInput
}

func (b *bootstrapper) Bootstrap(_ context.Context, in macvm.BootstrapInput) (macvm.BootstrapResult, error) {
	b.mu.Lock()
	b.in = in
	b.mu.Unlock()
	return macvm.BootstrapResult{Desktop: macvm.DesktopControl, AgentVersion: "0.10.0", Harnesses: []string{"none"}}, nil
}

// events collects prepare events.
type events struct {
	mu  sync.Mutex
	evs []macvm.PrepareEvent
	on  func(macvm.PrepareEvent)
}

func (e *events) add(ev macvm.PrepareEvent) {
	e.mu.Lock()
	e.evs = append(e.evs, ev)
	on := e.on
	e.mu.Unlock()
	if on != nil {
		on(ev)
	}
}

func (e *events) paused(phase string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.evs {
		if ev.Phase == phase && ev.Paused {
			return true
		}
	}
	return false
}

type prepFixture struct {
	eng   *macvmtest.Engine
	fb    *macvmtest.FirstBoot
	power *fakePower
	srv   *ipswServer
	net   *prepNet
	boot  *bootstrapper
	ev    *events
	logs  *bytes.Buffer
	opts  macvm.PrepareOptions
}

func newPrep(t *testing.T) *prepFixture {
	t.Helper()
	f := &prepFixture{
		fb: &macvmtest.FirstBoot{}, power: &fakePower{ac: true}, srv: newIPSWServer(t, 8<<10),
		net: &prepNet{closed: make(chan struct{})}, boot: &bootstrapper{}, ev: &events{}, logs: &bytes.Buffer{},
	}
	f.eng = &macvmtest.Engine{URL: f.srv.url(), Image: macvm.RestoreImage{BuildVersion: "26A1", OSVersion: "27.0", MinCPUs: 4, MinMemoryBytes: 4 << 30}}
	var logMu sync.Mutex
	f.opts = macvm.PrepareOptions{
		Engine: f.eng, Layout: macvm.Layout{Root: t.TempDir()}, Power: f.power, FirstBoot: f.fb,
		PrepNet: func() (macvm.PrepNetwork, error) { return f.net, nil }, Bootstrap: f.boot,
		CPUs: 4, MemoryMiB: 8192, DiskBytes: 1 << 20, HelperVersion: "test", Progress: f.ev.add,
		Log:       slog.New(slog.NewJSONHandler(lockedWriter{&logMu, f.logs}, nil)),
		PowerPoll: time.Millisecond, DownloadRange: 1 << 10, RemoteLoginWait: 2 * time.Second,
	}
	// The fake first boot shuts down when asked; stage 1 runs in between.
	return f
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func TestPrepareHappyPath(t *testing.T) {
	f := newPrep(t)
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	if m.State != macvm.BaseReady || m.IPSWSHA256 != f.srv.sha() || m.MinCPUs != 4 || m.MinMemoryMiB != 4096 ||
		m.Desktop != macvm.DesktopControl || m.GuestOS != "macOS 27.0 (26A1)" || m.AgentVersion != "0.10.0" {
		t.Fatalf("manifest = %+v", m)
	}
	if got, err := macvm.ReadyBase(base); err != nil || got.IPSWSHA256 != m.IPSWSHA256 {
		t.Fatalf("ReadyBase: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "installing")); !os.IsNotExist(err) {
		t.Fatal("install marker left on a ready base")
	}
	if des, _ := os.ReadDir(f.opts.Layout.CacheDir()); len(des) != 0 {
		t.Fatalf("restore image kept without KeepIPSW: %v", des)
	}
	args := f.fb.Last()
	if f.fb.Boots() != 1 || args.Username != "wardyn-admin" || len(args.Password) < 40 || args.CPUs != 4 || args.MemoryBytes != 8<<30 {
		t.Fatalf("first boot args = %+v", args)
	}
	if f.boot.in.Password != args.Password {
		t.Fatal("stage 1 did not get the provisioned password")
	}
	if strings.Contains(f.logs.String(), args.Password) {
		t.Fatal("the admin password reached the log")
	}
	if fi, err := os.Stat(base); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("base dir mode %v %v", fi.Mode(), err)
	}
	// A second Prepare keeps the ready base.
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil || f.eng.Installs() != 1 {
		t.Fatalf("re-prepare of a ready base: %v, %d installs", err, f.eng.Installs())
	}
}

// On battery the download pauses at the end of the range in flight
// and resumes from the next range on AC, so nothing is fetched twice.
func TestPrepareDownloadPausesOnBatteryAndResumes(t *testing.T) {
	f := newPrep(t)
	f.srv.midRange = func(i int64) {
		if i == 3 {
			f.power.set(false)
		}
	}
	resumed := make(chan struct{})
	var once sync.Once
	f.ev.on = func(ev macvm.PrepareEvent) {
		if ev.Phase == macvm.PhaseDownload && ev.Paused {
			once.Do(func() {
				go func() {
					time.Sleep(30 * time.Millisecond)
					f.power.set(true)
					close(resumed)
				}()
			})
		}
	}
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-resumed:
	default:
		t.Fatal("the download never paused on battery")
	}
	if m.IPSWSHA256 != f.srv.sha() {
		t.Fatal("resumed download is corrupt")
	}
	if extra := f.srv.served.Load() - int64(len(f.srv.body)); extra != 0 {
		t.Fatalf("re-fetched %d bytes; the range in flight should finish", extra)
	}
	if got, rng := f.srv.reqs.Load(), int64(f.opts.DownloadRange); got != (int64(len(f.srv.body))+rng-1)/rng {
		t.Fatalf("%d range requests", got)
	}
}

// Every later step starts only on AC and is never paused: going on
// battery mid-install changes nothing, and the next step waits for AC.
func TestPrepareLaterStepsNeverPause(t *testing.T) {
	f := newPrep(t)
	f.eng.InstallFunc = func(ctx context.Context, m *macvmtest.Machine, ipsw string, progress func(float64)) error {
		f.power.set(false)
		time.Sleep(30 * time.Millisecond)
		progress(0.5)
		time.Sleep(30 * time.Millisecond)
		progress(1)
		return ctx.Err()
	}
	f.ev.on = func(ev macvm.PrepareEvent) {
		if ev.Phase == macvm.PhaseProvision && ev.Paused {
			go f.power.set(true)
		}
	}
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if f.ev.paused(macvm.PhaseInstall) {
		t.Fatal("the install paused on battery")
	}
	if !f.ev.paused(macvm.PhaseProvision) {
		t.Fatal("the first boot started on battery")
	}
	if f.eng.Installs() != 1 {
		t.Fatalf("%d installs", f.eng.Installs())
	}
}

// An install cut short (the process or power is lost) leaves no
// trusted disk: the next Prepare discards the partial base and restarts from
// create with the cached restore image, without downloading again.
func TestPrepareInterruptedInstallRestartsFromCreate(t *testing.T) {
	f := newPrep(t)
	f.opts.KeepIPSW = true
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	f.eng.InstallFunc = func(ictx context.Context, m *macvmtest.Machine, ipsw string, progress func(float64)) error {
		if err := os.WriteFile(m.Plan.Storage[0].Path, []byte("partial install"), 0o600); err != nil {
			return err
		}
		close(started)
		<-ictx.Done()
		return ictx.Err()
	}
	go func() { <-started; cancel() }()
	if _, err := macvm.Prepare(ctx, f.opts); err == nil {
		t.Fatal("interrupted prepare succeeded")
	}
	base := f.opts.Layout.BaseDir(0)
	if _, err := os.Stat(filepath.Join(base, "installing")); err != nil {
		t.Fatalf("no install marker after an interrupted install: %v", err)
	}
	reqs := f.srv.reqs.Load()
	f.eng.InstallFunc = nil
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if f.eng.Installs() != 2 || f.srv.reqs.Load() != reqs {
		t.Fatalf("installs %d, extra requests %d; want a re-install from the cache", f.eng.Installs(), f.srv.reqs.Load()-reqs)
	}
	disk, _ := os.ReadFile(filepath.Join(base, macvm.DiskFile))
	if bytes.Contains(disk, []byte("partial install")) {
		t.Fatal("the partial disk was kept")
	}
	if m.State != macvm.BaseReady {
		t.Fatalf("manifest %+v", m)
	}
}

func TestPrepareValidationRetriesOnce(t *testing.T) {
	f := newPrep(t)
	f.eng.LoadFailures = 1
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if got := f.srv.served.Load(); got != 2*int64(len(f.srv.body)) {
		t.Fatalf("served %d bytes, want two downloads", got)
	}
	g := newPrep(t)
	g.eng.LoadFailures = 2
	if _, err := macvm.Prepare(context.Background(), g.opts); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("two failed validations: %v", err)
	}
}

func TestPrepareBelowMinimumNamesTheFlag(t *testing.T) {
	f := newPrep(t)
	f.opts.CPUs = 2
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil || !strings.Contains(err.Error(), "--per-run-cpu") {
		t.Fatalf("below min CPUs: %v", err)
	}
	g := newPrep(t)
	g.opts.MemoryMiB = 2048
	if _, err := macvm.Prepare(context.Background(), g.opts); err == nil || !strings.Contains(err.Error(), "--per-run-memory") {
		t.Fatalf("below min memory: %v", err)
	}
	if f.fb.Boots()+g.fb.Boots() != 0 {
		t.Fatal("a VM booted below the minimum")
	}
}

func TestPrepareManualSetupWhenRemoteLoginStaysSilent(t *testing.T) {
	f := newPrep(t)
	f.net.dialErr = errors.New("connection refused")
	f.opts.RemoteLoginWait = 100 * time.Millisecond
	if _, err := macvm.Prepare(context.Background(), f.opts); !errors.Is(err, macvm.ErrManualSetup) {
		t.Fatalf("got %v, want ErrManualSetup", err)
	}
}

// Cancelling prepare while it waits for Remote Login is a cancellation, not
// a request for setup by hand.
func TestPrepareCancelWhileWaitingIsNotManualSetup(t *testing.T) {
	f := newPrep(t)
	f.net.dialErr = errors.New("connection refused")
	f.opts.RemoteLoginWait = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.net.onDial = cancel
	_, err := macvm.Prepare(ctx, f.opts)
	if !errors.Is(err, context.Canceled) || errors.Is(err, macvm.ErrManualSetup) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestPrepareStopsBeforeDownloadWithoutProvisioning(t *testing.T) {
	f := newPrep(t)
	f.fb.Unsupported = errors.New("VZMacGuestProvisioningOptions missing")
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil {
		t.Fatal("prepared without provisioning")
	}
	if f.srv.reqs.Load() != 0 {
		t.Fatal("downloaded before checking provisioning")
	}
	g := newPrep(t)
	g.eng.Thermal = macvm.ThermalSerious
	if _, err := macvm.Prepare(context.Background(), g.opts); !errors.Is(err, macvm.ErrThermal) {
		t.Fatalf("hot Mac: %v", err)
	}
}

func TestPrepareTakesAndReleasesItsAdmission(t *testing.T) {
	f := newPrep(t)
	var took, released int
	f.opts.Admit = func(context.Context) (func(), error) { took++; return func() { released++ }, nil }
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if took != 1 || released != 1 {
		t.Fatalf("admission took %d released %d", took, released)
	}
	g := newPrep(t)
	g.opts.Admit = func(context.Context) (func(), error) { return nil, errors.New("full") }
	if _, err := macvm.Prepare(context.Background(), g.opts); err == nil || g.srv.reqs.Load() != 0 {
		t.Fatalf("prepare without admission: %v", err)
	}
}
