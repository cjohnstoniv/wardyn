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
	"math"
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

// macOSVMLedgerDamaged is the refusal the runner's caps carry, in words, when its ledger is missing or
// damaged. Every reason it offers no macOS VM, or can't start or prepare one now, also goes to its own
// organisation as a code with numbers (MacVMCaps.Reason and NoSession; MacVMBase.Step, Wait, Failure).
// Nothing sent names a person or an account; the base's error goes with this login's home as ~ and the
// uid in a socket path as <uid>. The full text, with paths, stays on this Mac, in the log and prepare.log.
const macOSVMLedgerDamaged = "This Mac's macOS VM ledger is missing or damaged. Reinstall the Wardyn runner package."

// macOSVMIdentity is the machine-identifier strategy every macOS VM run uses.
const macOSVMIdentity = macvm.IdentityFresh

// macOSVMs is this runner's handle on the Mac's macOS VMs: the ledger, the base, and the caps last built.
type macOSVMs struct {
	ledger   *admission.Ledger // nil when refusal is set
	host     admission.Host
	layout   macvm.Layout
	runnerID string
	// uid is this login's (os.Getuid()); ledgerDir is the ledger's directory when it is not the
	// package's, which a job's helper is then pointed at.
	uid       int
	ledgerDir string
	// refusal, when set, is the refusal the caps carry; why, when set, is why this runner offers no macOS
	// VM at all (either one turns them off).
	refusal, why string
	// reason, hostOSVersion and hostReason are the codes for why this runner offers no macOS VM at all
	// (MacVMCaps.Reason, HostOSVersion, HostReason); noSession and thermal are the session check's and
	// the probe's last answers (macos_vm_status.go). thermal is under mu; ticks counts run's ticks.
	reason, hostOSVersion, hostReason, thermal string
	noSession                                  bool
	ticks                                      int
	// helper is the wardyn-macvm beside this runner.
	helper string
	// perRunMemory is the runner's per-run memory limit in bytes, which every guest gets, and perRunCPUs
	// its per-run CPUs: the organisation's (setPerRunLimits), under mu. 0 is no limit known, and a guest
	// then gets the base's minimum. defaultPerRunMemory is setup's value, what no limit resets to.
	perRunMemory, defaultPerRunMemory uint64
	perRunCPUs                        int
	// sockRoot is the socket root ("" is admission.DefaultSockDir) and sockFstat replaces fstat(2) in its
	// checks (tests); prepNet brings up a base job's stage-2 network (the driver's StartMacPrepNet), and
	// prepSweep removes those a crash left, but the ones of the ledger entries it keeps (SweepMacPrep);
	// prepRemove removes one job's by its names (RemoveMacPrepNet).
	sockRoot   string
	sockFstat  func(fd int, name string) (unix.Stat_t, error)
	prepNet    func(ctx context.Context, entryID string) (*os.File, func(), error)
	prepSweep  func(ctx context.Context, keep []string) (int, error)
	prepRemove func(ctx context.Context, entryID string) error
	// dockerUp says Docker answers (the check a caps send fails on); nil is unknown. dockerWait marks
	// the jobs whose stage-2 network could not start while it did not (under mu): base() says wait docker.
	dockerUp   func(ctx context.Context) bool
	dockerWait map[string]bool
	// limitsFile, when set, is where setPerRunLimits saves the organisation's limits for `macos-vm prepare`;
	// limitsWarn logs a failed save once.
	limitsFile string
	limitsWarn sync.Once
	// version is the guest agent this runner ships; a base whose agent differs needs a tools update.
	version string
	// toolsUpdate starts that update (design 3.4, guest agent drift) for the base whose manifest it is
	// given; nil only says so in the log.
	toolsUpdate func(macvm.Manifest)
	// reclaim reconciles this runner's run entries whose VM and run directory are gone (the driver's).
	reclaim func() error
	// endNewest ends this runner's newest macOS VM run (the driver's), for the free-space check, given
	// the free bytes the check read.
	endNewest func(ctx context.Context, freeBytes int64) (string, error)
	interval  time.Duration
	kick      chan struct{}
	// ctx is the serve's (start), which the jobs' stage-2 networks live under; nets the jobs being handled.
	ctx  context.Context
	nets map[string]bool

	mu    sync.Mutex
	caps  *runnerwire.MacVMCaps
	drift string // the base agent version a tools update was last asked for
}

// macOSVMSetup is what openMacOSVMs reads.
type macOSVMSetup struct {
	ledgerDir, root, runnerID, version string
	limitsFile                         string
	uid                                int
	dockerMemory, perRunMemory         uint64
	host                               admission.Host
	owner                              func(os.FileInfo) (int, bool)
	// fstat replaces fstat(2) in the ledger's root-ownership checks (tests); nil is the real one.
	fstat func(fd int, name string) (unix.Stat_t, error)
}

// openMacOSVMs opens the ledger and the VM directory. A ledger the package did not install whole (the
// ledger itself checks the directory, its ancestors and its lock are root's, and creates or repairs
// none of them) makes a handle that offers no macOS VM and names the fix.
func openMacOSVMs(s macOSVMSetup) *macOSVMs {
	m := &macOSVMs{host: s.host, layout: macvm.Layout{Root: s.root}, runnerID: s.runnerID, uid: s.uid, version: s.version, perRunMemory: s.perRunMemory, limitsFile: s.limitsFile,
		defaultPerRunMemory: s.perRunMemory, interval: macOSVMRecompute, kick: make(chan struct{}, 1)}
	if s.ledgerDir != admission.DefaultDir {
		m.ledgerDir = s.ledgerDir
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		slog.Warn("macOS VM sandboxes are off on this runner: no VM directory", "error", err.Error())
		m.refusal, m.reason = macOSVMLedgerDamaged, runnerwire.MacVMReasonLedgerDamaged
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
		m.refusal, m.reason = macOSVMLedgerDamaged, runnerwire.MacVMReasonLedgerDamaged
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
			m.tick(ctx)
		case <-m.kick:
		}
		if m.recompute() {
			resend()
		}
	}
}

// start runs the recompute and, with the driver's end, the free-space check, until ctx ends; resend
// sends the caps after a change. It first sweeps the stage-2 networks a crash left, before watchJobs can
// hand any job one. A nil handle (macOS VMs off) starts nothing.
func (m *macOSVMs) start(ctx context.Context, resend func()) {
	if m == nil {
		return
	}
	m.sweepPrep(ctx)
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	if m.endNewest != nil {
		go m.watchDisk(ctx, macOSVMDiskEvery, func() (int64, error) { return m.host.FreeDiskBytes(m.layout.Root) }, m.endNewest)
	}
	go m.run(ctx, resend)
}

// watchDisk is the free-space check until ctx ends: free reads the VM volume's free bytes, and end ends
// this runner's newest macOS VM run (the driver's EndNewestMacVM), one per check while space stays low.
func (m *macOSVMs) watchDisk(ctx context.Context, every time.Duration, free func() (int64, error), end func(context.Context, int64) (string, error)) {
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
		run, err := end(ctx, n)
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
// holding a slot, then this runner's own reconciles, then the base and the offer (own live runs plus the
// host's headroom, for guests at the per-run memory, or the base's minimum when that is more). With no
// base ready it offers none. A ledger read that fails keeps the last max, as the organisation already
// has it.
func (m *macOSVMs) build(last *runnerwire.MacVMCaps) runnerwire.MacVMCaps {
	c := runnerwire.MacVMCaps{SignIns: true, Base: runnerwire.MacVMBase{State: runnerwire.MacVMBaseNone}}
	reason, hostOS, hostReason, thermal, noSession := m.hostStatus()
	c.NoSession = noSession
	if m.ledger == nil {
		c.Refusal, c.Reason, c.HostOSVersion, c.HostReason = m.refusal, reason, hostOS, hostReason
		return c
	}
	if _, err := m.ledger.Sweep(); err != nil {
		slog.Warn("macOS VM ledger: tombstone pass", "error", err.Error())
	}
	m.reconcilePrepare()
	m.watchJobs()
	m.pruneDockerWait()
	if m.reclaim != nil {
		if err := m.reclaim(); err != nil {
			slog.Warn("macOS VM ledger: reconcile", "error", err.Error())
		}
	}
	man, base := m.base()
	c.Base = base
	if base.State != runnerwire.MacVMBaseReady {
		m.reasonDiskShort(&c)
		m.reasonSlotLeaked(&c)
		return c
	}
	c.MinCPUs, c.MinMemoryMiB = man.MinCPUs, int(man.MinMemoryMiB)
	c.Harnesses, c.Desktop = slices.Clone(man.Harnesses), man.Desktop
	m.checkAgent(man)
	if why, refusal, belowReason := m.belowMinimum(man); why != "" {
		// design 3.10: a per-run limit below the base's minimum offers none and names the flag to raise.
		slog.Warn("macOS VM sandboxes: this runner offers none", "reason", why)
		c.Refusal, c.Reason = refusal, belowReason
		return c
	}
	m.reasonSlotLeaked(&c)
	m.reasonsWhenReady(&c, man, thermal)
	off, err := m.ledger.Offer(m.guestMemory(man), admission.RunDiskReservation(cmp.Or(man.DiskBytes, macvm.BaseDiskBytes)))
	switch {
	case err == nil:
		c.Max = off.Max
	case last != nil:
		slog.Warn("macOS VM ledger: offer", "error", err.Error())
		c.Max = last.Max
	}
	return c
}

// guestMemory is what one guest of this base takes: the per-run memory, or the base's minimum when that
// is more (a run below it is refused at start).
func (m *macOSVMs) guestMemory(man macvm.Manifest) uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return max(m.perRunMemory, man.MinMemoryMiB<<20)
}

// The caps' refusal for a per-run limit below the base's minimum: the owner's MV-R47 (memory) and MV-R48
// (CPU), with the runner's per-run amount, the base's minimum and the runner's id.
const (
	macOSVMMemoryBelowMinimum = "Per-run memory on this runner is %[1]s GB; a macOS VM needs at least %[2]d GB. Its owner raises it to %[2]d GB or more in Your account → My runner, or with `wardyn runner set %[3]s --per-run-memory %[2]d`."
	macOSVMCPUBelowMinimum    = "Per-run CPU on this runner is %[1]d CPUs; a macOS VM needs at least %[2]d. Its owner raises it to %[2]d or more in Your account → My runner, or with `wardyn runner set %[3]s --per-run-cpu %[2]d`."
)

// belowMinimum is why a per-run limit the organisation sent is below the base's minimum, naming the flag
// to raise, for this Mac's log, and the caps' refusal (design 6, "Guest size below minimum"), else "".
func (m *macOSVMs) belowMinimum(man macvm.Manifest) (why, refusal, reason string) {
	m.mu.Lock()
	mem, cpus := m.perRunMemory, m.perRunCPUs
	m.mu.Unlock()
	switch {
	case mem != 0 && mem < man.MinMemoryMiB<<20:
		gb := strconv.FormatFloat(math.Floor(float64(mem>>20)/1024*10)/10, 'f', -1, 64)
		need := (man.MinMemoryMiB + 1023) >> 10
		return fmt.Sprintf("the runner's per-run memory (%d MiB) is below the macOS base's minimum (%d MiB); raise --per-run-memory", mem>>20, man.MinMemoryMiB),
			fmt.Sprintf(macOSVMMemoryBelowMinimum, gb, need, m.runnerID), runnerwire.MacVMReasonMemoryBelowMinimum
	case cpus != 0 && cpus < man.MinCPUs:
		return fmt.Sprintf("the runner's per-run CPUs (%d) are below the macOS base's minimum (%d); raise --per-run-cpu", cpus, man.MinCPUs),
			fmt.Sprintf(macOSVMCPUBelowMinimum, cpus, man.MinCPUs, m.runnerID), runnerwire.MacVMReasonCPUBelowMinimum
	}
	return "", "", ""
}

// setPerRunLimits is runnerwire.RunnerHooks.PerRunLimits: the per-run CPU (milli-CPU) and memory (MiB)
// the organisation sends on every connect, which each guest is sized at, then a recompute. (0, 0) is
// "no per-run limit": the organisation stores a runner's limits whole or not at all (NULL is nil,
// internal/store/store_runner_limits.go limits) and sends 0 for nil (internal/runner/remote/hub.go
// perRunOf), and no write can set 0 (types.RunnerAmounts.validate: per-run CPU >= 1000 milli-CPU,
// memory >= 1 MiB). The guest then gets setup's value, else the base's minimum (guestMemory).
func (m *macOSVMs) setPerRunLimits(cpuMillis, memoryMiB int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.perRunMemory, m.perRunCPUs = m.defaultPerRunMemory, 0
	if memoryMiB > 0 {
		m.perRunMemory = uint64(memoryMiB) << 20
	}
	if cpuMillis > 0 {
		m.perRunCPUs = int(cpuMillis / 1000)
	}
	m.mu.Unlock()
	m.saveLimits(cpuMillis, memoryMiB)
	m.changed()
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
		if cause := toolsFailure(m.layout.BaseDir(0)); cause != "" {
			b.Failure, b.Error = runnerwire.MacVMFailureTools, m.scrubError(cause)
		}
	case m.preparing():
		b.State, b.Progress = runnerwire.MacVMBasePreparing, prepareProgress(m.layout.PrepareLog())
		scanPrepare(m.layout.PrepareLog()).apply(&b, true)
		if setupCodeWaiting(m.layout.Root) != "" {
			b.Wait = runnerwire.MacVMWaitSetupByHand
		}
		if m.pruneDockerWait() {
			b.Wait = runnerwire.MacVMWaitDocker
		}
	case err == nil && man.State == macvm.BaseUpdateNeeded:
		b.State = runnerwire.MacVMBaseUpdateNeeded
	case err == nil && man.State == macvm.BaseFailed:
		b.State = runnerwire.MacVMBaseFailed
		scanPrepare(m.layout.PrepareLog()).apply(&b, false)
	default:
		if msg := prepareFailure(m.layout.PrepareLog()); msg != "" {
			failure, cause := splitFailure(msg)
			b.State, b.Failure, b.Error = runnerwire.MacVMBaseFailed, failure, m.scrubError(cause)
			scanPrepare(m.layout.PrepareLog()).apply(&b, false)
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
		m.toolsUpdate(man)
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

// mine: e is this runner's and its file is this login's. The entries directory is shared (ERRATA E1), so
// another login can write an entry that names this runner.
func (m *macOSVMs) mine(e admission.Entry) bool { return e.RunnerID == m.runnerID && e.UID == m.uid }

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

// reconcilePrepare releases this runner's prepare and tools-update entries whose helper has exited
// (the helper releases nothing, startJob): neither job has a run directory, and the ledger itself
// refuses while a VM process of this user could still be its VM.
func (m *macOSVMs) reconcilePrepare() {
	for _, k := range []admission.Kind{admission.KindPrepare, admission.KindToolsUpdate} {
		for _, e := range m.own(k) {
			if !m.vmGone(e) {
				continue
			}
			if err := m.ledger.ReclaimOwn(e.ID, ""); err != nil {
				slog.Warn("macOS VM ledger: release a finished job", "kind", string(k), "error", err.Error())
			}
		}
	}
}

// prepareOrder is the helper's prepare phases in order, for an overall percentage.
// Setup by hand comes between provisioning and stage 1 (setupcode.go), and stage 2's tools after the
// shutdown that ends stage 1 (prepare.go stage2); ready is the end, not a phase of progress.
var prepareOrder = []string{macvm.PhaseCheck, macvm.PhaseDownload, macvm.PhaseValidate, macvm.PhaseCreate,
	macvm.PhaseInstall, macvm.PhaseProvision, macvm.PhaseSetupByHand, macvm.PhaseBootstrap, macvm.PhaseShutdown, macvm.PhaseTools}

// helperFailurePrefix starts the helper's own error line in a job's log.
const helperFailurePrefix = "wardyn-macvm: "

// prepareProgress is the overall percentage of the last progress event in prepare.log.
func prepareProgress(log string) int {
	ev, ok, _, _ := lastHelperLines(log)
	if !ok {
		return 0
	}
	i := slices.Index(prepareOrder, ev.Phase)
	if i < 0 {
		return 0
	}
	return min(int((float64(i)+min(max(ev.Fraction, 0), 1))*100/float64(len(prepareOrder))), 99)
}

// prepareFailure is the helper's error when its last prepare ended with one: the helper writes it, after
// its progress lines, as "wardyn-macvm: <error>". Its last own line decides, so an earlier prepare's
// error in the same log is not this one's.
func prepareFailure(log string) string {
	_, _, last, err := lastHelperLines(log)
	msg, ok := strings.CutPrefix(last, helperFailurePrefix)
	if !ok {
		if err != nil {
			return "the prepare log could not be read to its end: " + err.Error()
		}
		return ""
	}
	return msg
}

// setupCodeWaiting is the file where the helper left the one-time code macOS's Setup Assistant asks for
// (macvm.SetupCodeFile, owner-only 0600, read on this Mac by the person at it) when it is there (a
// non-empty 0600 regular file), else "". The runner never reads the code: it only says where the file
// is, so the code is in no log, no status and nothing sent to the organisation.
func setupCodeWaiting(root string) string {
	p := macvm.Layout{Root: root}.SetupCode()
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 || fi.Size() == 0 {
		return ""
	}
	return p
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
	// newImage builds a new base from Apple's newest restore image even when one is ready (the helper's
	// --reprepare); without it, a ready base needs no prepare.
	newImage bool
}

// errMacOSBaseReady is startPrepare's answer for a ready base and no request for a new image.
var errMacOSBaseReady = errors.New("this Mac's macOS base is ready; pass --new-image to build a new one from Apple's newest restore image")

// probeReport is the part of `wardyn-macvm probe`'s JSON the runner reads. Thermal is read when the helper
// reports it.
type probeReport struct {
	Host         string `json:"host"`
	Provisioning string `json:"provisioning"`
	Thermal      string `json:"thermal"`
	// Cgo and VZ say the helper was built with cgo and the Virtualization-framework engine; Reason is its
	// code for why first boot cannot (no_virtualization, no_provisioning), when the framework answered.
	Cgo    bool   `json:"cgo"`
	VZ     bool   `json:"vz"`
	Reason string `json:"reason"`
}

// probeWait bounds the helper's probe.
var probeWait = 30 * time.Second

// probeHelper runs `wardyn-macvm probe`, which exits 0 only when this Mac and this build can run macOS
// VMs. Any other end is the reason the runner offers none and starts no prepare.
func probeHelper(ctx context.Context, helper string) (probeReport, error) {
	ctx, cancel := context.WithTimeout(ctx, probeWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, helper, "probe").Output()
	var r probeReport
	_ = json.Unmarshal(out, &r)
	if err != nil {
		why := err.Error()
		for _, s := range []string{r.Host, r.Provisioning} {
			if s != "" && s != "ok" {
				why = s
				break
			}
		}
		return r, fmt.Errorf("the macOS VM helper's probe: %s", why)
	}
	return r, nil
}

// gate runs the helper's probe and turns macOS VMs off on this runner unless it passes. The caps then
// carry no refusal of their own, only a reason code (host_os_too_old or host_unsupported, when the probe
// says which); the words stay in the log and in `macos-vm status`.
func (m *macOSVMs) gate(ctx context.Context) {
	if m.ledger == nil {
		return
	}
	r, err := probeHelper(ctx, m.helper)
	if err != nil {
		slog.WarnContext(ctx, "macOS VM sandboxes are off on this runner", "reason", err.Error())
		m.ledger, m.why = nil, err.Error()
		m.explainProbe(r)
		return
	}
	m.setThermal(r.Thermal)
}

// helperJob is one helper run that holds a ledger entry of its own: a prepare or a tools update.
type helperJob struct {
	kind   admission.Kind
	disk   int64
	log    string
	sub    string // the helper's command
	helper string
	cpus   int
	memMiB uint64
	runner string
	ledger string // the ledger directory ("" is the package's)
	extra  []string
	descr  string // the job in the error words: "prepare" or "tools update"
}

// startPrepare starts the helper's prepare (see startJob), or with newImage and a ready base its
// re-prepare; a ready base and no newImage starts nothing (errMacOSBaseReady). The helper's probe must
// pass first.
func (m *macOSVMs) startPrepare(ctx context.Context, a prepareArgs) error {
	if m.ledger == nil {
		return errors.New(cmp.Or(m.refusal, m.why))
	}
	var extra []string
	if _, err := macvm.ReadyBase(m.layout.BaseDir(0)); err == nil {
		if !a.newImage {
			return errMacOSBaseReady
		}
		extra = append(extra, "--"+macvm.FlagReprepare)
	}
	if a.keepIPSW {
		extra = append(extra, "--"+macvm.FlagKeepIPSW)
	}
	r, err := probeHelper(ctx, a.helper)
	if err != nil {
		return err
	}
	if err := m.setupByHandHere(ctx, r); err != nil {
		return err
	}
	if err := excludeFromBackups(m.layout.Root); err != nil {
		slog.Warn("macOS VM directory: Time Machine exclusion", "error", err.Error())
	}
	return m.startJob(ctx, helperJob{kind: admission.KindPrepare, disk: admission.PrepareDiskReservation(macvm.BaseDiskBytes),
		log: m.layout.PrepareLog(), sub: macvm.PrepareCommand, helper: a.helper, cpus: a.cpus, memMiB: a.memoryMiB, runner: a.runnerID,
		ledger: a.ledgerDir, extra: extra, descr: "prepare"})
}

// processEUID is os.Geteuid (tests replace it).
var processEUID = os.Geteuid

// setupByHandHere refuses a prepare that will need setup by hand (the probe says this Mac has no guest
// provisioning) from where the guest's window cannot open: outside the desktop's GUI session
// (checkSession), or as root. The helper checks the same before it writes anything; this answer stays on
// the command line (`macos-vm prepare`, `setup`) and is not sent anywhere.
func (m *macOSVMs) setupByHandHere(ctx context.Context, r probeReport) error {
	if r.Reason != runnerwire.MacVMHostNoProvisioning {
		return nil
	}
	m.checkSession(ctx)
	if _, _, _, _, noSession := m.hostStatus(); noSession || processEUID() == 0 {
		return fmt.Errorf("this Mac's macOS has no guest provisioning, so its prepare needs setup by hand: %w", macvm.ErrNoDesktopSession)
	}
	return nil
}

// toolsUpdateLog, in the VM directory, is a tools update's output.
const toolsUpdateLog = "tools-update.log"

// startToolsUpdate starts the helper's tools update of the base whose manifest is man (design 3.4, 3.12):
// its own ledger entry, a guest at the base's minimum CPUs and the per-run memory (or the minimum), and
// the helper's `update-tools`, which takes the base's update lock, works on a staging copy and swaps it
// in, so runs keep cloning the old base meanwhile. It is an automatic update, so it passes no --force,
// which is for an update someone asks for (design 3.12, "an update someone asks for retries every pin"):
// the helper compares the base's agent with its own build (macvm.NeedsToolsUpdate) and pushes its own.
func (m *macOSVMs) startToolsUpdate(ctx context.Context, man macvm.Manifest) error {
	if m.ledger == nil {
		return errors.New(cmp.Or(m.refusal, m.why))
	}
	return m.startJob(ctx, helperJob{kind: admission.KindToolsUpdate, disk: admission.RunDiskReservation(cmp.Or(man.DiskBytes, macvm.BaseDiskBytes)),
		log: filepath.Join(m.layout.Root, toolsUpdateLog), sub: macvm.UpdateToolsCommand, helper: m.helper, cpus: max(man.MinCPUs, 1),
		memMiB: m.guestMemory(man) >> 20, runner: m.runnerID, ledger: m.ledgerDir, descr: "tools update"})
}

// startJob takes the job's ledger entry (the tombstone pass first) and starts the helper in its own
// session, its output appended to the job's log, then waits until the helper has claimed the entry, so
// the entry never outlives both. The helper releases nothing: once it exits, the runner's recompute
// reconciles the entry.
func (m *macOSVMs) startJob(ctx context.Context, j helperJob) error {
	if _, err := m.ledger.Sweep(); err != nil {
		return err
	}
	self := os.Getpid()
	start, err := admission.Self(m.host, self)
	if err != nil {
		return err
	}
	e, err := m.ledger.Admit(admission.Request{Kind: j.kind, GuestCPUs: j.cpus, GuestMemory: j.memMiB << 20,
		DiskReserve: j.disk, OwnerPID: self, OwnerStart: start})
	if err != nil {
		return err
	}
	never := func(err error) error {
		// The VM never started: the entry goes at once.
		return errors.Join(err, m.ledger.Release(e.ID, admission.Destroyed{Stopped: true}))
	}
	logf, err := os.OpenFile(j.log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return never(err)
	}
	defer logf.Close()
	args := []string{j.sub, "--" + macvm.FlagRoot, m.layout.Root, "--" + macvm.FlagCPUs, strconv.Itoa(j.cpus),
		"--" + macvm.FlagMemoryMiB, strconv.FormatUint(j.memMiB, 10), "--" + macvm.FlagRunnerID, j.runner, "--" + macvm.FlagLedgerEntry, e.ID}
	if j.ledger != "" {
		args = append(args, "--"+macvm.FlagLedgerDir, j.ledger)
	}
	if m.sockRoot != "" {
		args = append(args, "--"+macvm.FlagSockRoot, m.sockRoot)
	}
	cmd := exec.Command(j.helper, append(args, j.extra...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return never(fmt.Errorf("start the macOS VM helper: %w", err))
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	m.changed()
	return m.awaitClaim(ctx, e.ID, j, cmd, exited, never)
}

// awaitClaim is startJob's wait: a helper that never claimed its entry never built a VM (it claims
// first), so the entry goes.
func (m *macOSVMs) awaitClaim(ctx context.Context, id string, j helperJob, cmd *exec.Cmd, exited chan struct{}, never func(error) error) error {
	for deadline := time.Now().Add(prepareClaimWait); ; {
		if counted, err := m.ledger.Counted(); err == nil {
			i := slices.IndexFunc(counted, func(x admission.Entry) bool { return x.ID == id })
			if i >= 0 && counted[i].HelperPID != 0 {
				return nil
			}
		}
		select {
		case <-exited:
			if err := m.claimed(id, j); err != nil {
				return err
			}
			return never(fmt.Errorf("the macOS VM helper's %s ended at once; see %s", j.descr, j.log))
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			_ = cmd.Process.Kill()
			<-exited
			if err := m.claimed(id, j); err != nil {
				return err
			}
			return never(fmt.Errorf("the macOS VM helper did not take its ledger entry; see %s", j.log))
		}
	}
}

// prepareClaimWait bounds startJob's wait for the helper to claim its entry.
var prepareClaimWait = time.Minute

// claimed is nil when entry id is not claimed by a helper (the entry may go), else why it stays: a helper
// that claimed it is the VM's, and the recompute reconciles it once that helper has gone.
func (m *macOSVMs) claimed(id string, j helperJob) error {
	counted, err := m.ledger.Counted()
	if err != nil {
		return err
	}
	if i := slices.IndexFunc(counted, func(x admission.Entry) bool { return x.ID == id }); i >= 0 && counted[i].HelperPID != 0 {
		return fmt.Errorf("the macOS VM helper's %s ended; see %s", j.descr, j.log)
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

// remove deletes the bases and the download cache, never while this runner holds a VM. Prepare's log,
// the ledger and the VM directory stay.
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
		keep := ""
		if plan.keepIPSW {
			keep = " --" + macvm.FlagKeepIPSW
		}
		fmt.Fprintln(out, "macOS VMs are on. To build this Mac's macOS base: wardyn-runnerd macos-vm prepare --per-run-cpu <CPUs> --per-run-memory <GB>"+keep+" (the runner's per-run limits).")
		return
	}
	id, err := runneridentity.Load(plan.paths.stateDir, plan.org)
	if err != nil {
		fmt.Fprintf(out, "macOS VMs: %v\n", err)
		return
	}
	m := openMacOSVMs(macOSVMSetup{ledgerDir: admission.DefaultDir, root: root, runnerID: id.RunnerID.String(), uid: os.Getuid(), host: admission.SystemHost(), owner: fileOwner})
	startSetupPrepare(cmd, m, plan, id.RunnerID.String(), limits)
}

// startSetupPrepare is setup's prepare on m: the helper beside the installed runnerd, the limits just
// registered, and setup's --keep-ipsw.
func startSetupPrepare(cmd *cobra.Command, m *macOSVMs, plan setupPlan, runnerID string, limits *types.RunnerLimits) {
	out := cmd.OutOrStdout()
	a := prepareArgs{helper: filepath.Join(filepath.Dir(plan.paths.installed), "wardyn-macvm"), runnerID: runnerID,
		cpus: int(limits.PerRun.CPUMillis / 1000), memoryMiB: uint64(limits.PerRun.MemoryMiB), keepIPSW: plan.keepIPSW}
	if err := m.startPrepare(cmd.Context(), a); err != nil {
		fmt.Fprintf(out, "macOS VMs are on, but this Mac's macOS base did not start preparing: %v\n", err)
		return
	}
	fmt.Fprintf(out, "Preparing this Mac's macOS base in the background (hours); progress is in %s and in `wardyn-runnerd macos-vm status`.\n", m.layout.PrepareLog())
}
