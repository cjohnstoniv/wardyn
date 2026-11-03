// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	// IdleDays moves expires_at that far past each stop.
	IdleDays int `json:"idle_days,omitempty"`
}

// KeptRecord is kept.json: what the Mac knows of one kept machine, readable with no network.
type KeptRecord struct {
	ID             string    `json:"id"`
	OwnerHash      string    `json:"owner_hash"`
	RunnerID       string    `json:"runner_id"`
	Base           string    `json:"base,omitempty"`
	BasePreparedAt time.Time `json:"base_prepared_at"`
	CreatedAt      time.Time `json:"created_at"`
	// LastStop and StopKind (KeptStopGraceful or KeptStopHard) describe the latest stop; both are empty
	// until the machine has stopped once.
	LastStop time.Time `json:"last_stop,omitzero"`
	StopKind string    `json:"stop_kind,omitempty"`
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
// without making anything: a machine that was never booted has no lock file and is not locked. The probe
// takes the lock shared for an instant, so a helper starting at that instant may be told the machine is busy.
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
