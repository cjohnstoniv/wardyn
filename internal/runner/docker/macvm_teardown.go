// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// macVMStop is the macOS VM's part of a stop or a lease end: the VM is stopped (gracefully first when
// asked) and destroyed, since a macOS VM keeps no disk past its run; a kept machine's disk is not the
// run's, and stays (keptStopped).
func (d *Driver) macVMStop(ctx context.Context, v *vmRun, graceful bool) {
	cfg, err := readMacRun(v.dir, v.runID)
	if err != nil {
		return // already destroyed
	}
	d.destroyMacVM(ctx, v, cfg.LedgerEntry, graceful)
}

// destroyMacVM ends a macOS VM in the design's teardown order: the helper stops the VM (asking the guest
// first when graceful, then hard), the helper exits (or is killed), the run directory is deleted, and only
// then is the ledger entry released, by the helper's stop report, or with no helper ever having claimed
// it (the helper claims before it builds a VM, and a claim after the release finds no entry), or else by
// the ledger's own check that no VM process of this user can still be it. A step that fails leaves the
// entry held, and the next orphan sweep retries it. It runs on a context cut loose from the call's, which
// the organisation cancels when it gives up on a slow call, and bounded by macDestroyTimeout instead.
func (d *Driver) destroyMacVM(ctx context.Context, v *vmRun, entryID string, graceful bool) {
	c := d.macVMConfig()
	if c == nil {
		return
	}
	ctx, done := context.WithTimeout(context.WithoutCancel(ctx), macDestroyTimeout)
	defer done()
	log := slog.With(slog.String("run_id", v.runID.String()))
	// A kept machine's guest scrubs what its run wrote before a graceful stop; a hard stop scrubs at the next boot.
	var scrub *runner.KeptScrub
	k := c.keptMachineOf(v)
	if k != nil && graceful {
		scrub = keptStopScrub(ctx, v)
	}
	if _, err := c.Ledger.Update(entryID, func(e *admission.Entry) { e.State = admission.StateStopping }); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("docker: mark a macOS VM stopping in the ledger", slog.String("err", err.Error()))
	}
	stopped := false
	sctx, cancel := context.WithTimeout(ctx, macvm.DefaultStopTimeout+macHelperExitWait)
	if cc, err := c.dialControl(sctx, v.runID); err == nil {
		stopped = cc.Stop(sctx, graceful, macvm.DefaultStopTimeout) == nil
		_ = cc.Close()
	}
	cancel()
	if e, ok := c.entry(entryID); ok && !d.waitMacVMHelperExit(ctx, c, e) {
		log.Warn("docker: the macOS VM helper did not exit; killing it")
		d.endMacVMHelper(v, syscall.SIGKILL)
		if !d.waitMacVMHelperExit(ctx, c, e) {
			d.recordMacOSVMDisk(v.runID, runner.MacOSVMDiskKept, runner.MacOSVMDiskHelperAlive)
			if ctx.Err() != nil {
				log.Error("docker: the macOS VM destroy ran out of time before the helper exited; its ledger entry stays held")
			} else {
				log.Error("docker: the macOS VM helper outlived SIGKILL; its ledger entry stays held")
			}
			return
		}
	}
	// A kept machine's stop is recorded while the run folder is still there, so a sweep never reads the
	// session as interrupted.
	var keptExpiry time.Time
	if k != nil {
		scrub, keptExpiry = c.keptStopped(k, v.runID, scrub)
	}
	if err := c.removeAll(v.dir); err != nil {
		log.Warn("docker: delete a macOS VM run directory (pending; the orphan sweep retries)", slog.String("err", err.Error()))
		d.recordMacOSVMDisk(v.runID, runner.MacOSVMDiskKept, runner.MacOSVMDiskRemoveFailed)
		return
	}
	if k != nil {
		d.keptMachineDisk(v.runID, scrub, keptExpiry)
	} else {
		d.recordMacOSVMDisk(v.runID, runner.MacOSVMDiskRemoved, "")
	}
	c.sweepSockResidue()
	var err error
	if e, ok := c.entry(entryID); stopped || ok && e.HelperPID == 0 {
		err = c.Ledger.Release(entryID, admission.Destroyed{Stopped: true, RunDir: v.dir})
	} else {
		err = c.Ledger.ReclaimOwn(entryID, v.dir)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("docker: release a macOS VM's ledger entry (the orphan sweep retries)", slog.String("err", err.Error()))
	}
	c.changed()
}

// waitMacVMHelperExit waits for e's helper to leave the process table.
func (d *Driver) waitMacVMHelperExit(ctx context.Context, c *MacVMConfig, e admission.Entry) bool {
	deadline := time.Now().Add(cmp.Or(c.helperWait, macHelperExitWait))
	for c.helperAlive(e) {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// teardownMacVM is teardownVM's macOS VM part: destroy the run's VM, if it is one, whatever is left of it.
// It reports whether the run was a macOS VM.
func (d *Driver) teardownMacVM(ctx context.Context, runID uuid.UUID) bool {
	c := d.macVMConfig()
	if c == nil {
		return false
	}
	dir := c.runDir(runID)
	cfg, err := readMacRun(dir, runID)
	if err != nil {
		return false
	}
	v := newMacVMRun(c, runID, dir)
	defer func() { v.stop(); v.agent.Close() }()
	d.destroyMacVM(ctx, v, cfg.LedgerEntry, false)
	return true
}
