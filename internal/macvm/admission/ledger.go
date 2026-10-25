// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package admission is the host-wide ledger every macOS VM on a Mac takes an
// entry in before its helper starts: a run's VM, prepare's VM and a tools
// update's VM, from every login session's runner. It enforces Apple's cap of
// two macOS VMs, the host memory budget and a worst-case disk reservation,
// and frees a slot only once the VM is verifiably gone.
//
// Layout (the runner package's postinstall makes all three, as root):
//
//	/Users/Shared/Wardyn/                     root:wheel 0755
//	/Users/Shared/Wardyn/admission.lock       root:wheel 0644
//	/Users/Shared/Wardyn/macos-vm-admission/  root:wheel 1777 (sticky, like /tmp)
//
// The lock sits outside the world-writable entries directory, so no login
// can plant, rename or unlink it. A runner opens it walking from / one
// component at a time (openat with O_NOFOLLOW, then fstat): every directory
// on the way must be root-owned and either not group- or other-writable or
// sticky, the lock's own directory must not be group- or other-writable at
// all, and the lock must be a root-owned regular file that is not group- or
// other-writable. After taking the flock it checks the path still names the
// file it locked. Anything else is refused with the fix named, so a
// directory a user made before the install fails closed. The entries
// directory must be root-owned and sticky. It holds one file per entry,
// e-<random>.json, mode 0644: readable by every login's runner so
// all of them count it, writable only by the login that created it, and
// (sticky bit) removable only by it. Every write goes to an exclusive temp
// file in the same dir and is renamed into place, so a reader never sees half
// an entry; a file that does not parse anyway counts as a live VM (fail
// closed) until the boot changes. An entry's uid is its file's owner, never a
// field inside it. Every transaction holds flock(LOCK_EX) on admission.lock,
// waiting at most LockWait. A runner never creates the lock: if it is
// missing, Open refuses and names the fix.
//
// Limits stated, not fixed here:
//   - only the owning login (or root) can delete a file, so another login's
//     earlier-boot entries and tombstones are never cleared by a runner; they
//     are not counted, but the directory keeps them until a root sweep (a
//     package follow-up);
//   - the memory budget subtracts the Docker memory of logins that hold an
//     entry (and the asking login's), not of a logged-in user whose Docker
//     runs with no macOS VM; the live free-memory check at admission is the
//     guard for that;
//   - another login's change reaches this runner's offer only when it
//     recomputes: Watch recomputes every OfferRecompute.
//
// Two numbers, each compared with its own count:
//   - host capacity (the cap, the memory budget, the disk reservations) is
//     checked at Admit against every counted entry on the Mac, whichever login
//     or kind holds it. It is the authority;
//   - a runner's allowance, the max it offers (Offer), is its own live runs
//     plus the host's headroom. The org compares it with that runner's own
//     runs at placement; the ledger never compares the host-wide count with it.
//
// Trust: every local account can write to the directory and open the lock,
// so any local user can add entries that hold slots, memory or disk, or hold
// the lock: a local denial of service on macOS VMs only. A forged entry can
// refuse admissions but never grant one, because entries only ever subtract
// capacity. A tombstone (Sweep) is honoured only after the reader re-checks
// its condition against the live process table, so a forged or early
// tombstone changes nothing. An honoured tombstone recovers only the dead
// VM's capacity (its slot and memory); its disk reservation stays held as a
// debt until the owner's runner reconciles the entry (ReclaimOwn, once the
// owner's run files are gone).
package admission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Limits.
const (
	// LicenceCap is Apple's limit: up to two additional macOS instances.
	LicenceCap = 2
	// MacOSReserveBytes is the memory left to macOS itself.
	MacOSReserveBytes uint64 = 4 << 30
	// DefaultOverheadBytes is the host memory one VM costs beyond its guest
	// memory, until a real Mac's measurement replaces it.
	DefaultOverheadBytes uint64 = 1 << 30
	// LiveMemoryMarginBytes: a start is also refused when available memory is
	// below the guest's memory plus this.
	LiveMemoryMarginBytes uint64 = 2 << 30
	// DiskFloorBytes is free space every admission leaves untouched.
	DiskFloorBytes int64 = 5 << 30
	// DiskExtraBytes covers aux storage, the capped helper log and metadata.
	DiskExtraBytes int64 = 1 << 30
	// IPSWBytes is the restore image prepare downloads.
	IPSWBytes int64 = 25 << 30
	// LockWait bounds the wait for admission.lock.
	LockWait = 10 * time.Second
)

// ErrBusy is returned when the lock is not free within LockWait.
var ErrBusy = &Refusal{Reason: "This Mac's macOS VM ledger is busy"}

// RunDiskReservation is what a run or a tools update reserves: the disk's
// logical size plus DiskExtraBytes. Writes to a clone are private
// copy-on-write blocks, so on the live volume a clone's growth cannot pass
// its logical size; nothing already written is credited back.
//
// The promise is narrowed to that: the reservation bounds each VM's growth on
// the live volume, not the space host APFS snapshots hold. While a snapshot
// (Time Machine's hourly local one, or any other) keeps a clone's earlier
// blocks, rewriting them allocates new ones, and a deleted run's blocks stay
// allocated until the snapshot goes, so Wardyn's VMs can use more than they
// reserved. Admission reads live free space at every start, so space a
// snapshot already holds counts against the next VM, and the runner's
// periodic free-space check is the remaining guard. No backup or snapshot
// setting is changed to close the gap.
func RunDiskReservation(diskLogicalBytes int64) int64 { return diskLogicalBytes + DiskExtraBytes }

// PrepareDiskReservation is prepare's: the restore image plus a run's.
func PrepareDiskReservation(diskLogicalBytes int64) int64 {
	return IPSWBytes + RunDiskReservation(diskLogicalBytes)
}

// Kind is what an entry's VM is for.
type Kind string

const (
	KindRun         Kind = "run"
	KindPrepare     Kind = "prepare"
	KindToolsUpdate Kind = "tools_update"
	// KindLeaked holds a framework slot the ledger cannot account for (the
	// framework refused a start while fewer entries were live than the cap).
	// It clears only when the Mac restarts and is never tombstoned.
	KindLeaked Kind = "leaked"
)

// State is an entry's lifecycle state. Every state is counted.
type State string

const (
	StateReserved  State = "reserved"
	StateStarting  State = "starting"
	StatePreparing State = "preparing"
	StateRunning   State = "running"
	StateStopping  State = "stopping"
	StateLeaked    State = "leaked"
)

// Entry is one VM's claim on the host.
type Entry struct {
	ID       string `json:"id"`
	Kind     Kind   `json:"kind"`
	State    State  `json:"state"`
	RunnerID string `json:"runner_id"`
	RunID    string `json:"run_id,omitempty"`
	// Slot is the licence slot (0 or 1), for the per-slot-base fallback.
	Slot        int    `json:"slot"`
	OwnerPID    int    `json:"owner_pid"`
	OwnerStart  int64  `json:"owner_start"`
	HelperPID   int    `json:"helper_pid,omitempty"`
	HelperStart int64  `json:"helper_start,omitempty"`
	GuestCPUs   int    `json:"guest_cpus"`
	GuestMemory uint64 `json:"guest_memory_bytes"`
	// DockerMemory is the memory the login's Docker engine is configured with.
	DockerMemory uint64    `json:"docker_memory_bytes"`
	Overhead     uint64    `json:"overhead_bytes"`
	DiskReserve  int64     `json:"disk_reserve_bytes"`
	BootTime     int64     `json:"boot_time"`
	Created      time.Time `json:"created"`

	// UID is the entry file's owner.
	UID  int    `json:"-"`
	file string // the entry's file name
}

// Proc is one process in the host's process table.
type Proc struct {
	PID   int
	UID   int
	Start int64 // start time, microseconds since the epoch
	Name  string
}

// Host is what the ledger reads from the Mac.
type Host interface {
	// BootTime is kern.boottime in microseconds since the epoch.
	BootTime() (int64, error)
	// MemoryBytes is hw.memsize.
	MemoryBytes() (uint64, error)
	// AvailableMemoryBytes is memory free for use now.
	AvailableMemoryBytes() (uint64, error)
	// FreeDiskBytes is the space available to this user on path's volume.
	FreeDiskBytes(path string) (int64, error)
	Processes() ([]Proc, error)
}

// VirtualizationProcess is the name of the framework's per-VM process. The
// process table truncates names (to 16 bytes on macOS), so matching is by
// prefix, which can only over-count and so fails closed. (The exact name is
// pinned on a real Mac.)
const VirtualizationProcess = "com.apple.Virtualization.VirtualMachine"

// HelperProcess is the helper's process name. A helper of a uid counts like
// a Virtualization process when deciding whether that uid's VM may be gone:
// it may be about to create one.
const HelperProcess = "wardyn-macvm"

// OfferRecompute is how often Watch recomputes the offer, which is how a
// runner learns of other logins' entries.
const OfferRecompute = 15 * time.Second

// DefaultDir is the ledger directory the runner package's postinstall makes.
// Its lock is admission.lock in its parent directory.
const DefaultDir = "/Users/Shared/Wardyn/macos-vm-admission"

// DefaultLockPath is DefaultDir's lock.
const DefaultLockPath = "/Users/Shared/Wardyn/admission.lock"

const (
	lockFile   = "admission.lock"
	tombSuffix = ".tombstone"
)

// Config configures a Ledger.
type Config struct {
	Dir      string
	UID      int    // this login's uid (os.Getuid())
	RunnerID string // this runner's id
	// HostCap is the host cap: LicenceCap, or 1 under the per-slot-base
	// fallback when only one base exists. 0 means LicenceCap.
	HostCap int
	// DockerMemoryBytes is the memory this login's Docker engine is
	// configured with; it is not available to macOS guests.
	DockerMemoryBytes uint64
	// Volume is a path on the volume VM disks live on (macos-vm.noindex).
	Volume        string
	OverheadBytes uint64 // 0: DefaultOverheadBytes
	Host          Host
	// FileOwner returns a ledger file's owner uid; nil reads it from the stat.
	FileOwner func(os.FileInfo) int
	// Fstat, if set, replaces fstat(2) in the lock and directory checks;
	// name is the path the descriptor was opened at (tests).
	Fstat func(fd int, name string) (unix.Stat_t, error)
	// LockWait overrides LockWait (tests).
	LockWait time.Duration

	created    func(name string) // tests: told of each file this ledger creates
	afterFlock func()            // tests: runs once the flock is held
}

// Ledger is one runner's handle on the host ledger.
type Ledger struct{ c Config }

// Open checks the ledger directory and returns a handle.
func Open(c Config) (*Ledger, error) {
	if c.Host == nil || c.Dir == "" || c.RunnerID == "" {
		return nil, errors.New("admission: a ledger needs a host, a directory and a runner id")
	}
	if c.HostCap <= 0 || c.HostCap > LicenceCap {
		c.HostCap = LicenceCap
	}
	if c.OverheadBytes == 0 {
		c.OverheadBytes = DefaultOverheadBytes
	}
	if c.FileOwner == nil {
		c.FileOwner = statOwner
	}
	if c.LockWait == 0 {
		c.LockWait = LockWait
	}
	c.Dir = filepath.Clean(c.Dir)
	l := &Ledger{c: c}
	if err := l.checkEntriesDir(); err != nil {
		return nil, err
	}
	f, err := l.openLock()
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	return l, nil
}

func statOwner(fi os.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

// Request asks for an entry.
type Request struct {
	Kind        Kind
	RunID       string
	GuestCPUs   int
	GuestMemory uint64
	DiskReserve int64
	OwnerPID    int
	OwnerStart  int64
}

// Refusal is an admission refused for capacity, with the reason in words. A refusal for disk also
// carries its figures: what the run needs free (its reservation plus the floor) and what is free
// beyond the reservations already held; both are zero on every other refusal.
type Refusal struct {
	Reason       string
	DiskNeeded   int64
	DiskFreeLeft int64
}

func (r *Refusal) Error() string { return "admission: " + r.Reason }

// IsRefusal reports whether err is a capacity refusal.
func IsRefusal(err error) bool {
	var r *Refusal
	return errors.As(err, &r)
}

// view is the ledger as one transaction sees it.
type view struct {
	boot    int64
	procs   []Proc
	counted []Entry // counted toward host capacity
	// debts are honoured-tombstoned entries: their slot and memory are
	// recovered, but their disk reservation is held until the owner's
	// runner reconciles them (owner-file cleanup pending).
	debts   []Entry
	all     []Entry // every parsed entry, any boot, tombstoned or not
	unknown int     // unparseable entry files, each counted as one VM
	tombs   map[string]tomb
}

type tomb struct {
	name  string
	owner int
}

// maxEntryFile bounds an entry file a reader parses.
const maxEntryFile = 64 << 10

func (l *Ledger) load() (*view, error) {
	boot, err := l.c.Host.BootTime()
	if err != nil {
		return nil, err
	}
	procs, err := l.c.Host.Processes()
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(l.c.Dir)
	if err != nil {
		return nil, err
	}
	v := &view{boot: boot, procs: procs, tombs: map[string]tomb{}}
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, "e-") && strings.HasSuffix(name, ".json"+tombSuffix) {
			if fi, err := de.Info(); err == nil {
				v.tombs[strings.TrimSuffix(name, tombSuffix)] = tomb{name: name, owner: l.c.FileOwner(fi)}
			}
		}
	}
	for _, de := range des {
		name := de.Name()
		if !strings.HasPrefix(name, "e-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		fi, err := de.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue // gone since the listing
		}
		if err != nil { // one that cannot be inspected may be live: never skipped
			return nil, err
		}
		e, ok := readEntry(filepath.Join(l.c.Dir, name), fi)
		if !ok {
			if fi.ModTime().UnixMicro() >= boot {
				v.unknown++ // fail closed: counts as a live VM
			}
			continue
		}
		e.file, e.UID = name, l.c.FileOwner(fi)
		v.all = append(v.all, e)
		if e.BootTime != boot {
			continue
		}
		if v.tombstoned(e) {
			v.debts = append(v.debts, e)
			continue
		}
		v.counted = append(v.counted, e)
	}
	return v, nil
}

func readEntry(path string, fi os.FileInfo) (Entry, bool) {
	var e Entry
	if !fi.Mode().IsRegular() || fi.Size() > maxEntryFile {
		return e, false
	}
	f, err := os.Open(path)
	if err != nil {
		return e, false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxEntryFile))
	if err != nil || json.Unmarshal(b, &e) != nil || e.ID == "" {
		return e, false
	}
	return e, true
}

func (l *Ledger) mine(e Entry) bool { return e.UID == l.c.UID && e.RunnerID == l.c.RunnerID }

// usage is the capacity the counted entries hold.
type usage struct {
	count     int
	memory    uint64
	disk      int64
	ownRuns   int
	dockerBy  map[int]uint64 // Docker memory per login holding an entry
	slotsUsed [LicenceCap]bool
}

func (l *Ledger) usage(v *view) usage {
	u := usage{count: len(v.counted) + v.unknown, dockerBy: map[int]uint64{}}
	for _, e := range v.debts {
		u.disk += e.DiskReserve
	}
	for _, e := range v.counted {
		u.memory += e.GuestMemory + e.Overhead
		u.disk += e.DiskReserve
		if e.Slot >= 0 && e.Slot < LicenceCap {
			u.slotsUsed[e.Slot] = true
		}
		if l.mine(e) && e.Kind == KindRun {
			u.ownRuns++
		}
		if e.DockerMemory > u.dockerBy[e.UID] {
			u.dockerBy[e.UID] = e.DockerMemory
		}
	}
	return u
}

// memoryBudget is host memory minus macOS's share and the Docker memory of
// every login holding an entry, plus this login's, each counted once.
func (l *Ledger) memoryBudget(u usage) (uint64, error) {
	total, err := l.c.Host.MemoryBytes()
	if err != nil {
		return 0, err
	}
	reserved := MacOSReserveBytes
	for uid, d := range u.dockerBy {
		if uid != l.c.UID {
			reserved += d
		}
	}
	reserved += max(u.dockerBy[l.c.UID], l.c.DockerMemoryBytes)
	if total <= reserved {
		return 0, nil
	}
	return total - reserved, nil
}

// Admit writes a reserved entry for req, or refuses it when host capacity
// would be passed. It is the authority; a runner's offered max is checked by
// the org, never here.
func (l *Ledger) Admit(req Request) (Entry, error) {
	var out Entry
	err := l.withLock(func() error {
		v, err := l.load()
		if err != nil {
			return err
		}
		l.tidy(v)
		u := l.usage(v)
		if u.count+1 > l.c.HostCap {
			return &Refusal{Reason: fmt.Sprintf("this Mac already runs %d macOS VMs, the most it may", u.count)}
		}
		budget, err := l.memoryBudget(u)
		if err != nil {
			return err
		}
		if u.memory+req.GuestMemory+l.c.OverheadBytes > budget {
			return &Refusal{Reason: fmt.Sprintf("this Mac's memory for macOS VMs (%d MiB after Docker and macOS) is in use", budget>>20)}
		}
		if avail, err := l.c.Host.AvailableMemoryBytes(); err == nil && avail < req.GuestMemory+LiveMemoryMarginBytes {
			return &Refusal{Reason: "this Mac does not have enough free memory right now"}
		}
		free, err := l.c.Host.FreeDiskBytes(l.c.Volume)
		if err != nil {
			return err
		}
		if free-u.disk < req.DiskReserve+DiskFloorBytes {
			return &Refusal{Reason: fmt.Sprintf("this Mac needs %d GiB free for another macOS VM", (req.DiskReserve+DiskFloorBytes+u.disk)>>30),
				DiskNeeded: req.DiskReserve + DiskFloorBytes, DiskFreeLeft: free - u.disk}
		}
		slot := 0
		for slot < LicenceCap-1 && u.slotsUsed[slot] {
			slot++
		}
		out = Entry{
			Kind: req.Kind, State: StateReserved, RunnerID: l.c.RunnerID, RunID: req.RunID,
			Slot: slot, OwnerPID: req.OwnerPID, OwnerStart: req.OwnerStart,
			GuestCPUs: req.GuestCPUs, GuestMemory: req.GuestMemory, DockerMemory: l.c.DockerMemoryBytes,
			Overhead: l.c.OverheadBytes, DiskReserve: req.DiskReserve, BootTime: v.boot, Created: time.Now().UTC(),
		}
		if req.Kind == KindPrepare {
			out.State = StatePreparing
		}
		return l.create(&out)
	})
	return out, err
}

// AddLeaked records a framework slot the ledger cannot account for: the
// framework refused a start while fewer entries than the cap were counted.
// It holds one slot (no memory or disk) until the Mac restarts. own is the
// entry of the start the framework refused: it is still counted then, and
// the refusal is checked without it. "" excludes nothing.
func (l *Ledger) AddLeaked(own string, ownerPID int, ownerStart int64) (Entry, error) {
	var out Entry
	err := l.withLock(func() error {
		v, err := l.load()
		if err != nil {
			return err
		}
		count := l.usage(v).count
		if own != "" && slices.ContainsFunc(v.counted, func(e Entry) bool { return e.ID == own }) {
			count--
		}
		if count >= l.c.HostCap {
			return &Refusal{Reason: "the framework's refusal is accounted for by counted entries"}
		}
		out = Entry{Kind: KindLeaked, State: StateLeaked, RunnerID: l.c.RunnerID, Slot: -1,
			OwnerPID: ownerPID, OwnerStart: ownerStart, BootTime: v.boot, Created: time.Now().UTC()}
		return l.create(&out)
	})
	return out, err
}

// Update changes one of this runner's entries (its state, its helper).
func (l *Ledger) Update(id string, fn func(*Entry)) (Entry, error) {
	var out Entry
	err := l.withLock(func() error {
		v, err := l.load()
		if err != nil {
			return err
		}
		e, err := l.own(v, id)
		if err != nil {
			return err
		}
		file := e.file
		fn(&e)
		e.ID, e.RunnerID, e.UID, e.file = id, l.c.RunnerID, l.c.UID, file
		out = e
		return l.rewrite(&e)
	})
	return out, err
}

// ClaimHelper is called by the helper itself (same uid and runner as the
// entry), under the lock, before it creates its VM: it records its own pid and
// start time, so the entry never depends on runnerd surviving the spawn. It
// refuses when the entry is gone, from an earlier boot, or has a tombstone
// beside it (written or forged: the helper does not start either way).
func (l *Ledger) ClaimHelper(id string, pid int, start int64) (Entry, error) {
	var out Entry
	err := l.withLock(func() error {
		v, err := l.load()
		if err != nil {
			return err
		}
		e, err := l.own(v, id)
		if err != nil {
			return fmt.Errorf("admission: this VM's ledger entry: %w", err)
		}
		if e.BootTime != v.boot {
			return errors.New("admission: this VM's ledger entry is from an earlier boot")
		}
		if _, ok := v.tombs[e.file]; ok {
			return errors.New("admission: this VM's ledger entry was tombstoned")
		}
		e.HelperPID, e.HelperStart = pid, start
		if e.State == StateReserved {
			e.State = StateStarting
		}
		out = e
		return l.rewrite(&e)
	})
	return out, err
}

func (l *Ledger) own(v *view, id string) (Entry, error) {
	e, ok := find(v.all, id)
	if !ok {
		return Entry{}, os.ErrNotExist
	}
	if !l.mine(e) {
		return Entry{}, errors.New("admission: that entry belongs to another runner")
	}
	return e, nil
}

// Destroyed is the owner's evidence that an entry's VM is gone.
type Destroyed struct {
	// Stopped: the helper reported the VM stopped, or the VM never started.
	Stopped bool
	// RunDir is the run (or prepare) directory, which must be gone; empty for
	// a VM with no run dir of its own.
	RunDir string
}

// Release removes one of this runner's entries, only after verified
// destruction: the helper said stopped, its process is gone, and its run dir
// is gone.
func (l *Ledger) Release(id string, d Destroyed) error {
	return l.withLock(func() error {
		v, err := l.load()
		if err != nil {
			return err
		}
		e, err := l.own(v, id)
		if err != nil {
			return err
		}
		switch {
		case !d.Stopped:
			return errors.New("admission: release needs the helper's report that the VM stopped")
		case e.HelperPID != 0 && v.alive(e.HelperPID, e.HelperStart):
			return errors.New("admission: the helper is still running")
		}
		if d.RunDir != "" {
			if _, err := os.Lstat(d.RunDir); !os.IsNotExist(err) {
				return errors.New("admission: the run directory is not gone yet")
			}
		}
		return l.remove(e)
	})
}

// Offer is what this runner advertises: Max is its own live runs plus the
// host's headroom (the smallest of the cap left, the VMs at the per-run size
// the memory budget still fits, and those the disk still fits). Reason says
// why the headroom is 0.
type Offer struct {
	Max      int
	Headroom int
	Reason   string
}

// Offer computes the advertised max for VMs of the given per-run size.
func (l *Ledger) Offer(perRunMemory uint64, perRunDisk int64) (Offer, error) {
	var out Offer
	err := l.withLock(func() error {
		v, err := l.load()
		if err != nil {
			return err
		}
		u := l.usage(v)
		budget, err := l.memoryBudget(u)
		if err != nil {
			return err
		}
		free, err := l.c.Host.FreeDiskBytes(l.c.Volume)
		if err != nil {
			return err
		}
		more, reason := l.c.HostCap-u.count, "this Mac already runs the most macOS VMs it may"
		memFit := 0
		if budget > u.memory {
			memFit = int((budget - u.memory) / (perRunMemory + l.c.OverheadBytes))
		}
		if memFit < more {
			more, reason = memFit, "this Mac's memory, after Docker's and macOS's own share, does not fit another macOS VM at the per-run memory limit"
		}
		if perRunDisk > 0 {
			diskFit := int(max(free-u.disk-DiskFloorBytes, 0) / perRunDisk)
			if diskFit < more {
				more, reason = diskFit, "this Mac does not have enough free disk for another macOS VM"
			}
		}
		out.Headroom = max(more, 0)
		out.Max = u.ownRuns + out.Headroom
		if out.Headroom == 0 {
			out.Reason = reason
		}
		return nil
	})
	return out, err
}

// Watch recomputes the offer every interval (OfferRecompute when 0) until ctx
// ends, and calls changed with the first offer and with every one that
// differs, so the runner can resend its caps. Errors are skipped: the last
// offer stands until a recompute succeeds.
func (l *Ledger) Watch(ctx context.Context, interval time.Duration, perRunMemory uint64, perRunDisk int64, changed func(Offer)) {
	if interval <= 0 {
		interval = OfferRecompute
	}
	var last *Offer
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if o, err := l.Offer(perRunMemory, perRunDisk); err == nil && (last == nil || *last != o) {
			last = &o
			changed(o)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Counted returns the entries counted toward host capacity now.
func (l *Ledger) Counted() ([]Entry, error) {
	var out []Entry
	err := l.withLock(func() error {
		v, err := l.load()
		if err != nil {
			return err
		}
		out = v.counted
		return nil
	})
	return out, err
}

func find(es []Entry, id string) (Entry, bool) {
	for _, e := range es {
		if e.ID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// remove deletes one of this login's entries; a tombstone's writer removes
// its tombstone once it sees the entry gone.
func (l *Ledger) remove(e Entry) error {
	if err := os.Remove(filepath.Join(l.c.Dir, e.file)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// create gives e a random id and writes its file.
func (l *Ledger) create(e *Entry) error {
	for attempt := 0; ; attempt++ {
		id, err := newID()
		if err != nil {
			return err
		}
		e.ID, e.UID, e.file = id, l.c.UID, "e-"+id+".json"
		if _, err := os.Lstat(filepath.Join(l.c.Dir, e.file)); err == nil && attempt < 3 {
			continue
		}
		return l.rewrite(e)
	}
}

// rewrite writes e's file through an exclusive temp file in the same dir and
// a rename, so a reader never sees half an entry.
func (l *Ledger) rewrite(e *Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return l.writeFile(e.file, b)
}

// writeFile writes name (mode exactly 0644, whatever the umask) via a temp
// file and a rename. Only this login's own names are ever written.
func (l *Ledger) writeFile(name string, b []byte) error {
	suffix, err := newID()
	if err != nil {
		return err
	}
	tmp := filepath.Join(l.c.Dir, "."+name+"."+suffix+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer os.Remove(tmp) // a no-op after the rename
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(l.c.Dir, name)); err != nil {
		return err
	}
	if l.c.created != nil {
		l.c.created(name)
	}
	return nil
}

func newID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
