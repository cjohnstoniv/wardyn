// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// The staging and swap half of a tools update: the update lock and staging copy, and the exchange that
// puts the updated copy in place of a base or of a kept machine's folder.

// beginUpdate takes base's update lock for a tools update or a re-prepare and
// sweeps what a crashed one left in the staging dir <base>.update (a half-built
// base, or an old generation after a swap), whether or not the caller goes on
// to build. It returns the staging dir; done deletes it (after a swap, the old
// generation, so that goes once the base lock is released) and then drops the
// update lock.
//
// A delete that fails is logged and the dir left, as a crash leaves it: one
// staging dir per slot that no ledger entry reserves. It occupies live free
// space, which admission reads at every start, so it only makes admission
// refuse sooner, until the next prepare, tools update or runnerd start
// deletes it.
func beginUpdate(ctx context.Context, base string, log *slog.Logger) (staging string, done func(), err error) {
	staging = base + ".update"
	unlock, err := lockFile(ctx, updateLockPath(base), true)
	if err != nil {
		return "", nil, err
	}
	if err := os.RemoveAll(staging); err != nil {
		unlock()
		return "", nil, err
	}
	return staging, func() {
		if err := os.RemoveAll(staging); err != nil {
			log.Warn("macvm: the update's staging dir was not deleted; the next update sweeps it", "path", staging, "err", err.Error())
		}
		unlock()
	}, nil
}

// swapBase exchanges the updated base nb with base while holding base's lock
// exclusively; nb then holds the old generation.
func swapBase(ctx context.Context, nb, base string) error {
	unlock, err := lockFile(ctx, baseLockPath(base), true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := swapFn(nb, base); err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS) {
			return fmt.Errorf("macvm: the macOS VM directory must be on an APFS volume, which can swap two directories at once (%v); the base was left as it was", err)
		}
		return fmt.Errorf("macvm: swapping in the updated base: %w", err)
	}
	// The post-swap file-set check, still under the exclusive lock: the new
	// base holds only a base's own files, so nothing a stage-2 boot left
	// (a boot configuration, a socket, a log) reaches a run's clone. If it
	// holds anything else, the old generation is swapped back.
	if err := checkBaseFiles(base); err != nil {
		if berr := swapFn(nb, base); berr != nil {
			return fmt.Errorf("%w; swapping the old base back failed too: %v", err, berr)
		}
		return fmt.Errorf("%w; the old base was kept", err)
	}
	return nil
}

// baseFiles are the files a base dir holds, and requiredBaseFiles those it must (tools.json and admin-password may be absent).
var baseFiles, requiredBaseFiles = []string{DiskFile, AuxFile, MachineIDFile, ManifestFile, PairKeyFile, AdminPasswordFile, ToolsRecordFile},
	[]string{DiskFile, AuxFile, MachineIDFile, ManifestFile, PairKeyFile}

// checkBaseFiles fails unless dir holds the base's own files only, each a
// regular file, with the disk, aux storage, identifier and manifest present.
func checkBaseFiles(dir string) error {
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, de := range des {
		if !slices.Contains(baseFiles, de.Name()) || !de.Type().IsRegular() {
			return fmt.Errorf("macvm: the new macOS base holds %q, which is not a base file", de.Name())
		}
		seen[de.Name()] = true
	}
	for _, f := range requiredBaseFiles {
		if !seen[f] {
			return fmt.Errorf("macvm: the new macOS base has no %s", f)
		}
	}
	return nil
}

// swapFn is swapDirs; tests replace it to force a failing exchange. There is
// no fallback to two renames: a base that is half swapped is not a base.
var swapFn = swapDirs

// lockKept checks that o.KeptDir is a kept machine's folder of o.Layout, takes its lock and returns its
// record, read under the lock. The check comes first so that no lock file is made for a folder that is not
// one; a base is never a kept folder, since KeptMachineDir is under the layout's kept/.
func (o UpdateOptions) lockKept() (dir string, rec KeptRecord, unlock func(), err error) {
	dir = filepath.Clean(o.KeptDir)
	if filepath.Dir(dir) != o.Layout.KeptDir() {
		return "", rec, nil, fmt.Errorf("macvm: %s is not a kept machine folder of this Mac", o.KeptDir)
	}
	if rec, err = readKeptAt(o.Layout, dir); err != nil {
		return "", rec, nil, err
	}
	if unlock, err = LockKept(dir); err != nil {
		return "", rec, nil, err
	}
	// Whatever a delete or a reset did before the lock is read again.
	if rec, err = readKeptAt(o.Layout, dir); err != nil {
		unlock()
		return "", rec, nil, err
	}
	return dir, rec, unlock, nil
}

// readKeptAt reads dir's kept.json and checks that the record's id is the one dir is the folder of.
func readKeptAt(l Layout, dir string) (KeptRecord, error) {
	b, err := os.ReadFile(filepath.Join(dir, KeptFile))
	if err != nil {
		return KeptRecord{}, err
	}
	var id struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &id); err != nil {
		return KeptRecord{}, fmt.Errorf("macvm: kept machine record: %w", err)
	}
	if _, err := uuid.Parse(id.ID); err != nil || l.KeptMachineDir(id.ID) != dir {
		return KeptRecord{}, fmt.Errorf("%w: %s holds %q", ErrKeptIDMismatch, dir, id.ID)
	}
	return ReadKept(dir, id.ID)
}

// cloneKeptStaging is cloneStaging for a kept machine: it clones the folder's disk and aux storage into
// staging with the machine's own identifier and pair key, as a boot of that folder would use them. It
// carries no admin password: a kept folder holds none.
func cloneKeptStaging(e Engine, dir, staging string) (Identity, error) {
	return cloneRun(e, dir, staging, IdentityTravel)
}

// swapKept exchanges the updated kept machine nb with its folder dir; the caller holds dir's LockKept, so
// no boot, delete or other update runs beside it, and nb then holds the old generation. If the new folder
// holds anything but a kept machine's own files, the old generation is swapped back.
func swapKept(nb, dir string) error {
	if err := swapFn(nb, dir); err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS) {
			return fmt.Errorf("macvm: the macOS VM directory must be on an APFS volume, which can swap two directories at once (%v); the kept machine was left as it was", err)
		}
		return fmt.Errorf("macvm: swapping in the updated kept machine: %w", err)
	}
	if err := checkKeptFiles(dir); err != nil {
		if berr := swapFn(nb, dir); berr != nil {
			return fmt.Errorf("%w; swapping the old kept machine back failed too: %v", err, berr)
		}
		return fmt.Errorf("%w; the old kept machine was kept", err)
	}
	return nil
}

// checkKeptFiles fails unless dir holds a kept machine's own files only, each a regular file, with the
// disk, aux storage, identifier, pair key, manifest and record present.
func checkKeptFiles(dir string) error {
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, de := range des {
		if (de.Name() != KeptFile && !slices.Contains(keptFiles, de.Name())) || !de.Type().IsRegular() {
			return fmt.Errorf("macvm: the new kept machine holds %q, which is not a kept machine file", de.Name())
		}
		seen[de.Name()] = true
	}
	for _, f := range []string{DiskFile, AuxFile, MachineIDFile, PairKeyFile, ManifestFile, KeptFile} {
		if !seen[f] {
			return fmt.Errorf("macvm: the new kept machine has no %s", f)
		}
	}
	return nil
}
