// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// keptToolsUpdateLog, in the VM directory, is the last kept machine tools update's output.
const keptToolsUpdateLog = "kept-tools-update.log"

// keptToolsUpdater brings a kept machine's guest agent and pinned harness CLIs up to this Mac's, on the
// machine's own disk and tools record. It is the Docker driver's KeptToolsUpdater (KeptConfig.Tools).
type keptToolsUpdater struct {
	m *macOSVMs
	// start runs one helper job to its end (macOSVMs.startJob; tests replace it).
	start func(ctx context.Context, j helperJob) error
}

// newKeptToolsUpdater is the updater for m's Mac, for the wiring to pass as KeptConfig.Tools.
func newKeptToolsUpdater(m *macOSVMs) *keptToolsUpdater {
	return &keptToolsUpdater{m: m, start: m.startJob}
}

// UpdateKeptTools updates the kept machine in keptDir (Layout.KeptMachineDir) when its tools record is
// behind this Mac's, and returns once the helper has finished; nil when it is current. It runs the helper's
// update-tools on that folder only, never on the base. The helper takes the machine's lock, so the caller
// must not hold it; macvm.ErrKeptLocked says a boot, a delete or another update holds it. The old disk
// stays as it was when the update fails.
func (u *keptToolsUpdater) UpdateKeptTools(ctx context.Context, keptDir string) error {
	return u.update(ctx, keptDir, false)
}

// ForceKeptTools is UpdateKeptTools for an update someone asked for: it runs even when the machine looks
// current, and retries every withheld harness (`macos-vm machines update <id> --force`).
func (u *keptToolsUpdater) ForceKeptTools(ctx context.Context, keptDir string) error {
	return u.update(ctx, keptDir, true)
}

func (u *keptToolsUpdater) update(ctx context.Context, keptDir string, force bool) error {
	m := u.m
	if m.ledger == nil {
		return errors.New(cmp.Or(m.refusal, m.why))
	}
	dir := filepath.Clean(keptDir)
	if filepath.Dir(dir) != m.layout.KeptDir() {
		return fmt.Errorf("%s is not a kept machine folder of this Mac", keptDir)
	}
	man, err := macvm.ReadyBase(dir)
	if err != nil {
		return fmt.Errorf("the kept machine's tools cannot be updated: %w", err)
	}
	if !force {
		behind, err := u.behind(dir)
		if err != nil || !behind {
			return err
		}
	}
	// The helper takes the lock itself; this only avoids a job and a ledger entry for a machine that is
	// in use. It waits a short while for a holder (a stop write is not a boot), and holds nothing after.
	unlock, err := macvm.LockKeptWait(dir, keptLockWait)
	if err != nil {
		return err
	}
	unlock()
	extra, err := m.baseFileArgs([]string{"--" + macvm.FlagKept, dir})
	if err != nil {
		return err
	}
	if force {
		extra = append(extra, "--"+macvm.FlagForce)
	}
	err = u.start(ctx, helperJob{kind: admission.KindToolsUpdate, extra: extra, disk: admission.RunDiskReservation(cmp.Or(man.DiskBytes, macvm.BaseDiskBytes)),
		log: filepath.Join(m.layout.Root, keptToolsUpdateLog), sub: macvm.UpdateToolsCommand, helper: m.helper, cpus: max(man.MinCPUs, 1),
		memMiB: m.guestMemory(man) >> 20, runner: m.runnerID, ledger: m.ledgerDir, descr: "kept machine tools update", out: io.Discard})
	// The helper answers in words; the one outcome the caller tells apart keeps its identity.
	if err != nil && strings.Contains(err.Error(), macvm.ErrKeptLocked.Error()) {
		return macvm.ErrKeptLocked
	}
	return err
}

// behind says the machine in dir lacks this Mac's pins, guest agent build or install list, as the helper
// decides it (macvm.UpdateTools without Force).
func (u *keptToolsUpdater) behind(dir string) (bool, error) {
	m := u.m
	if m.items.err != nil {
		return false, fmt.Errorf("macos_vm_base_file: %w", m.items.err)
	}
	t, err := macvm.ShippedTooling()
	if err != nil {
		return false, err
	}
	var l macvm.BaseList
	if m.items.file != "" {
		if l, err = macvm.LoadBaseList(m.items.file); err != nil {
			return false, err
		}
	}
	return macvm.NeedsToolsUpdate(dir, t, m.version, time.Now()) || macvm.ItemsChanged(dir, l.Digest()), nil
}
