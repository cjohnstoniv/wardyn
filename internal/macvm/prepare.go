// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

// BaseDiskBytes is the base image's sparse disk size.
const BaseDiskBytes int64 = 50 << 30

// Prepare phases, in order.
const (
	PhaseCheck     = "check"
	PhaseDownload  = "download"
	PhaseValidate  = "validate"
	PhaseCreate    = "create"
	PhaseInstall   = "install"
	PhaseProvision = "provision"
	PhaseBootstrap = "bootstrap"
	PhaseShutdown  = "shutdown"
	PhaseReady     = "ready"
	// PhaseSetupByHand: macOS asked for its setup by hand, and the one-time
	// code is in Layout.SetupCode() on this Mac. Its event carries no
	// detail: an event's detail is logged, and the code is never logged.
	PhaseSetupByHand = "setup_by_hand"
)

// PrepareEvent is one progress report.
type PrepareEvent struct {
	Phase    string  `json:"phase"`
	Fraction float64 `json:"fraction,omitempty"`
	Bytes    int64   `json:"bytes,omitempty"`
	Total    int64   `json:"total,omitempty"`
	// BytesPerSec is the download rate since the download (re)started.
	BytesPerSec float64 `json:"bytes_per_sec,omitempty"`
	Paused      bool    `json:"paused,omitempty"`
	Detail      string  `json:"detail,omitempty"`
	// ToolsDial is the guest agent's classified tools-port dial, recorded
	// once per stage-2 boot.
	ToolsDial string `json:"tools_dial,omitempty"`
	// Retry marks a download event for a range that failed and is fetched
	// again; Bytes is that range's start.
	Retry bool `json:"retry,omitempty"`
	// Version is the restore image's macOS version: parsed from its file name
	// on the download's first event, then read from the image by the second
	// validate event, which also carries the file's size as Total.
	Version string `json:"version,omitempty"`
}

// InstallFailurePrefix starts the error an install that failed ends with; the cause follows it.
const InstallFailurePrefix = "macvm: install: "

// PrepNetwork is the in-helper relay prepare's VM uses (MACVM-NET): it serves
// the guest NIC with no proxy, so the guest reaches nothing, and it can dial
// into the guest.
type PrepNetwork interface {
	// Serve relays the host side of the guest NIC until Close.
	Serve(nic *os.File) error
	DialGuest(ctx context.Context, port int) (net.Conn, error)
	// DHCPRequests counts the guest's DHCP requests
	// (vmnet.Relay.DHCPRequests): stage 1's boot epoch.
	DHCPRequests() uint64
	Close() error
}

// BootstrapInput is what stage 1 gets: a dialer into the running
// provisioned guest and its one-time admin account. Password is generated
// per prepare, held in this process's memory only, never written or logged,
// and useless once stage 1 has turned Remote Login off.
type BootstrapInput struct {
	Dial     func(ctx context.Context, port int) (net.Conn, error)
	Username string
	Password string
	// NewPassword, when set, means Password is the one-time code the Mac's
	// person gave the account in Setup Assistant (setup by hand): stage 1's
	// first SSH step replaces it with NewPassword (a random 32-byte one held
	// in memory only) and then calls Replaced, before anything else.
	NewPassword string
	Replaced    func()
	// Progress reports each stage-1 step as a PhaseBootstrap event. Its
	// detail carries no secret.
	Progress func(PrepareEvent)
	// GuestEpoch identifies the guest's current boot as the helper sees it
	// without SSH: it fails once the VM has stopped, and otherwise counts the
	// DHCP requests the prep relay saw, which a reboot or a reset network
	// adds to. The provisioning boot has no vsock, so this is the strongest
	// liveness the helper has. Stage 1 counts a refused port 22 as Remote
	// Login off only when the epoch is the same before and after.
	GuestEpoch func() (uint64, error)
}

// ErrGuestStopped means the provisioning VM stopped.
var ErrGuestStopped = errors.New("macvm: the provisioning VM stopped")

// BootstrapResult is what stage 1 found.
type BootstrapResult struct {
	Desktop      string // DesktopControl, DesktopObserve or DesktopNone
	AgentVersion string
	Harnesses    []string
}

// Bootstrapper runs stage 1 over SSH (copy and enable the guest agent, create
// the agent user, Screen Sharing and its control check, guest power settings,
// Remote Login off).
type Bootstrapper interface {
	Bootstrap(ctx context.Context, in BootstrapInput) (BootstrapResult, error)
}

// PrepareOptions configures Prepare.
type PrepareOptions struct {
	Engine Engine
	Layout Layout
	Slot   int
	Power  PowerSource
	// Admit takes the host ledger entry once the host checks pass, and
	// returns its release. Nil means the caller already holds one.
	Admit func(ctx context.Context) (release func(), err error)
	// FirstBoot runs the base's first boot (macvm.NewProvisioner()).
	FirstBoot FirstBooter
	PrepNet   func() (PrepNetwork, error)
	Bootstrap Bootstrapper
	// Stage2 installs the developer tools (nil: the base serves "none" only).
	Stage2 Stage2
	HTTP   *http.Client
	// CPUs and MemoryMiB are the runner's per-run limits. Prepare's VM uses
	// them as they are; below the image's minimum, prepare stops.
	CPUs      int
	MemoryMiB uint64
	DiskBytes int64 // 0: BaseDiskBytes
	KeepIPSW  bool
	// Reprepare builds a whole new base even when one is ready (a newer
	// restore image, or the operator asked) and swaps it in for the old one.
	// Without it a ready base is returned as it is.
	Reprepare bool
	// HelperVersion goes into the manifest.
	HelperVersion string
	Progress      func(PrepareEvent)
	Log           *slog.Logger
	// InstallProcess, when set, is the argv of the process that runs the
	// install (the helper with InstallCommand, which calls RunInstall). Unset,
	// the install runs in this process, which only a fake engine survives: see
	// RunInstall.
	InstallProcess []string
	// PowerPoll is how often power is checked while waiting or downloading.
	PowerPoll time.Duration
	// DownloadRange overrides DownloadRange (tests).
	DownloadRange int64
	// RemoteLoginWait bounds the wait for the provisioned guest's Remote Login.
	RemoteLoginWait time.Duration
	// SetupByHandWait, when set, turns on setup by hand: if Remote Login
	// stays silent, or at once on a host without guest provisioning, prepare
	// writes a one-time code to Layout.SetupCode(), opens the guest's window
	// (FirstBoot.Screen) and waits this long for the Mac's person to finish
	// Setup Assistant with it and turn Remote Login on. Zero stops with
	// ErrManualSetup instead, and refuses a host without provisioning.
	SetupByHandWait time.Duration
}

// DefaultSetupByHandWait is the helper's setup-by-hand wait: owner packet 1
// D1: (a) with (c) as the fallback; 0 = (a) only.
const DefaultSetupByHandWait = time.Hour

// ErrManualSetup means the provisioned first boot did not come up with Remote
// Login: macOS asked for its setup by hand.
var ErrManualSetup = errors.New("macvm: macOS asked for setup by hand")

const provisionUser = "wardyn-admin"

// Prepare builds the base image in o.Layout.BaseDir(o.Slot) from the newest
// restore image this Mac supports.
//
// Power, per phase: the download pauses on battery at the end of its current
// range and resumes from that range on AC. Every later step (validate,
// create, install, first boot, stage 1) starts only on AC and is then never
// paused: going on battery mid-step changes nothing, because pausing or
// stopping the installer is undefined behaviour. If such a step is cut short
// anyway (power loss, restart, the helper dies), the next Prepare discards
// the partial base and restarts from create with the cached restore image.
//
// A re-prepare (o.Reprepare with a ready base), and a prepare that replaces a
// complete base in a format this helper does not run, never write under the
// live base. Like a tools update it holds the base's update lock for its whole run,
// builds the new base in the staging dir <base>.update, and swaps it in with
// one exchange under the base's lock; runs keep cloning the old base
// meanwhile, and the old generation is deleted after the exchange. Cut short,
// it leaves the live base as it was, and the next update or re-prepare sweeps
// the staging dir.
func Prepare(ctx context.Context, o PrepareOptions) (Manifest, error) {
	p, err := newPrep(o)
	if err != nil {
		return Manifest{}, err
	}
	return p.run(ctx)
}

type prep struct {
	PrepareOptions
	base, cache string
	// byHand: the host has no guest provisioning, so the first boot is plain
	// and goes straight to setup by hand.
	byHand bool
}

func newPrep(o PrepareOptions) (*prep, error) {
	if o.Engine == nil || o.Power == nil || o.FirstBoot == nil || o.Layout.Root == "" {
		return nil, errors.New("macvm: prepare needs an engine, a first-boot provisioner, a power source and a root")
	}
	if o.HTTP == nil {
		o.HTTP = http.DefaultClient
	}
	if o.DiskBytes == 0 {
		o.DiskBytes = BaseDiskBytes
	}
	if o.PowerPoll == 0 {
		o.PowerPoll = 2 * time.Second
	}
	if o.DownloadRange == 0 {
		o.DownloadRange = DownloadRange
	}
	if o.RemoteLoginWait == 0 {
		o.RemoteLoginWait = 10 * time.Minute
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if o.Progress == nil {
		o.Progress = func(PrepareEvent) {}
	}
	return &prep{PrepareOptions: o, base: o.Layout.BaseDir(o.Slot), cache: o.Layout.CacheDir()}, nil
}

func (p *prep) emit(ev PrepareEvent) {
	p.Log.Info("macvm: prepare", "phase", ev.Phase, "fraction", ev.Fraction, "bytes", ev.Bytes, "total", ev.Total, "paused", ev.Paused, "detail", ev.Detail, "tools_dial", ev.ToolsDial)
	p.Progress(ev)
}

// check is PhaseCheck: the host, then provisioning or the setup by hand that
// replaces it, then the heat.
func (p *prep) check() error {
	if err := p.Engine.HostCheck(); err != nil {
		return err
	}
	if err := p.FirstBoot.Supported(); err != nil {
		if !errors.Is(err, ErrNoProvisioning) || p.SetupByHandWait == 0 {
			return err
		}
		if err := SetupByHandReady(p.FirstBoot, p.SetupByHandWait); err != nil {
			return err
		}
		p.byHand = true
	}
	return thermalGate(p.Engine)
}

func (p *prep) run(ctx context.Context) (Manifest, error) {
	p.emit(PrepareEvent{Phase: PhaseCheck})
	if err := p.check(); err != nil {
		return Manifest{}, err
	}
	// A ready base is kept unless a re-prepare was asked for.
	if m, err := ReadyBase(p.base); err == nil && !p.Reprepare {
		return m, nil
	}
	for _, d := range []string{p.Layout.Root, p.cache, filepath.Dir(p.base)} {
		if err := mkdirPrivate(d); err != nil {
			return Manifest{}, err
		}
	}
	staging, done, err := beginUpdate(ctx, p.base, p.Log)
	if err != nil {
		return Manifest{}, err
	}
	// The admission entry is released last, after done has deleted the old
	// generation that entry reserves disk for.
	release := func() {}
	defer func() { done(); release() }()
	// A first prepare, or one after an interrupted install, builds in place:
	// until its manifest is ready nothing clones it. A re-prepare, and the
	// replacement of a complete base in another format, build in the staging
	// dir and leave the old base as it is until the swap.
	live := p.base
	if m, err := ReadyBase(live); err == nil {
		if !p.Reprepare {
			return m, nil // another prepare finished it while this one waited
		}
		p.base = staging
	} else if finishedBase(live) {
		p.base = staging
	}
	if p.Admit != nil {
		r, err := p.Admit(ctx)
		if err != nil {
			return Manifest{}, err
		}
		release = r
	}
	if err := mkdirPrivate(p.base); err != nil {
		return Manifest{}, err
	}
	if err := p.discardInterrupted(); err != nil {
		return Manifest{}, err
	}

	img, sha, ipsw, err := p.restoreImage(ctx)
	if err != nil {
		return Manifest{}, err
	}
	if p.CPUs < img.MinCPUs {
		return Manifest{}, fmt.Errorf("macvm: this Mac's per-run CPU limit (%d) is below macOS's minimum of %d: raise --per-run-cpu", p.CPUs, img.MinCPUs)
	}
	if p.MemoryMiB*mib < img.MinMemoryBytes {
		return Manifest{}, fmt.Errorf("macvm: this Mac's per-run memory limit (%d MiB) is below macOS's minimum of %d MiB: raise --per-run-memory", p.MemoryMiB, img.MinMemoryBytes/mib)
	}

	// From here on every step starts only on AC power and is never paused;
	// an interruption leaves the marker, and the next Prepare restarts here.
	if err := p.waitAC(ctx, PhaseCreate); err != nil {
		return Manifest{}, err
	}
	p.emit(PrepareEvent{Phase: PhaseCreate})
	if err := os.WriteFile(filepath.Join(p.base, installingMarker), nil, 0o600); err != nil {
		return Manifest{}, err
	}
	machineID, err := p.create(img)
	if err != nil {
		return Manifest{}, err
	}
	spec := MachineSpec{
		CPUs: p.CPUs, MemoryBytes: p.MemoryMiB * mib,
		MinCPUs: img.MinCPUs, MinMemoryBytes: img.MinMemoryBytes,
		HardwareModel: img.HardwareModel, MachineIdentifier: machineID,
		DiskPath: filepath.Join(p.base, DiskFile), AuxPath: filepath.Join(p.base, AuxFile),
	}
	if err := p.install(ctx, spec, ipsw); err != nil {
		return Manifest{}, err
	}
	res, err := p.provisionAndBootstrap(ctx, spec)
	if err != nil {
		return Manifest{}, err
	}
	// Stage 2 runs while the install marker is still down: cut short, the
	// whole base is discarded and the next Prepare restarts from create.
	if err := p.stage2(ctx, spec, &res); err != nil {
		return Manifest{}, err
	}

	m := Manifest{
		Format: BaseFormat, State: BaseReady, GuestOS: fmt.Sprintf("macOS %s (%s)", img.OSVersion, img.BuildVersion),
		BuildVersion: img.BuildVersion, IPSWSHA256: sha, HelperVersion: p.HelperVersion, AgentVersion: res.AgentVersion,
		CreatedAt: time.Now().UTC(), MinCPUs: img.MinCPUs, MinMemoryMiB: img.MinMemoryBytes / mib,
		DiskBytes: p.DiskBytes, HardwareModel: img.HardwareModel, Desktop: res.Desktop, Harnesses: res.Harnesses,
	}
	if err := writeJSONAtomic(filepath.Join(p.base, ManifestFile), m, 0o600); err != nil {
		return Manifest{}, err
	}
	if err := os.Remove(filepath.Join(p.base, installingMarker)); err != nil {
		return Manifest{}, err
	}
	if p.base != live {
		if err := swapBase(ctx, p.base, live); err != nil {
			return Manifest{}, err
		}
	}
	if !p.KeepIPSW {
		_ = os.Remove(ipsw)
	}
	p.emit(PrepareEvent{Phase: PhaseReady, Fraction: 1})
	return m, nil
}

// stage2 installs the developer tools and records which harnesses the base can
// serve. Like every step after the download it starts only on AC and is never
// paused.
func (p *prep) stage2(ctx context.Context, spec MachineSpec, res *BootstrapResult) error {
	if p.Stage2 == nil {
		return nil
	}
	if err := p.waitAC(ctx, PhaseTools); err != nil {
		return err
	}
	p.emit(PrepareEvent{Phase: PhaseTools})
	tr, err := p.Stage2.Run(ctx, Stage2Input{Spec: spec, Dir: p.base, ControlCheck: true})
	if tr.ToolsDial != "" {
		p.emit(PrepareEvent{Phase: PhaseTools, ToolsDial: tr.ToolsDial})
	}
	if err != nil {
		return fmt.Errorf("macvm: stage 2: %w", err)
	}
	if tr.AgentVersion != "" {
		res.AgentVersion = tr.AgentVersion
	}
	if tr.Desktop != "" {
		res.Desktop = tr.Desktop
	}
	res.Harnesses = tr.Harnesses
	return writeToolsRecord(p.base, tr, res.AgentVersion, time.Now())
}

// finishedBase says whether dir holds a base whose install finished, in any
// format: no install marker, and a manifest that parses and says ready.
func finishedBase(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, installingMarker)); !os.IsNotExist(err) {
		return false
	}
	m, err := ReadManifest(dir)
	return err == nil && m.State == BaseReady
}

// discardInterrupted removes a base whose install started but never reached a
// ready manifest (the marker is still there, or the files exist without a
// ready manifest). The cached restore image is kept.
func (p *prep) discardInterrupted() error {
	_, markerErr := os.Stat(filepath.Join(p.base, installingMarker))
	_, readyErr := ReadyBase(p.base)
	_, diskErr := os.Stat(filepath.Join(p.base, DiskFile))
	if markerErr != nil && (readyErr == nil || diskErr != nil) {
		return nil
	}
	p.Log.Warn("macvm: discarding an install that did not finish", "base", p.base)
	for _, f := range []string{DiskFile, AuxFile, MachineIDFile, ManifestFile, ToolsRecordFile, installingMarker} {
		if err := os.Remove(filepath.Join(p.base, f)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	p.removeSetupCode()
	return nil
}

// restoreImage returns a validated restore image in the cache: reused if
// already complete, otherwise downloaded (resuming a partial file). An image
// that fails validation is deleted and downloaded again, once.
func (p *prep) restoreImage(ctx context.Context) (RestoreImage, string, string, error) {
	u, err := p.Engine.LatestRestoreImageURL(ctx)
	if err != nil {
		return RestoreImage{}, "", "", fmt.Errorf("macvm: latest restore image: %w", err)
	}
	name, err := ipswName(u)
	if err != nil {
		return RestoreImage{}, "", "", err
	}
	final := filepath.Join(p.cache, name)
	for attempt := 0; ; attempt++ {
		if _, err := os.Stat(final); err != nil {
			if err := p.download(ctx, u, final); err != nil {
				return RestoreImage{}, "", "", err
			}
		}
		if err := p.waitAC(ctx, PhaseValidate); err != nil {
			return RestoreImage{}, "", "", err
		}
		p.emit(PrepareEvent{Phase: PhaseValidate})
		img, err := p.Engine.LoadRestoreImage(final)
		if err == nil {
			// The first validate event is written before the load, so it cannot say what the image is.
			var size int64
			if fi, serr := os.Stat(final); serr == nil {
				size = fi.Size()
			}
			p.emit(PrepareEvent{Phase: PhaseValidate, Total: size, Version: img.OSVersion})
			sha, herr := fileSHA256(final)
			if herr != nil {
				return RestoreImage{}, "", "", herr
			}
			return img, sha, final, nil
		}
		_ = os.Remove(final)
		if attempt >= 1 {
			return RestoreImage{}, "", "", fmt.Errorf("macvm: the restore image failed validation twice: %w", err)
		}
		p.Log.Warn("macvm: restore image failed validation; downloading again", "err", err.Error())
	}
}

func (p *prep) download(ctx context.Context, u, final string) error {
	var start time.Time
	var startBytes int64 = -1
	d := &downloader{
		client: p.HTTP, power: p.Power, poll: p.PowerPoll, rng: p.DownloadRange,
		paused: func(on bool) {
			ev := PrepareEvent{Phase: PhaseDownload, Paused: on}
			if on {
				ev.Detail = "waiting for AC power"
			}
			p.emit(ev)
		},
		progress: func(done, total int64) {
			ev := PrepareEvent{Phase: PhaseDownload, Bytes: done, Total: total}
			// The rate counts the bytes after the first event, over the time since it: the first event
			// has no earlier one, and a resumed download's bytes fetched before this run are not its speed.
			if startBytes < 0 {
				startBytes, start = done, time.Now()
			} else if s := time.Since(start).Seconds(); s > 0 {
				ev.BytesPerSec = float64(done-startBytes) / s
			}
			if total > 0 {
				ev.Fraction = float64(done) / float64(total)
			}
			p.emit(ev)
		},
	}
	d.retrying = func(off, total int64) {
		p.emit(PrepareEvent{Phase: PhaseDownload, Retry: true, Bytes: off, Total: max(total, 0)})
	}
	p.emit(PrepareEvent{Phase: PhaseDownload, Version: ipswVersion(path.Base(final))})
	part := final + ".part"
	if err := d.fetch(ctx, u, part); err != nil {
		return err
	}
	return os.Rename(part, final)
}

// waitAC waits for AC power before a phase that must not start on battery.
func (p *prep) waitAC(ctx context.Context, phase string) error {
	d := &downloader{power: p.Power, poll: p.PowerPoll, paused: func(on bool) {
		ev := PrepareEvent{Phase: phase, Paused: on}
		if on {
			ev.Detail = "waiting for AC power"
		}
		p.emit(ev)
	}}
	return d.waitAC(ctx)
}

// create writes the base's aux storage, its own machine identifier and a
// sparse disk.
func (p *prep) create(img RestoreImage) ([]byte, error) {
	if err := p.Engine.CreateAuxStorage(filepath.Join(p.base, AuxFile), img.HardwareModel); err != nil {
		return nil, fmt.Errorf("macvm: aux storage: %w", err)
	}
	id, err := p.Engine.NewMachineIdentifier()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(p.base, MachineIDFile), id, 0o600); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(p.base, DiskFile), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(p.DiskBytes); err != nil {
		_ = f.Close()
		return nil, err
	}
	return id, f.Close()
}

// install runs the installer to completion. Power is not consulted: the
// installer is never paused. If ctx ends (the process or the Mac is going
// away), the marker stays and the next Prepare discards the partial disk.
func (p *prep) install(ctx context.Context, spec MachineSpec, ipsw string) error {
	if err := p.waitAC(ctx, PhaseInstall); err != nil {
		return err
	}
	p.emit(PrepareEvent{Phase: PhaseInstall})
	progress := func(f float64) { p.emit(PrepareEvent{Phase: PhaseInstall, Fraction: f}) }
	var err error
	if len(p.InstallProcess) > 0 {
		err = installInProcess(ctx, p.InstallProcess, spec, ipsw, progress)
	} else {
		err = installVM(ctx, p.Engine, spec, ipsw, progress)
	}
	if err != nil {
		return fmt.Errorf(InstallFailurePrefix+"%w", err)
	}
	return nil
}

// auxLockRetries and auxLockBackoff bound firstBoot's retry on the aux lock
// refusal. It is a backstop, not the release: the install process has exited
// by then (InstallProcess), but the framework's own per-VM process may take a
// moment to let go of the aux storage, with no signal to wait on.
var (
	auxLockRetries = 5
	auxLockBackoff = 2 * time.Second
)

// auxLocked says err is the framework refusing a VM whose auxiliary storage
// another VM still locks. The framework says so only in its description.
func auxLocked(err error) bool {
	return strings.Contains(err.Error(), "Failed to lock auxiliary storage")
}

// firstBoot starts the first boot, retrying a short while on exactly the aux
// lock refusal.
func (p *prep) firstBoot(ctx context.Context, spec MachineSpec, nic *os.File, user, pass string) (FirstBoot, error) {
	for try := 0; ; try++ {
		fb, err := p.FirstBoot.Boot(spec.DiskPath, spec.AuxPath, spec.HardwareModel, spec.MachineIdentifier,
			uint(spec.CPUs), spec.MemoryBytes, nic, user, pass)
		if err == nil || try == auxLockRetries || !auxLocked(err) {
			return fb, err
		}
		p.Log.Warn("macvm: the first boot's aux storage is still locked; retrying", "err", err.Error())
		select {
		case <-ctx.Done():
			return FirstBoot{}, err
		case <-time.After(auxLockBackoff):
		}
	}
}

// provisionAndBootstrap runs the one first boot, waits for the guest's Remote
// Login (provisioned, or by hand), runs stage 1 and shuts the VM down.
func (p *prep) provisionAndBootstrap(ctx context.Context, spec MachineSpec) (BootstrapResult, error) {
	if p.PrepNet == nil || p.Bootstrap == nil {
		return BootstrapResult{}, errors.New("macvm: prepare needs its prep network and stage 1")
	}
	nw, err := p.PrepNet()
	if err != nil {
		return BootstrapResult{}, err
	}
	defer nw.Close()
	guest, host, err := NewNetPair()
	if err != nil {
		return BootstrapResult{}, err
	}
	defer guest.Close()
	defer host.Close()
	go func() { _ = nw.Serve(host) }()
	password, err := randomPassword()
	if err != nil {
		return BootstrapResult{}, err
	}
	if err := p.waitAC(ctx, PhaseProvision); err != nil {
		return BootstrapResult{}, err
	}
	p.emit(PrepareEvent{Phase: PhaseProvision})
	user, pass := provisionUser, password
	if p.byHand {
		user, pass = "", "" // a plain boot: Setup Assistant
	}
	fb, err := p.firstBoot(ctx, spec, guest, user, pass)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("macvm: provisioning boot: %w", err)
	}
	stopped := false
	shutdown := func() error {
		sctx, cancel := context.WithTimeout(context.Background(), DefaultStopTimeout)
		defer cancel()
		stopped = true
		return fb.Stop(sctx)
	}
	defer func() {
		if !stopped {
			_ = shutdown()
		}
	}()
	// done is watched while waiting: a guest that stops by itself is a failure.
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	early := make(chan error, 1)
	var vmStopped atomic.Bool
	go func() {
		select {
		case err := <-fb.Done:
			vmStopped.Store(true)
			early <- err
			wcancel()
		case <-wctx.Done():
		}
	}()
	login, replace := password, ""
	err = ErrManualSetup // a plain boot waits for no provisioned Remote Login
	if !p.byHand {
		err = waitPort(wctx, nw, 22, p.RemoteLoginWait)
	}
	if errors.Is(err, ErrManualSetup) && p.SetupByHandWait > 0 {
		// The code goes with this prepare: replaced by stage 1, or useless
		// once the base it was typed into is discarded.
		defer p.removeSetupCode()
		var code string
		if code, err = p.setupByHand(wctx, nw, fb.Screen); err == nil {
			login, replace = code, password
		}
	}
	if err != nil {
		select {
		case e := <-early:
			stopped = true
			return BootstrapResult{}, fmt.Errorf("macvm: the provisioning boot stopped early: %v", e)
		default:
		}
		return BootstrapResult{}, err
	}
	if err := p.waitAC(wctx, PhaseBootstrap); err != nil {
		return BootstrapResult{}, err
	}
	p.emit(PrepareEvent{Phase: PhaseBootstrap})
	in := BootstrapInput{Dial: nw.DialGuest, Username: provisionUser, Password: login, Progress: p.emit,
		GuestEpoch: func() (uint64, error) {
			if vmStopped.Load() {
				return 0, ErrGuestStopped
			}
			return nw.DHCPRequests(), nil
		}}
	if replace != "" {
		in.NewPassword, in.Replaced = replace, p.removeSetupCode
	}
	res, err := p.Bootstrap.Bootstrap(wctx, in)
	if err != nil {
		return BootstrapResult{}, fmt.Errorf("macvm: stage 1: %w", err)
	}
	if res.Desktop == "" {
		res.Desktop = DesktopNone
	}
	p.emit(PrepareEvent{Phase: PhaseShutdown})
	wcancel()
	if err := shutdown(); err != nil {
		return BootstrapResult{}, fmt.Errorf("macvm: the prepared VM did not shut down cleanly: %w", err)
	}
	return res, nil
}

// waitPort dials the guest's port until it answers or wait passes. Only the
// wait passing means macOS wants setup by hand; prepare being cancelled
// returns the cancellation.
func waitPort(ctx context.Context, nw PrepNetwork, port int, wait time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		dctx, dcancel := context.WithTimeout(wctx, 5*time.Second)
		c, err := nw.DialGuest(dctx, port)
		dcancel()
		if err == nil {
			_ = c.Close()
			return nil
		}
		select {
		case <-wctx.Done():
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrManualSetup
		case <-time.After(time.Second):
		}
	}
}

// randomPassword is 32 random bytes, base64: the provisioned admin's password,
// held in memory only.
func randomPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ipswNameVersion matches the restore image names Apple publishes: UniversalMac_<version>_<build>_Restore.ipsw.
var ipswNameVersion = regexp.MustCompile(`^UniversalMac_(\d+(?:\.\d+){0,2})_[^_]+_Restore\.ipsw$`)

// ipswVersion is the macOS version in a restore image's file name, or "" when the name has none.
func ipswVersion(name string) string {
	if m := ipswNameVersion.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return ""
}

// ipswName is the cache file name for a restore image URL.
func ipswName(u string) (string, error) {
	pu, err := url.Parse(u)
	if err != nil {
		return "", fmt.Errorf("macvm: restore image URL: %w", err)
	}
	name := path.Base(pu.Path)
	if !strings.HasSuffix(name, ".ipsw") || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("macvm: restore image URL %q does not name an .ipsw", u)
	}
	return name, nil
}
