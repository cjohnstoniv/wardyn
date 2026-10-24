// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// macOS VM sandboxes on a Mac (machine_kind macos_vm): the runner's side of the host admission ledger,
// the base image and the caps it offers. The VMs themselves are the Docker driver's (EnableMacVM). Off
// unless the operator turns them on: WARDYN_RUNNER_MACOS_VM=1, the managed preference MacOSVM, or
// `wardyn-runnerd setup --macos-vm on`, all of which land in the macos_vm setting.

// macOSVMRecompute is how often the runner recomputes the max it offers, which is how another login's
// VMs reach this runner's offer: the ledger has no change notification. It is the ledger's own interval.
const macOSVMRecompute = admission.OfferRecompute

// The free-space check (design 3.10): every 30 s, a Mac whose VM volume has under 5 GiB free ends this
// runner's newest macOS VM run. Best effort against other programs' writes: the ledger's reservations
// bound Wardyn's own VMs.
const (
	macOSVMDiskEvery = 30 * time.Second
	macOSVMDiskFloor = 5 << 30
)

// The refusals the runner's caps carry when it offers no macOS VM.
const (
	macOSVMLedgerDamaged = "This Mac's macOS VM ledger is missing or damaged. Reinstall the Wardyn runner package."
	macOSVMNoHelper      = "This Mac's Wardyn runner has no macOS VM helper. Reinstall the Wardyn runner package."
)

// macOSVMs is this runner's handle on the Mac's macOS VMs: the ledger, the base, and the caps last built.
type macOSVMs struct {
	ledger   *admission.Ledger // nil when refusal is set
	host     admission.Host
	layout   macvm.Layout
	runnerID string
	// refusal, when set, is why this runner offers no macOS VM at all.
	refusal string
	// version is the guest agent this runner ships; a base whose agent differs needs a tools update.
	version string
	// toolsUpdate starts that update (design 3.4, guest agent drift); nil only says so in the log.
	toolsUpdate func(have, want string)
	// reclaim reconciles this runner's run entries whose VM and run directory are gone (the driver's).
	reclaim func() error
	// endNewest ends this runner's newest macOS VM run (the driver's), for the free-space check.
	endNewest func(context.Context) (string, error)
	interval  time.Duration
	kick      chan struct{}

	mu    sync.Mutex
	caps  *runnerwire.MacVMCaps
	drift string // the base agent version a tools update was last asked for
}

// macOSVMSetup is what openMacOSVMs reads.
type macOSVMSetup struct {
	ledgerDir, root, runnerID, version string
	uid                                int
	dockerMemory                       uint64
	host                               admission.Host
	owner                              func(os.FileInfo) (int, bool)
	// fstat replaces fstat(2) in the ledger's root-ownership checks (tests); nil is the real one.
	fstat func(fd int, name string) (unix.Stat_t, error)
}

// openMacOSVMs opens the ledger and the VM directory. A ledger the package did not install whole (the
// ledger itself checks the directory, its ancestors and its lock are root's, and creates or repairs
// none of them) makes a handle that offers no macOS VM and names the fix.
func openMacOSVMs(s macOSVMSetup) *macOSVMs {
	m := &macOSVMs{host: s.host, layout: macvm.Layout{Root: s.root}, runnerID: s.runnerID, version: s.version, interval: macOSVMRecompute, kick: make(chan struct{}, 1)}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		slog.Warn("macOS VM sandboxes are off on this runner: no VM directory", "error", err.Error())
		m.refusal = macOSVMLedgerDamaged
		return m
	}
	l, err := admission.Open(admission.Config{Dir: s.ledgerDir, UID: s.uid, RunnerID: s.runnerID, Host: s.host,
		DockerMemoryBytes: s.dockerMemory, Volume: s.root, Fstat: s.fstat, FileOwner: func(fi os.FileInfo) int {
			uid, ok := s.owner(fi)
			if !ok {
				return -1
			}
			return uid
		}})
	if err != nil {
		slog.Warn("macOS VM sandboxes are off on this runner: the ledger is missing or damaged", "error", err.Error())
		m.refusal = macOSVMLedgerDamaged
		return m
	}
	m.ledger = l
	return m
}

// changed asks for a recompute now (after one of this runner's own ledger transactions).
func (m *macOSVMs) changed() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// enabled says whether this runner starts macOS VMs at all (a create_vm for one is refused otherwise).
func (m *macOSVMs) enabled() bool { return m != nil && m.ledger != nil }

// current is the caps to send.
func (m *macOSVMs) current() *runnerwire.MacVMCaps {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.caps == nil {
		return nil
	}
	c := *m.caps
	return &c
}

// run recomputes every interval and on every kick until ctx ends, and calls resend whenever the caps
// changed, so the organisation's copy follows this runner's max (and its base) within one interval.
func (m *macOSVMs) run(ctx context.Context, resend func()) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.kick:
		}
		if m.recompute() {
			resend()
		}
	}
}

// watchDisk is the free-space check until ctx ends: free reads the VM volume's free bytes, and end ends
// this runner's newest macOS VM run (the driver's EndNewestMacVM), one per check while space stays low.
func (m *macOSVMs) watchDisk(ctx context.Context, every time.Duration, free func() (int64, error), end func(context.Context) (string, error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		n, err := free()
		if err != nil {
			slog.Warn("macOS VM free-space check", "error", err.Error())
			continue
		}
		if n >= macOSVMDiskFloor {
			continue
		}
		run, err := end(ctx)
		switch {
		case err != nil:
			slog.Warn("macOS VM free-space check: end the newest macOS VM run", "run_id", run, "free_bytes", n, "error", err.Error())
		case run != "":
			slog.Warn("This Mac ran low on disk: ended its newest macOS VM run", "run_id", run, "free_bytes", n)
		}
	}
}

// recompute builds the caps and reports whether they differ from the last ones.
func (m *macOSVMs) recompute() bool {
	m.mu.Lock()
	last := m.caps
	m.mu.Unlock()
	c := m.build(last)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.caps != nil && reflect.DeepEqual(*m.caps, c) {
		return false
	}
	m.caps = &c
	return true
}

// build is one pass: the tombstone pass first (the ledger's Sweep), so another login's dead VM stops
// holding a slot, then this runner's own reconciles, then the offer (own live runs plus the host's
// headroom) and the base. A ledger read that fails keeps the last max, as the organisation already has it.
func (m *macOSVMs) build(last *runnerwire.MacVMCaps) runnerwire.MacVMCaps {
	c := runnerwire.MacVMCaps{SignIns: true, Base: runnerwire.MacVMBase{State: runnerwire.MacVMBaseNone}}
	if m.ledger == nil {
		c.Refusal = m.refusal
		return c
	}
	if _, err := m.ledger.Sweep(); err != nil {
		slog.Warn("macOS VM ledger: tombstone pass", "error", err.Error())
	}
	m.reconcilePrepare()
	if m.reclaim != nil {
		if err := m.reclaim(); err != nil {
			slog.Warn("macOS VM ledger: reconcile", "error", err.Error())
		}
	}
	man, base := m.base()
	c.Base = base
	if base.State == runnerwire.MacVMBaseReady {
		c.MinCPUs, c.MinMemoryMiB = man.MinCPUs, int(man.MinMemoryMiB)
		c.Harnesses, c.Desktop = slices.Clone(man.Harnesses), man.Desktop
		m.checkAgent(man)
	}
	off, err := m.ledger.Offer(man.MinMemoryMiB<<20, admission.RunDiskReservation(cmp.Or(man.DiskBytes, macvm.BaseDiskBytes)))
	switch {
	case err == nil:
		c.Max = off.Max
	case last != nil:
		slog.Warn("macOS VM ledger: offer", "error", err.Error())
		c.Max = last.Max
	}
	return c
}

// base is the base image's state for the caps.
func (m *macOSVMs) base() (macvm.Manifest, runnerwire.MacVMBase) {
	man, err := macvm.ReadManifest(m.layout.BaseDir(0))
	b := runnerwire.MacVMBase{State: runnerwire.MacVMBaseNone, GuestOS: man.GuestOS}
	switch {
	case err == nil && man.Format != macvm.BaseFormat:
		b.State = runnerwire.MacVMBaseUpdateNeeded
	case err == nil && man.State == macvm.BaseReady:
		b.State = runnerwire.MacVMBaseReady
	case m.preparing():
		b.State, b.Progress = runnerwire.MacVMBasePreparing, prepareProgress(m.layout.PrepareLog())
	case err == nil && man.State == macvm.BaseUpdateNeeded:
		b.State = runnerwire.MacVMBaseUpdateNeeded
	case err == nil && man.State == macvm.BaseFailed:
		b.State = runnerwire.MacVMBaseFailed
	default:
		if msg := prepareFailure(m.layout.PrepareLog()); msg != "" {
			b.State, b.Error = runnerwire.MacVMBaseFailed, msg
		}
	}
	return man, b
}

// checkAgent is the guest agent drift trigger: a base whose agent is not this runner's needs a tools
// update, asked for once per base agent version. The base keeps serving meanwhile.
func (m *macOSVMs) checkAgent(man macvm.Manifest) {
	if man.AgentVersion == "" || man.AgentVersion == m.version {
		return
	}
	m.mu.Lock()
	asked := m.drift == man.AgentVersion
	m.drift = man.AgentVersion
	m.mu.Unlock()
	if asked {
		return
	}
	slog.Warn("macOS VM base: its guest agent is not this runner's; a tools update replaces it", "base_agent", man.AgentVersion, "runner", m.version)
	if m.toolsUpdate != nil {
		m.toolsUpdate(man.AgentVersion, m.version)
	}
}

// own is this runner's counted entries of kind k.
func (m *macOSVMs) own(k admission.Kind) []admission.Entry {
	counted, err := m.ledger.Counted()
	if err != nil {
		return nil
	}
	var out []admission.Entry
	for _, e := range counted {
		if e.Kind == k && m.mine(e) {
			out = append(out, e)
		}
	}
	return out
}

func (m *macOSVMs) mine(e admission.Entry) bool { return e.RunnerID == m.runnerID }

// preparing reports a live prepare of this runner's.
func (m *macOSVMs) preparing() bool {
	for _, e := range m.own(admission.KindPrepare) {
		if !m.vmGone(e) {
			return true
		}
	}
	return false
}

// vmGone: nothing can still start or run e's VM.
func (m *macOSVMs) vmGone(e admission.Entry) bool {
	pid, start := e.HelperPID, e.HelperStart
	if pid == 0 {
		pid, start = e.OwnerPID, e.OwnerStart
	}
	got, err := admission.Self(m.host, pid)
	return err != nil || got != start
}

// reconcilePrepare releases this runner's prepare entries whose helper has exited: prepare has no run
// directory, and the ledger itself refuses while a VM process of this user could still be its VM.
func (m *macOSVMs) reconcilePrepare() {
	for _, e := range m.own(admission.KindPrepare) {
		if !m.vmGone(e) {
			continue
		}
		if err := m.ledger.ReclaimOwn(e.ID, ""); err != nil {
			slog.Warn("macOS VM ledger: release a finished prepare", "error", err.Error())
		}
	}
}

// prepareOrder is the helper's prepare phases in order, for an overall percentage.
var prepareOrder = []string{macvm.PhaseCheck, macvm.PhaseDownload, macvm.PhaseValidate, macvm.PhaseCreate,
	macvm.PhaseInstall, macvm.PhaseProvision, macvm.PhaseBootstrap, macvm.PhaseShutdown}

// lastLine is the last non-empty line of the file at path (prepare.log is small: progress lines only).
func lastLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var last string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			last = l
		}
	}
	return last
}

// prepareProgress is the overall percentage of the last progress line in prepare.log.
func prepareProgress(log string) int {
	var ev macvm.PrepareEvent
	if json.Unmarshal([]byte(lastLine(log)), &ev) != nil {
		return 0
	}
	i := slices.Index(prepareOrder, ev.Phase)
	if i < 0 {
		return 0
	}
	return min(int((float64(i)+min(max(ev.Fraction, 0), 1))*100/float64(len(prepareOrder))), 99)
}

// prepareFailure is the helper's error when its last prepare ended with one: the helper writes it, after
// its progress lines, as "wardyn-macvm: <error>".
func prepareFailure(log string) string {
	msg, ok := strings.CutPrefix(lastLine(log), "wardyn-macvm: ")
	if !ok {
		return ""
	}
	return msg
}

// phaseSetupByHand is the helper's prepare event when the guest's first boot asked for Setup Assistant;
// its detail is the one-time code the account is created with.
const phaseSetupByHand = "setup_by_hand"

// setupByHandCode is that code when prepare's last event asks for setup by hand, else "".
func setupByHandCode(log string) string {
	var ev macvm.PrepareEvent
	if json.Unmarshal([]byte(lastLine(log)), &ev) != nil || ev.Phase != phaseSetupByHand {
		return ""
	}
	return ev.Detail
}

// latestFile, in the VM directory, is the helper's last read of Apple's newest supported restore image.
const latestFile = "latest.json"

// newerMacOS names Apple's newer macOS when the helper found one the base is not built from, else "".
func newerMacOS(root string, man macvm.Manifest) string {
	b, err := os.ReadFile(filepath.Join(root, latestFile))
	if err != nil {
		return ""
	}
	var latest struct {
		GuestOS      string `json:"guest_os"`
		BuildVersion string `json:"build_version"`
	}
	if json.Unmarshal(b, &latest) != nil || latest.BuildVersion == "" || latest.BuildVersion == man.BuildVersion {
		return ""
	}
	return cmp.Or(latest.GuestOS, latest.BuildVersion)
}

// desktopMode is the base's desktop in status's words.
func desktopMode(mode string) string {
	switch mode {
	case "", "none":
		return "none"
	case "observe":
		return "view only (macOS turned Screen Sharing on for viewing only)"
	}
	return mode
}

// macOSVMArithmetic is setup's worked sum (design 3.10): a runner's macOS guests together get at most
// its total memory, which cannot pass Docker's, while the host's budget for them is what Docker and
// macOS leave. Bytes throughout.
type macOSVMArithmetic struct {
	host, docker, total, perRun uint64
}

// max is how many macOS VMs this Mac can run at once at the per-run memory: Apple's cap, the runner's
// total and the host budget, whichever holds fewest.
func (a macOSVMArithmetic) max() int {
	if a.perRun == 0 {
		return 0
	}
	n := min(admission.LicenceCap, int(a.total/a.perRun))
	return min(n, int(a.budget()/(a.perRun+admission.DefaultOverheadBytes)))
}

func (a macOSVMArithmetic) budget() uint64 {
	if a.host <= a.docker+admission.MacOSReserveBytes {
		return 0
	}
	return a.host - a.docker - admission.MacOSReserveBytes
}

// lines are setup's words for the sum, with this Mac's numbers, and what to change when it holds none.
func (a macOSVMArithmetic) lines() []string {
	gb := func(b uint64) string { return strconv.FormatFloat(float64(b)/(1<<30), 'f', -1, 64) }
	out := []string{
		fmt.Sprintf("macOS VMs on this Mac: %s GB of memory, less %s GB for Docker and 4 GB for macOS, leaves %s GB for macOS guests.", gb(a.host), gb(a.docker), gb(a.budget())),
		fmt.Sprintf("Each guest takes the per-run memory (%s GB) and about 1 GB more, and the runner's total memory (%s GB) holds all of them at once.", gb(a.perRun), gb(a.total)),
		fmt.Sprintf("So this Mac runs at most %d macOS VM(s) at once (Apple's licence allows 2).", a.max()),
	}
	if a.max() == 0 {
		out = append(out, "To run one: give Docker less memory, or lower --per-run-memory (a guest needs at least the macOS base's minimum, which prepare reports), or raise --total-memory up to what Docker has.")
	}
	return out
}

// excludeFromBackups runs `tmutil addexclusion` on Wardyn's own VM directory: a sticky exclusion that
// follows the directory, so Time Machine does not copy bases and run disks to a backup disk. It touches
// no other backup setting. (The .noindex suffix already keeps Spotlight out.)
var excludeFromBackups = func(dir string) error {
	if _, err := os.Stat("/usr/bin/tmutil"); err != nil {
		return nil // not a Mac
	}
	out, err := exec.Command("/usr/bin/tmutil", "addexclusion", dir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmutil addexclusion: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// prepareArgs is what startPrepare runs the helper's prepare with.
type prepareArgs struct {
	helper, runnerID, ledgerDir string
	cpus                        int
	memoryMiB                   uint64
	keepIPSW                    bool
}

// startPrepare takes prepare's ledger entry (the tombstone pass first) and starts the helper's prepare in
// its own session, its output appended to prepare.log, then waits until the helper has claimed the
// entry, so the entry never outlives both. The helper releases nothing: once it exits, the runner's
// recompute reconciles the entry.
func (m *macOSVMs) startPrepare(ctx context.Context, a prepareArgs) error {
	if m.ledger == nil {
		return errors.New(m.refusal)
	}
	if err := excludeFromBackups(m.layout.Root); err != nil {
		slog.Warn("macOS VM directory: Time Machine exclusion", "error", err.Error())
	}
	if _, err := m.ledger.Sweep(); err != nil {
		return err
	}
	self := os.Getpid()
	start, err := admission.Self(m.host, self)
	if err != nil {
		return err
	}
	e, err := m.ledger.Admit(admission.Request{Kind: admission.KindPrepare, GuestCPUs: a.cpus, GuestMemory: a.memoryMiB << 20,
		DiskReserve: admission.PrepareDiskReservation(macvm.BaseDiskBytes), OwnerPID: self, OwnerStart: start})
	if err != nil {
		return err
	}
	never := func(err error) error {
		// The VM never started: the entry goes at once.
		return errors.Join(err, m.ledger.Release(e.ID, admission.Destroyed{Stopped: true}))
	}
	logf, err := os.OpenFile(m.layout.PrepareLog(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return never(err)
	}
	defer logf.Close()
	args := []string{"prepare", "--root", m.layout.Root, "--cpus", strconv.Itoa(a.cpus), "--memory-mib", strconv.FormatUint(a.memoryMiB, 10),
		"--runner-id", a.runnerID, "--ledger-entry", e.ID}
	if a.ledgerDir != "" {
		args = append(args, "--ledger-dir", a.ledgerDir)
	}
	if a.keepIPSW {
		args = append(args, "--keep-ipsw")
	}
	cmd := exec.Command(a.helper, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return never(fmt.Errorf("start the macOS VM helper: %w", err))
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	m.changed()
	// A helper that never claimed its entry never built a VM (it claims first), so the entry goes.
	for deadline := time.Now().Add(prepareClaimWait); ; {
		if counted, err := m.ledger.Counted(); err == nil {
			i := slices.IndexFunc(counted, func(x admission.Entry) bool { return x.ID == e.ID })
			if i >= 0 && counted[i].HelperPID != 0 {
				return nil
			}
		}
		select {
		case <-exited:
			if err := m.claimed(e.ID); err != nil {
				return err
			}
			return never(errors.New("the macOS VM helper's prepare ended at once; see " + m.layout.PrepareLog()))
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			_ = cmd.Process.Kill()
			<-exited
			if err := m.claimed(e.ID); err != nil {
				return err
			}
			return never(errors.New("the macOS VM helper did not take its ledger entry; see " + m.layout.PrepareLog()))
		}
	}
}

// prepareClaimWait bounds startPrepare's wait for the helper to claim its entry.
var prepareClaimWait = time.Minute

// claimed is nil when entry id is not claimed by a helper (the entry may go), else why it stays: a helper
// that claimed it is the VM's, and the recompute reconciles it once that helper has gone.
func (m *macOSVMs) claimed(id string) error {
	counted, err := m.ledger.Counted()
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(counted, func(x admission.Entry) bool { return x.ID == id }); i >= 0 && counted[i].HelperPID != 0 {
		return errors.New("the macOS VM helper's prepare ended; see " + m.layout.PrepareLog())
	}
	return nil
}

// macVMHelperPath is the helper beside this wardyn-runnerd (the package installs both in one directory).
func macVMHelperPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return filepath.Join(filepath.Dir(exe), "wardyn-macvm"), nil
}

// macHostOS is this Mac's build in the form a VM machine records, e.g. "macOS 27.0.1 (26A434)".
func macHostOS() string {
	v, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return ""
	}
	b, _ := exec.Command("/usr/bin/sw_vers", "-buildVersion").Output()
	out := "macOS " + strings.TrimSpace(string(v))
	if s := strings.TrimSpace(string(b)); s != "" {
		out += " (" + s + ")"
	}
	return out
}

// newMacOSVMCommand is `wardyn-runnerd macos-vm`: this Mac's macOS VM state, its base's preparation, and
// the base's removal.
func newMacOSVMCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "macos-vm", Short: "Show, prepare or remove this Mac's macOS VM base", Args: cobra.NoArgs}
	open := func(cmd *cobra.Command) (*macOSVMs, string, error) {
		cfg, err := serveConfig(cmd, "")
		if err != nil {
			return nil, "", err
		}
		stateDir := cfg.get("state_dir")
		if stateDir == "" {
			if stateDir, err = runneridentity.DefaultDir(); err != nil {
				return nil, "", err
			}
		}
		id, err := runneridentity.Load(stateDir, cfg.get("org_url"))
		if err != nil {
			return nil, "", err
		}
		root, err := macvm.DefaultRoot()
		if err != nil {
			return nil, "", err
		}
		m := openMacOSVMs(macOSVMSetup{ledgerDir: admission.DefaultDir, root: root, runnerID: id.RunnerID.String(), uid: os.Getuid(),
			host: admission.SystemHost(), owner: fileOwner})
		if !macOSVMOn(cfg.get("macos_vm")) {
			fmt.Fprintln(cmd.OutOrStdout(), "macOS VM sandboxes are off on this runner (setting macos_vm, from "+cfg.field("macos_vm").Source+").")
		}
		return m, id.RunnerID.String(), nil
	}
	status := &cobra.Command{Use: "status", Short: "Show this Mac's macOS VM base, ledger and offer", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		m, _, err := open(cmd)
		if err != nil {
			return err
		}
		return m.writeStatus(cmd.OutOrStdout())
	}}
	var cpu float64
	var memGB int64
	var keep bool
	prepare := &cobra.Command{Use: "prepare", Short: "Build this Mac's macOS base image from Apple's restore image (hours; it continues in the background)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cpu < 1 || memGB < 1 {
				return errors.New("pass --per-run-cpu and --per-run-memory: the runner's per-run limits, which prepare's own VM uses and never raises")
			}
			m, runnerID, err := open(cmd)
			if err != nil {
				return err
			}
			helper, err := macVMHelperPath()
			if err != nil {
				return err
			}
			if err := m.startPrepare(cmd.Context(), prepareArgs{helper: helper, runnerID: runnerID, cpus: int(cpu), memoryMiB: uint64(memGB) << 10, keepIPSW: keep}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Preparing this Mac's macOS base in the background; progress is in %s and in `wardyn-runnerd macos-vm status`.\n", m.layout.PrepareLog())
			return nil
		}}
	prepare.Flags().Float64Var(&cpu, "per-run-cpu", 0, "the runner's per-run CPUs")
	prepare.Flags().Int64Var(&memGB, "per-run-memory", 0, "the runner's per-run memory in GB")
	prepare.Flags().BoolVar(&keep, "keep-ipsw", false, "keep Apple's restore image after the base is ready")
	remove := &cobra.Command{Use: "remove", Short: "Delete this Mac's macOS base images and download cache", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		m, _, err := open(cmd)
		if err != nil {
			return err
		}
		return m.remove(cmd.OutOrStdout())
	}}
	cmd.AddCommand(status, prepare, remove)
	return cmd
}

// writeStatus prints the ledger, the base and the offer, for `macos-vm status`.
func (m *macOSVMs) writeStatus(out io.Writer) error {
	if m.ledger == nil {
		fmt.Fprintln(out, m.refusal)
		return nil
	}
	m.recompute()
	c := m.current()
	fmt.Fprintf(out, "base: %s", c.Base.State)
	if c.Base.GuestOS != "" {
		fmt.Fprintf(out, ", %s", c.Base.GuestOS)
	}
	if c.Base.State == runnerwire.MacVMBasePreparing {
		fmt.Fprintf(out, ", %d%%", c.Base.Progress)
	}
	if c.Base.Error != "" {
		fmt.Fprintf(out, ": %s", c.Base.Error)
	}
	fmt.Fprintln(out)
	if code := setupByHandCode(m.layout.PrepareLog()); code != "" {
		// Shown here, never logged: the account's password until the helper's first login replaces it.
		fmt.Fprintf(out, "macOS asked for setup by hand. Open the macOS window on this Mac, create the account wardyn-admin with the code %s as its password, and turn on Remote Login.\n", code)
	}
	if c.Base.State == runnerwire.MacVMBaseReady {
		fmt.Fprintf(out, "harnesses: %s; desktop: %s; guest minimum: %d CPUs, %d MiB\n", strings.Join(c.Harnesses, ", "), desktopMode(c.Desktop), c.MinCPUs, c.MinMemoryMiB)
		if man, err := macvm.ReadManifest(m.layout.BaseDir(0)); err == nil {
			if newer := newerMacOS(m.layout.Root, man); newer != "" {
				fmt.Fprintf(out, "A newer macOS is available (%s). Run `wardyn-runnerd macos-vm prepare` again to update this Mac's base.\n", newer)
			}
		}
	}
	counted, err := m.ledger.Counted()
	if err != nil {
		return err
	}
	debts, err := m.ledger.Debts()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "macOS VMs on this Mac (every login): %d; this runner offers %d at once\n", len(counted), c.Max)
	var held int64
	for _, e := range debts {
		held += e.DiskReserve
	}
	if held > 0 {
		fmt.Fprintf(out, "disk held for ended logins' VMs until they log in again: %d GB\n", held>>30)
	}
	return nil
}

// remove deletes the bases, the download cache and prepare's log, never while this runner holds a VM.
func (m *macOSVMs) remove(out io.Writer) error {
	if m.ledger != nil {
		counted, err := m.ledger.Counted()
		if err != nil {
			return err
		}
		if slices.ContainsFunc(counted, m.mine) {
			return errors.New("this runner has a macOS VM or a prepare running; end it first")
		}
	}
	// The ledger and the Time Machine exclusion stay: the package owns the one, and the other follows
	// the directory, which stays too.
	for _, p := range []string{filepath.Join(m.layout.Root, "bases"), m.layout.CacheDir()} {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "Removed this Mac's macOS base images and download cache from %s.\n", m.layout.Root)
	return nil
}

// macHostMemory is this Mac's memory (hw.memsize), swapped in tests.
var macHostMemory = func() (uint64, error) { return admission.SystemHost().MemoryBytes() }

// macOSVMHost reports an Apple silicon Mac, where macOS VMs run; swapped in tests.
var macOSVMHost = func(goos string) bool { return goos == "darwin" && runtime.GOARCH == "arm64" }

// checkMacOSVMFlag refuses a --macos-vm setup cannot honour, before any token is spent.
func checkMacOSVMFlag(v, goos string) error {
	switch {
	case v != "" && v != "on" && v != "off":
		return errors.New("--macos-vm is on or off")
	case v == "on" && !macOSVMHost(goos):
		return errors.New("--macos-vm on needs an Apple silicon Mac")
	}
	return nil
}

// askMacOSVM settles setup's macos_vm: the flag, else (on an Apple silicon Mac, at a terminal, with
// macOS VMs not yet on) the person's answer, else the earlier setting. When they are on, it shows this
// Mac's memory sum for the limits just registered.
func askMacOSVM(cmd *cobra.Command, goos string, plan *setupPlan, limits *types.RunnerLimits) error {
	if !macOSVMHost(goos) {
		return nil
	}
	out := cmd.OutOrStdout()
	if plan.macOSVM == "" && !macOSVMOn(plan.existing.MacOSVM) && asksAt(cmd.InOrStdin(), out) {
		fmt.Fprint(out, "Turn on macOS VM sandboxes on this Mac? Preparing their base downloads Apple's macOS (about 25 GB) and needs about 85 GB free. [y/N] ")
		line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		plan.macOSVM = "off"
		if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
			plan.macOSVM = "on"
		}
	}
	if !macOSVMOn(plan.runnerFile().MacOSVM) || limits == nil {
		return nil
	}
	a := macOSVMArithmetic{total: uint64(limits.Total.MemoryMiB) << 20, perRun: uint64(limits.PerRun.MemoryMiB) << 20}
	if m, err := measureHost(cmd.Context()); err == nil {
		a.docker = uint64(max(m.MemoryMiB, 0)) << 20
	}
	host, err := macHostMemory()
	if err != nil {
		return nil // the sum needs the Mac's memory; the ledger checks it at every start anyway
	}
	a.host = host
	for _, l := range a.lines() {
		fmt.Fprintln(out, l)
	}
	return nil
}

// prepareMacOSVMBase starts the base's preparation once the service runs, with the per-run limits just
// registered (prepare's VM never raises them). A rerun, whose limits setup does not know, or a base
// already ready, starts nothing. A prepare that cannot start is said, not fatal: the runner is set up.
func prepareMacOSVMBase(cmd *cobra.Command, plan setupPlan, limits *types.RunnerLimits) {
	out := cmd.OutOrStdout()
	root, err := macvm.DefaultRoot()
	if err != nil {
		fmt.Fprintf(out, "macOS VMs: %v\n", err)
		return
	}
	if _, err := macvm.ReadyBase(macvm.Layout{Root: root}.BaseDir(0)); err == nil {
		return
	}
	if limits == nil {
		fmt.Fprintln(out, "macOS VMs are on. To build this Mac's macOS base: wardyn-runnerd macos-vm prepare --per-run-cpu <CPUs> --per-run-memory <GB> (the runner's per-run limits).")
		return
	}
	id, err := runneridentity.Load(plan.paths.stateDir, plan.org)
	if err != nil {
		fmt.Fprintf(out, "macOS VMs: %v\n", err)
		return
	}
	m := openMacOSVMs(macOSVMSetup{ledgerDir: admission.DefaultDir, root: root, runnerID: id.RunnerID.String(), uid: os.Getuid(), host: admission.SystemHost(), owner: fileOwner})
	a := prepareArgs{helper: filepath.Join(filepath.Dir(plan.paths.installed), "wardyn-macvm"), runnerID: id.RunnerID.String(),
		cpus: int(limits.PerRun.CPUMillis / 1000), memoryMiB: uint64(limits.PerRun.MemoryMiB)}
	if err := m.startPrepare(cmd.Context(), a); err != nil {
		fmt.Fprintf(out, "macOS VMs are on, but this Mac's macOS base did not start preparing: %v\n", err)
		return
	}
	fmt.Fprintf(out, "Preparing this Mac's macOS base in the background (hours); progress is in %s and in `wardyn-runnerd macos-vm status`.\n", m.layout.PrepareLog())
}
