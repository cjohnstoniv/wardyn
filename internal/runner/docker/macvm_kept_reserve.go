// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The Mac's kept limit is reserved, not only checked: a new machine or a reset writes <key>.reserve, under
// one lock for the whole Mac (kept/.reserve.lock), holding its disk's worst case, before its helper starts.
// The Mac's total is read in one pass under that lock (keptMacBytes): a key counts its reservation while
// the reservation is there, else its record, so a record the helper publishes meanwhile never hides a
// start. Only the runner removes a reservation, under the lock: once it has confirmed the clone against
// the Mac's total (keptConfirmReserve), when its create ends, or, by the sweep, once its create is gone.
const (
	keptReserveSuffix = ".reserve"
	keptReserveLock   = ".reserve"
	// keptReserveWait bounds the wait for the Mac's reservation lock.
	keptReserveWait = 10 * time.Second
)

// keptReservation is a <key>.reserve file.
type keptReservation struct {
	// Bytes is the key's worst case while the start runs: the clone's, or for a reset the larger of the
	// clone's and the disk's it replaces (Prior).
	Bytes int64 `json:"bytes"`
	Prior int64 `json:"prior,omitempty"`
	// RunDir is the run folder of the create that holds it.
	RunDir string `json:"run_dir"`
}

// lockKeptReserve takes the Mac's reservation lock, waiting up to keptReserveWait for another start.
func (c *MacVMConfig) lockKeptReserve() (func(), error) {
	deadline := time.Now().Add(keptReserveWait)
	for {
		unlock, err := macvm.LockKept(filepath.Join(c.Layout.KeptDir(), keptReserveLock))
		if !errors.Is(err, macvm.ErrKeptLocked) || time.Now().After(deadline) {
			return unlock, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readKeptReserve reads dir's reservation; false when there is none.
func readKeptReserve(dir string) (keptReservation, bool) {
	var r keptReservation
	b, err := os.ReadFile(dir + keptReserveSuffix)
	return r, err == nil && json.Unmarshal(b, &r) == nil
}

// keptMacBytes is the Mac's kept disk at its worst but except's, in one pass under the reservation lock:
// every key a folder or a reservation names counts its reservation while there is one, else its record.
func (c *MacVMConfig) keptMacBytes(except string) (int64, error) {
	des, err := os.ReadDir(c.Layout.KeptDir())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("docker: read the kept machines: %w", err)
	}
	if c.keptListed != nil {
		c.keptListed()
	}
	var n int64
	seen := map[string]bool{}
	for _, de := range des {
		key, reserve := strings.CutSuffix(de.Name(), keptReserveSuffix)
		dir := filepath.Join(c.Layout.KeptDir(), key)
		if !isKeptKey(key) || !reserve && !de.IsDir() || seen[key] || dir == except {
			continue
		}
		seen[key] = true
		if r, ok := readKeptReserve(dir); ok {
			n += r.Bytes
		} else if _, ok := c.keptFolderAt(dir); ok {
			n += keptWorstOf(dir)
		}
	}
	return n, nil
}

// reserveKept reserves spec's new machine or reset against the Mac's kept limit: its worst case, a new
// machine's clone or the larger of a reset's clone and the disk it replaces. It refuses a start that would
// pass the limit. The base is read again under the lock, into man, and pinned in kept by its digest: the
// helper clones only that base and refuses the boot, before it touches a reset's old disk, when the base
// has changed since (macvm.ErrKeptBaseChanged). release drops the reservation if the confirm did not.
func (c *MacVMConfig) reserveKept(spec runner.SandboxSpec, man *macvm.Manifest, kept *macvm.KeptRun) (release func(), err error) {
	k := spec.KeptMachine
	dir := c.keptDir(k.ID)
	unlock, err := c.lockKeptReserve()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := c.pinKeptBase(spec, man, kept); err != nil {
		return nil, err
	}
	r := keptReservation{Bytes: keptWorst(*man), RunDir: c.runDir(spec.RunID)}
	if k.Mode == macvm.KeptFresh {
		r.Prior = keptWorstOf(dir)
		r.Bytes = max(r.Bytes, r.Prior)
	}
	held, err := c.keptMacBytes(dir)
	if err != nil {
		return nil, err
	}
	if held+r.Bytes > c.LimitBytes {
		return nil, keptSaidMacLimit(c.LimitBytes, r.Bytes-r.Prior)
	}
	b, _ := json.Marshal(r)
	if err := os.WriteFile(dir+keptReserveSuffix, b, 0o600); err != nil {
		return nil, fmt.Errorf("docker: reserve the kept machine's disk: %w", err)
	}
	return func() { c.dropKeptReserve(dir) }, nil
}

// dropKeptReserve removes dir's reservation under the reservation lock; one the lock cannot be had for is
// left to the sweep.
func (c *MacVMConfig) dropKeptReserve(dir string) {
	unlock, err := c.lockKeptReserve()
	if err != nil {
		slog.Warn("docker: drop a kept machine's disk reservation (the sweep retries)", slog.String("err", err.Error()))
		return
	}
	defer unlock()
	_ = os.Remove(dir + keptReserveSuffix)
}

// pinKeptBase reads the run's base, its digest before and after, into man and kept.BaseDigest; a base
// replaced while it is read is refused as changed. The base is checked against the run again.
func (c *MacVMConfig) pinKeptBase(spec runner.SandboxSpec, man *macvm.Manifest, kept *macvm.KeptRun) error {
	base, err := c.Layout.NamedBaseDir(spec.MacOSBase)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	digest, err := macvm.BaseDigest(base)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	m, err := macvm.ReadyBase(base)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	if again, err := macvm.BaseDigest(base); err != nil || again != digest {
		return keptRefusal(keptSaidBase)
	}
	if err := macVMFits(spec, m); err != nil {
		return err
	}
	*man, kept.BaseDigest = m, digest
	return nil
}

// keptConfirmReserve turns a new machine's or a reset's reservation into its record once the helper has
// cloned, under the reservation lock: the Mac's total with the disk it cloned in place of the reservation
// must still fit, a second line behind the pinned base digest (pinKeptBase). A clone that does not fit is
// refused and its reservation stays until the create's rollback is done (release): a new machine is
// dropped first, and a reset's disk, which the helper has replaced, is then counted by its record.
func (c *MacVMConfig) keptConfirmReserve(runDir string, runID uuid.UUID) error {
	cfg, err := readMacRun(runDir, runID)
	if err != nil || cfg.Kept == nil || cfg.Kept.Mode == macvm.KeptResume {
		return nil
	}
	dir := c.Layout.KeptMachineDir(cfg.Kept.ID)
	unlock, err := c.lockKeptReserve()
	if err != nil {
		return err
	}
	defer unlock()
	r, ok := readKeptReserve(dir)
	if !ok || r.RunDir != runDir {
		return nil // not this create's to confirm
	}
	held, err := c.keptMacBytes(dir)
	if err != nil {
		return err
	}
	if cloned := keptWorstOf(dir); held+cloned > c.LimitBytes {
		return keptSaidMacLimit(c.LimitBytes, max(0, cloned-r.Prior))
	}
	return os.Remove(dir + keptReserveSuffix)
}

// sweepKeptReservations drops a reservation whose create is gone: its run folder is not on the Mac and
// it is older than a create's wait for its helper.
func (c *MacVMConfig) sweepKeptReservations() error {
	des, err := os.ReadDir(c.Layout.KeptDir())
	if err != nil {
		return nil
	}
	unlock, err := c.lockKeptReserve()
	if err != nil {
		return err
	}
	defer unlock()
	for _, de := range des {
		key, ok := strings.CutSuffix(de.Name(), keptReserveSuffix)
		dir := filepath.Join(c.Layout.KeptDir(), key)
		r, has := readKeptReserve(dir)
		fi, err := de.Info()
		if !ok || !isKeptKey(key) || !has || err != nil || time.Since(fi.ModTime()) <= macHelperStartWait {
			continue
		}
		if _, err := os.Lstat(r.RunDir); errors.Is(err, fs.ErrNotExist) {
			_ = os.Remove(dir + keptReserveSuffix)
		}
	}
	return nil
}
