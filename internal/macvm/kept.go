// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// The three ways a boot can reach a kept machine (KeptRun.Mode).
const (
	// KeptNew clones the base into a new kept folder and boots the clone.
	KeptNew = "new"
	// KeptResume boots the disk already in the kept folder, with the machine-id kept beside it.
	KeptResume = "resume"
	// KeptFresh deletes the kept folder's disk and clones the current base into the same folder, same id.
	KeptFresh = "fresh"
)

// KeptFile is the kept machine's record inside its folder.
const KeptFile = "kept.json"

// Stop kinds a KeptRecord holds.
const (
	KeptStopGraceful = "graceful"
	KeptStopHard     = "hard"
)

// KeptRun is runner.KeptMachineSpec as the helper needs it, written into run.json. The run folder still
// holds run.json, the log and the sockets; the disk, aux storage, machine-id and pair key of a kept
// machine live in its kept folder, so ending the run deletes the run folder and keeps the machine.
type KeptRun struct {
	ID   string `json:"id"`   // the machine's UUID
	Mode string `json:"mode"` // KeptNew, KeptResume or KeptFresh
	// OwnerHash is types.DriveSubjectHash of the owner and RunnerID the registration that made the
	// machine: a resume or reset whose pair differs from kept.json's is refused.
	OwnerHash string `json:"owner_hash"`
	RunnerID  string `json:"runner_id"`
	// Base is the base's name, recorded in kept.json when the machine is made or reset.
	Base string `json:"base,omitempty"`
	// BaseDigest is BaseDigest(base) as the runner checked it, which it reserved the kept disk's bytes
	// against. A new or fresh boot whose current base differs is refused (ErrKeptBaseChanged) before any
	// disk is touched; a resume boots the machine's own disk and ignores it.
	BaseDigest string `json:"base_digest,omitempty"`
	// IdleDays moves expires_at that far past each boot and stop (KeptDefaultIdleDays when 0).
	IdleDays int `json:"idle_days,omitempty"`
	// Name is the machine's name as the organisation knows it, recorded in kept.json when the machine is
	// made and again when a boot names it otherwise (a rename); empty leaves the recorded one.
	Name string `json:"name,omitempty"`
}

// KeptRecord is kept.json: what the Mac knows of one kept machine, readable with no network.
type KeptRecord struct {
	ID        string `json:"id"`
	OwnerHash string `json:"owner_hash"`
	RunnerID  string `json:"runner_id"`
	Base      string `json:"base,omitempty"`
	// Name is the machine's name as of its latest boot that carried one, for listing it with no network.
	Name           string    `json:"name,omitempty"`
	BasePreparedAt time.Time `json:"base_prepared_at"`
	CreatedAt      time.Time `json:"created_at"`
	// LastStop and StopKind (KeptStopGraceful or KeptStopHard) describe the latest stop; both are empty
	// until the machine has stopped once.
	LastStop time.Time `json:"last_stop,omitzero"`
	StopKind string    `json:"stop_kind,omitempty"`
	// ActiveRun is the run id that booted the machine and has not yet stopped it (KeptStopUpdate clears it);
	// a stop that is not that run's changes nothing. LastUsed is the latest boot or stop.
	ActiveRun string `json:"active_run,omitempty"`
	// IdleDays is the allowance the latest boot or stop gave ExpiresAt (KeptRecordIdleDays reads it).
	IdleDays int       `json:"idle_days,omitempty"`
	LastUsed time.Time `json:"last_used,omitzero"`
	// Scrubbed is true only when the latest stop ran the guest's scrub; a boot sets it false.
	Scrubbed  bool      `json:"scrubbed"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

// ErrKeptIDMismatch is a kept.json whose id is not the one asked for: two ids shared a folder key.
var ErrKeptIDMismatch = errors.New("macvm: the kept machine folder holds another machine")

// ReadKept reads dir's kept.json (dir is Layout.KeptMachineDir(id)) and checks that it names the full id.
// A missing file wraps os.ErrNotExist.
func ReadKept(dir, id string) (KeptRecord, error) {
	var rec KeptRecord
	b, err := os.ReadFile(filepath.Join(dir, KeptFile))
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return KeptRecord{}, fmt.Errorf("macvm: kept machine record: %w", err)
	}
	if _, err := uuid.Parse(id); err != nil || rec.ID != id {
		return KeptRecord{}, fmt.Errorf("%w: %s holds %q, not %q", ErrKeptIDMismatch, dir, rec.ID, id)
	}
	return rec, nil
}

// WriteKept writes rec as dir's kept.json (0600, atomically), making dir (0700) if need be.
func WriteKept(dir string, rec KeptRecord) error {
	if _, err := uuid.Parse(rec.ID); err != nil {
		return fmt.Errorf("macvm: kept machine id %q is not a UUID", rec.ID)
	}
	if err := mkdirPrivate(dir); err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(dir, KeptFile), rec, 0o600)
}

// KeptLocked reports whether a helper holds dir's boot lock (dir/../<key>.lock) now, without waiting and
// without making anything: a machine that was never booted has no lock file and is not locked. The lock is
// a flock and the kernel offers no way to read it without taking it, so this takes it shared for an instant:
// a caller that LockKept's at that instant is refused (LockKeptWait rides it out). It is a probe, not for hot
// paths or loops; a caller with the record and the ledger should read in-use from them instead.
func KeptLocked(dir string) (bool, error) {
	lock := dir + ".lock"
	if _, err := os.Lstat(lock); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unlock, err := lockFile(ctx, lock, false)
	switch {
	case errors.Is(err, context.Canceled):
		return true, nil
	case err != nil:
		return false, err
	}
	unlock()
	return false, nil
}

// LockKept takes dir's exclusive boot lock in one try, and ErrKeptLocked when another holds it. The helper
// holds it for a boot's whole life; the runner holds it for a delete or a stop rewrite of kept.json, so none
// of them runs beside a boot (KeptLocked is only a probe and excludes nothing). The lock file stays when the
// lock is released; RemoveKept removes it with the folder.
func LockKept(dir string) (unlock func(), err error) {
	unlock, err = LockDisk(dir)
	if errors.Is(err, ErrDiskInUse) {
		return nil, ErrKeptLocked
	}
	return unlock, err
}

// LockKeptWait is LockKept that polls for up to wait before it returns ErrKeptLocked, for a caller whose
// refusal would be spurious if the holder is only a brief write or probe. Delete and erase keep the one-try
// LockKept.
func LockKeptWait(dir string, wait time.Duration) (unlock func(), err error) {
	deadline := time.Now().Add(wait)
	for {
		unlock, err = LockKept(dir)
		if !errors.Is(err, ErrKeptLocked) || !time.Now().Before(deadline) {
			return unlock, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// RemoveKept deletes the kept machine's folder, the old generations an update moved aside
// (RemoveOldGenerations) and then its lock file, holding the lock throughout: ErrKeptLocked while a boot
// holds it, and false with no error when there was no folder.
func RemoveKept(dir string) (bool, error) {
	unlock, err := LockKept(dir)
	if err != nil {
		return false, err
	}
	defer unlock()
	_, serr := os.Lstat(dir)
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	if err := RemoveOldGenerations(dir); err != nil {
		return true, err
	}
	if err := os.Remove(dir + ".lock"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return true, err
	}
	return serr == nil, nil
}

// KeptAllocatedBytes is the space dir's files take on disk (st_blocks, so a sparse or cloned disk counts
// what it has written), for the kept machines inventory.
func KeptAllocatedBytes(dir string) (int64, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, e := range ents {
		var st unix.Stat_t
		if err := unix.Lstat(filepath.Join(dir, e.Name()), &st); err != nil {
			return 0, err
		}
		if st.Mode&unix.S_IFMT == unix.S_IFREG {
			n += st.Blocks * 512
		}
	}
	return n, nil
}

// Why a kept machine did not boot.
var (
	// ErrKeptLocked is a machine another helper on this Mac holds (the runner words it kept_machine_locked).
	ErrKeptLocked = errors.New("macvm: this kept machine is already running on this Mac")
	// ErrKeptOwner is a resume or reset whose owner or runner is not the one that made the machine.
	ErrKeptOwner = errors.New("macvm: this kept machine belongs to another person or registration")
	// ErrKeptExpired is a machine past its expires_at; it is deleted, not booted.
	ErrKeptExpired = errors.New("macvm: this kept machine expired")
	// ErrKeptBaseChanged is a new or fresh boot whose base is no longer the one the runner checked.
	ErrKeptBaseChanged = errors.New("macvm: the macOS base changed since this kept machine's start was checked")
	// ErrKeptIncomplete is a kept folder missing a file a boot needs, or holding one that is not the
	// person's own regular file.
	ErrKeptIncomplete = errors.New("macvm: this kept machine's folder is incomplete")
)

// KeptRefusalFile is written into the run folder when a kept machine's boot is refused, before the helper
// has a ctl.sock to report on: the runner reads it (ReadKeptRefusal) instead of matching the error's text.
const KeptRefusalFile = "kept-refusal.json"

// The kept_refusal values (KeptRefusalCode), one per way a boot is refused.
const (
	KeptRefusalLocked      = "locked"
	KeptRefusalOwner       = "owner"
	KeptRefusalIncomplete  = "incomplete"
	KeptRefusalExpired     = "expired"
	KeptRefusalIDMismatch  = "id_mismatch"
	KeptRefusalBaseChanged = "base_changed"
)

type keptRefusal struct {
	Code string `json:"kept_refusal"`
}

// KeptRefusalCode is the kept_refusal value for err, or "" when err is none of the refusals.
func KeptRefusalCode(err error) string {
	switch {
	case errors.Is(err, ErrKeptLocked):
		return KeptRefusalLocked
	case errors.Is(err, ErrKeptOwner):
		return KeptRefusalOwner
	case errors.Is(err, ErrKeptIncomplete):
		return KeptRefusalIncomplete
	case errors.Is(err, ErrKeptExpired):
		return KeptRefusalExpired
	case errors.Is(err, ErrKeptIDMismatch):
		return KeptRefusalIDMismatch
	case errors.Is(err, ErrKeptBaseChanged):
		return KeptRefusalBaseChanged
	}
	return ""
}

// ReadKeptRefusal is the kept_refusal value runDir's helper left, or "" when it left none.
func ReadKeptRefusal(runDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(runDir, KeptRefusalFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	var r keptRefusal
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	return r.Code, err
}

// BaseDigest is the digest of the base in baseDir a runner pins in KeptRun.BaseDigest and the helper
// compares at a new or fresh boot: BaseStamp, so a prepare, a tools update or an edit each change it.
func BaseDigest(baseDir string) (string, error) { return BaseStamp(baseDir) }

// keptFiles are what a kept folder holds besides kept.json and the tools record.
var keptFiles = []string{DiskFile, AuxFile, MachineIDFile, PairKeyFile, ManifestFile, ToolsRecordFile}

// openMachine makes the disk this boot runs: the run's own clone of the base, or a kept machine's folder
// under its lock. release drops the lock and is safe to call once; the VM must have ended first.
func (r *runner) openMachine() (man Manifest, id Identity, dir string, release func(), err error) {
	defer func() { r.recordKeptRefusal(err) }()
	if r.cfg.Kept == nil {
		man, id, err = cloneReadyBase(r.o.Engine, r.cfg.BaseDir, r.o.RunDir, r.cfg.Identity)
		return man, id, r.o.RunDir, func() {}, err
	}
	dir, err = keptDirOf(r.o.RunDir, r.cfg.Kept.ID)
	if err != nil {
		return man, id, "", nil, err
	}
	man, id, release, err = openKept(r.o.Engine, r.cfg, dir, time.Now().UTC())
	if err != nil {
		return man, id, "", nil, err
	}
	return man, id, dir, release, nil
}

// keptDirOf is the kept machine id's folder for the run folder runDir. It is derived from where the run
// folder sits (root/runs/<key>), never named by run.json, so a run.json cannot point a boot elsewhere.
func keptDirOf(runDir, id string) (string, error) {
	runs := filepath.Dir(filepath.Clean(runDir))
	if filepath.Base(runs) != "runs" {
		return "", fmt.Errorf("macvm: %s is not a run folder under runs/", runDir)
	}
	return Layout{Root: filepath.Dir(runs)}.KeptMachineDir(id), nil
}

// keptBootLockWait is how long a boot waits for the kept machine's lock before it is refused as running: a
// runner's brief record write or a probe holds it for moments, and must not refuse a boot.
var keptBootLockWait = 10 * time.Second

// openKept takes dir's boot lock, then per cfg.Kept.Mode clones the base into dir (new), checks and boots
// what is there (resume), or replaces it with a clone of the current base under the same id (fresh).
// The lock is held until release; an error releases it.
func openKept(e Engine, cfg RunConfig, dir string, now time.Time) (man Manifest, id Identity, release func(), err error) {
	k := cfg.Kept
	if k.OwnerHash == "" || k.RunnerID == "" {
		return man, id, nil, errors.New("macvm: a kept machine boot needs its owner and runner")
	}
	unlock, err := LockKeptWait(dir, keptBootLockWait)
	if err != nil {
		return man, id, nil, err
	}
	release = unlock
	defer func() {
		if err != nil {
			unlock()
			release = nil
		}
	}()
	switch k.Mode {
	case KeptNew:
		// Whatever is there, even a folder with no record, may be a person's: only a clone this call makes is removed.
		if _, serr := os.Lstat(dir); serr == nil {
			return man, id, nil, fmt.Errorf("macvm: kept machine %s already exists", k.ID)
		}
		man, id, err = cloneKept(e, cfg, dir, nil)
		if err != nil {
			_ = os.RemoveAll(dir)
			return man, id, nil, err
		}
		rec := KeptRecord{ID: k.ID, OwnerHash: k.OwnerHash, RunnerID: k.RunnerID, Base: k.Base, Name: k.Name,
			BasePreparedAt: man.CreatedAt, CreatedAt: now}
		markKeptBoot(&rec, cfg.RunID, now, k.IdleDays)
		err = WriteKept(dir, rec)
		if err != nil {
			_ = os.RemoveAll(dir)
		}
		return man, id, release, err
	case KeptResume, KeptFresh:
		var rec KeptRecord
		if rec, err = checkKept(dir, k, now); err != nil {
			return man, id, nil, err
		}
		rec.Name = cmp.Or(k.Name, rec.Name) // the organisation's rename, if it sent one
		if k.Mode == KeptFresh {
			man, id, err = resetKept(e, cfg, dir, rec, now)
			return man, id, release, err
		}
		man, id, err = readKeptDisk(dir, cfg.Identity)
		if err != nil {
			return man, id, nil, err
		}
		markKeptBoot(&rec, cfg.RunID, now, k.IdleDays)
		err = WriteKept(dir, rec)
		return man, id, release, err
	}
	return man, id, nil, fmt.Errorf("macvm: kept machine mode %q", k.Mode)
}

// KeptDefaultIdleDays is the idle allowance when a boot or stop is given none.
const KeptDefaultIdleDays = 30

// keptIdleDays is idleDays with the default for a value that is not positive.
func keptIdleDays(idleDays int) int {
	if idleDays <= 0 {
		return KeptDefaultIdleDays
	}
	return idleDays
}

func keptExpiry(now time.Time, idleDays int) time.Time {
	return now.AddDate(0, 0, keptIdleDays(idleDays))
}

// KeptRecordIdleDays is the idle allowance rec was last given, for a runner that recovers a stop with no
// network: the stored IdleDays, else the span from LastUsed to ExpiresAt in whole days (1 to 90) for a record
// written before the field, else KeptDefaultIdleDays.
func KeptRecordIdleDays(rec KeptRecord) int {
	switch {
	case rec.IdleDays > 0:
		return rec.IdleDays
	case !rec.LastUsed.IsZero() && !rec.ExpiresAt.IsZero():
		return min(max(int(math.Round(rec.ExpiresAt.Sub(rec.LastUsed).Hours()/24)), 1), 90)
	}
	return KeptDefaultIdleDays
}

// markKeptBoot is what every boot writes before its VM starts, after the expiry check: the run that holds the
// machine, a fresh idle allowance (so a machine in use cannot expire under its run), and scrubbed false, since
// the run is about to write what a scrub removes.
func markKeptBoot(rec *KeptRecord, runID string, now time.Time, idleDays int) {
	rec.ActiveRun, rec.LastUsed, rec.ExpiresAt, rec.Scrubbed = runID, now, keptExpiry(now, idleDays), false
	rec.IdleDays = keptIdleDays(idleDays)
}

// KeptStopUpdate is the stop's change to rec, for the runner to make under LockKept: it returns false and
// changes nothing unless rec.ActiveRun is runID, the run the stop belongs to. Otherwise the run no longer
// holds the machine, and its idle allowance restarts from now whether or not the boot's expiry has passed,
// so a machine used past its old expiry is not deleted the moment it stops.
func KeptStopUpdate(rec *KeptRecord, runID string, now time.Time, idleDays int) (ok bool) {
	if runID == "" || rec.ActiveRun != runID {
		return false
	}
	rec.ActiveRun, rec.LastUsed, rec.ExpiresAt, rec.IdleDays = "", now, keptExpiry(now, idleDays), keptIdleDays(idleDays)
	return true
}

// checkKept reads dir's record and refuses a boot by anyone but the machine's owner on its runner, and one
// past its expiry.
func checkKept(dir string, k *KeptRun, now time.Time) (KeptRecord, error) {
	rec, err := ReadKept(dir, k.ID)
	if err != nil {
		return rec, err
	}
	if rec.OwnerHash != k.OwnerHash || rec.RunnerID != k.RunnerID {
		return rec, ErrKeptOwner
	}
	if !rec.ExpiresAt.IsZero() && now.After(rec.ExpiresAt) {
		return rec, ErrKeptExpired
	}
	return rec, nil
}

// cloneKept clones the base into dir, with its manifest and tools record, under one hold of the base's
// lock so all come from the same generation. The manifest copy is what a resume boots by, so a later base
// does not change a machine.
//
// A pinned BaseDigest is compared under that lock, and prep (a reset's removal of the old machine) runs only
// after it matches and before the clone, so a base that moved leaves an existing disk untouched.
func cloneKept(e Engine, cfg RunConfig, dir string, prep func() error) (Manifest, Identity, error) {
	unlock, err := lockFile(context.Background(), baseLockPath(cfg.BaseDir), false)
	if err != nil {
		return Manifest{}, Identity{}, err
	}
	defer unlock()
	man, err := ReadyBase(cfg.BaseDir)
	if err != nil {
		return man, Identity{}, err
	}
	if want := cfg.Kept.BaseDigest; want != "" {
		if got, err := BaseDigest(cfg.BaseDir); err != nil || got != want {
			return man, Identity{}, errors.Join(ErrKeptBaseChanged, err)
		}
	}
	if prep != nil {
		if err := prep(); err != nil {
			return man, Identity{}, err
		}
	}
	id, err := cloneRun(e, cfg.BaseDir, dir, cfg.Identity)
	if err != nil {
		return man, id, err
	}
	if err := copyToolsRecord(cfg.BaseDir, dir); err != nil {
		return man, id, err
	}
	return man, id, writeJSONAtomic(filepath.Join(dir, ManifestFile), man, 0o600)
}

// resetKept replaces dir's machine with a clone of the current base, keeping its id, owner and creation
// time. The record stays valid throughout, so a cut-short reset can be reset again.
func resetKept(e Engine, cfg RunConfig, dir string, rec KeptRecord, now time.Time) (Manifest, Identity, error) {
	man, id, err := cloneKept(e, cfg, dir, func() error {
		for _, f := range keptFiles {
			if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return man, id, err
	}
	rec.Base, rec.BasePreparedAt = cfg.Kept.Base, man.CreatedAt
	rec.LastStop, rec.StopKind = time.Time{}, ""
	markKeptBoot(&rec, cfg.RunID, now, cfg.Kept.IdleDays)
	return man, id, WriteKept(dir, rec)
}

// readKeptDisk reads what a resume boots: the folder's manifest copy and its own machine-id. Disk, aux
// storage and key must be the person's regular files.
func readKeptDisk(dir string, s IdentityStrategy) (Manifest, Identity, error) {
	man, err := ReadyBase(dir)
	if err != nil {
		return man, Identity{}, fmt.Errorf("%w: %v", ErrKeptIncomplete, err)
	}
	for _, f := range []string{DiskFile, AuxFile, MachineIDFile} {
		fi, err := os.Lstat(filepath.Join(dir, f))
		if err == nil && (!fi.Mode().IsRegular() || checkOwner(fi) != nil) {
			err = errors.New("not this user's regular file")
		}
		if err != nil {
			return man, Identity{}, fmt.Errorf("%w: %s: %v", ErrKeptIncomplete, f, err)
		}
	}
	if _, err := readPairKey(dir); err != nil {
		return man, Identity{}, fmt.Errorf("%w: %v", ErrKeptIncomplete, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, MachineIDFile))
	if err != nil || len(b) == 0 {
		return man, Identity{}, fmt.Errorf("%w: machine-id", ErrKeptIncomplete)
	}
	return man, Identity{Strategy: s, MachineIdentifier: b}, nil
}

// recordKeptRefusal leaves the kept_refusal code of a refused boot in the run folder; any other error leaves none.
func (r *runner) recordKeptRefusal(err error) {
	code := KeptRefusalCode(err)
	if code == "" {
		return
	}
	if werr := writeJSONAtomic(filepath.Join(r.o.RunDir, KeptRefusalFile), keptRefusal{code}, 0o600); werr != nil {
		r.o.Log.Error("macvm: recording a kept machine refusal failed", "run_id", r.cfg.RunID, "err", werr.Error())
	}
}
