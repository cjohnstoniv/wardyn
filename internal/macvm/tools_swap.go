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
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// The staging and swap half of a tools update: the update lock and staging copy, and the exchange that
// puts the updated copy in place of a base or of a kept machine's folder.

// ErrToolsUpdateBusy is a tools update or a derivation that finds base's update lock held. Its text is what the person reads, so it is a sentence, as the edit-base refusals are.
const ErrToolsUpdateBusy ToolsUpdateRefusal = "A tools update of this macOS base is already running."

// ToolsUpdateRefusal is why a tools update starts nothing.
type ToolsUpdateRefusal string

func (r ToolsUpdateRefusal) Error() string { return string(r) }

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

// beginUpdateOnce is beginUpdate with one try at the lock: held by another update or a prepare, it returns
// ErrToolsUpdateBusy at once. A helper that waited here would sit with no host ledger entry, taking a VM
// slot's place for as long as the holder runs. A caller whose own ctx ended gets that error, not the busy one.
func beginUpdateOnce(ctx context.Context, base string, log *slog.Logger) (staging string, done func(), err error) {
	try, cancel := context.WithCancel(ctx)
	cancel()
	staging, done, err = beginUpdate(try, base, log)
	if errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return "", nil, ErrToolsUpdateBusy
	}
	return staging, done, err
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
			return fmt.Errorf("%w; swapping the old base back failed too: %v; %s", err, berr, keepOldGeneration(nb, "base"))
		}
		return fmt.Errorf("%w; the old base was kept", err)
	}
	return nil
}

// keepOldGeneration moves the staging dir nb, which holds the old generation after an exchange whose swap
// back failed, to a name beside it that neither the update's clean-up (beginUpdate's done and its sweep)
// nor runnerd's start-up sweep deletes, and says where the old generation is.
func keepOldGeneration(nb, what string) string {
	kept := nb + oldGenerationSuffix + uuid.NewString()
	if err := os.Rename(nb, kept); err != nil {
		return fmt.Sprintf("moving the old %s aside failed too (%v), so the update's clean-up deletes it", what, err)
	}
	return fmt.Sprintf("the old %s is kept at %s", what, kept)
}

// oldGenerationSuffix is what keepOldGeneration adds to a staging dir's name: <dir>.update.old-<id>.
const oldGenerationSuffix = ".old-"

// RemoveOldGenerations deletes the old generations of dir (a base or a kept machine's folder) that
// keepOldGeneration moved aside, so a kept machine's delete and expiry take its old disks with it.
func RemoveOldGenerations(dir string) error {
	parent, prefix := filepath.Dir(dir), filepath.Base(dir)+".update"+oldGenerationSuffix
	des, err := os.ReadDir(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, de := range des {
		if strings.HasPrefix(de.Name(), prefix) {
			if err := os.RemoveAll(filepath.Join(parent, de.Name())); err != nil {
				return err
			}
		}
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

// updateSite is what an update works on: the base in a slot, or one kept machine's folder with its record,
// read under the lock the update holds.
type updateSite struct {
	dir  string
	kept bool
	rec  KeptRecord
}

// openSite finds o's site. For a kept machine it checks that o.KeptDir is a folder of o.Layout's kept
// machines, takes its lock and returns the unlock; the check comes first so that no lock file is made for a
// folder that is not one. A base is never a kept folder, since KeptMachineDir is under the layout's kept/.
func (o UpdateOptions) openSite() (s updateSite, unlock func(), err error) {
	if o.KeptDir == "" {
		return updateSite{dir: o.Layout.BaseDir(o.Slot)}, func() {}, nil
	}
	s = updateSite{dir: filepath.Clean(o.KeptDir), kept: true}
	if filepath.Dir(s.dir) != o.Layout.KeptDir() {
		return s, nil, fmt.Errorf("macvm: %s is not a kept machine folder of this Mac", o.KeptDir)
	}
	if _, err = readKeptAt(o.Layout, s.dir); err != nil {
		return s, nil, err
	}
	if unlock, err = LockKept(s.dir); err != nil {
		return s, nil, err
	}
	// Whatever a delete or a reset did before the lock is read again.
	if s.rec, err = readKeptAt(o.Layout, s.dir); err != nil {
		unlock()
		return s, nil, err
	}
	return s, unlock, nil
}

// clone makes the staging copy nb of the site: a base's with its admin password, a kept machine's with its
// own identifier and pair key and none (a kept folder holds none).
func (s updateSite) clone(e Engine, nb string) (Identity, error) {
	if s.kept {
		return cloneRun(e, s.dir, nb, IdentityTravel)
	}
	return cloneStaging(e, s.dir, nb)
}

// swap puts the updated copy nb in the site's place. A kept machine's record goes in first, with scrubbed
// false: the stage-2 boot wrote to its disk, and the next boot's scrub removes what it holds of.
func (s updateSite) swap(ctx context.Context, nb string) error {
	if !s.kept {
		return swapBase(ctx, nb, s.dir)
	}
	s.rec.Scrubbed = false
	if err := WriteKept(nb, s.rec); err != nil {
		return err
	}
	return swapKept(nb, s.dir)
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
			return fmt.Errorf("%w; swapping the old kept machine back failed too: %v; %s", err, berr, keepOldGeneration(nb, "kept machine"))
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
