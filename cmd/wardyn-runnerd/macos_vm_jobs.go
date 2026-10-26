// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

// jobSockPoll is how often handNet looks for a job's stage-2 ctl.sock and, once it handed the network,
// for the job's helper to have gone; jobDialTries bounds its dials
// there (the socket's file appears at bind, a moment before it accepts).
var (
	jobSockPoll  = time.Second
	jobDialTries = 20
)

// watchJobs starts handNet for each of this runner's claimed prepare and tools-update entries whose
// helper lives and that no handNet has yet, whichever process started the job (serve, or `macos-vm
// prepare`, which exits once the helper claimed its entry). It runs with every recompute, under serve
// only: without its ctx or prepNet (no Docker driver) it starts nothing.
func (m *macOSVMs) watchJobs() {
	m.mu.Lock()
	ctx := m.ctx
	m.mu.Unlock()
	if ctx == nil || m.prepNet == nil || m.prepRemove == nil {
		return
	}
	for _, k := range []admission.Kind{admission.KindPrepare, admission.KindToolsUpdate} {
		for _, e := range m.own(k) {
			if e.HelperPID == 0 || m.vmGone(e) {
				continue
			}
			m.mu.Lock()
			busy := m.nets[e.ID]
			if !busy {
				if m.nets == nil {
					m.nets = map[string]bool{}
				}
				m.nets[e.ID] = true
			}
			m.mu.Unlock()
			if !busy {
				go m.handNet(ctx, e)
			}
		}
	}
}

// handNet is a prepare's or a tools update's stage-2 network (design 3.6): once the helper's stage-2
// boot listens on the job's ctl.sock (macvm.JobControlSock of its ledger entry), the prep network comes
// up and its relay's attach descriptor goes to the helper as net_fd (SCM_RIGHTS); the helper waits 2
// minutes for it. A boot that already has its network got it from a runnerd that has since stopped or
// crashed: that network is adopted as it is, and the helper is never sent a second. The network goes,
// by the job's names, when the helper has gone; never when this runnerd stops, as the helper outlives it
// (its own session) and the next runnerd adopts the network.
func (m *macOSVMs) handNet(ctx context.Context, e admission.Entry) {
	defer func() { m.mu.Lock(); delete(m.nets, e.ID); m.mu.Unlock() }()
	wait := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(jobSockPoll):
			return true
		}
	}
	sock := macvm.JobControlSock(m.sockRoot, e.ID)
	for {
		if _, err := os.Lstat(sock); err == nil {
			break
		}
		if !wait() || m.vmGone(e) {
			m.markDockerWait(e.ID, false)
			return
		}
	}
	attached, err := m.jobNetAttached(ctx, e.ID)
	if err != nil {
		slog.Warn("macOS VM base: ask the stage-2 boot for its network", "error", err.Error())
		return // the next recompute asks again; nothing is built or removed meanwhile
	}
	if !attached {
		attach, undo, err := m.prepNet(ctx, e.ID)
		if err != nil {
			slog.Warn("macOS VM base: the stage-2 network did not start", "error", err.Error())
			// Docker being down is the one cause the drawer names; the next recompute tries again.
			m.markDockerWait(e.ID, m.dockerUp != nil && !m.dockerUp(ctx))
			return
		}
		err = m.sendJobNet(ctx, e.ID, attach)
		_ = attach.Close()
		if err != nil {
			undo()
			slog.Warn("macOS VM base: hand the stage-2 boot its network", "error", err.Error())
			return
		}
	}
	m.markDockerWait(e.ID, false)
	for wait() {
		if m.vmGone(e) {
			if err := m.prepRemove(context.WithoutCancel(ctx), e.ID); err != nil {
				slog.Warn("macOS VM base: remove the stage-2 network", "error", err.Error())
			}
			return
		}
	}
}

// markDockerWait sets or clears the job's mark (Docker was down when its stage-2 network was asked for),
// and asks for a recompute when the mark changed, so the caps say so at once.
func (m *macOSVMs) markDockerWait(id string, on bool) {
	m.mu.Lock()
	changed := m.dockerWait[id] != on
	if on {
		if m.dockerWait == nil {
			m.dockerWait = map[string]bool{}
		}
		m.dockerWait[id] = true
	} else {
		delete(m.dockerWait, id)
	}
	m.mu.Unlock()
	if changed {
		m.changed()
	}
}

// pruneDockerWait drops the marks of jobs whose helper has gone (their handler returns without clearing it
// when Docker is down), and reports whether any job still waits for Docker.
func (m *macOSVMs) pruneDockerWait() bool {
	m.mu.Lock()
	marked := slices.Collect(maps.Keys(m.dockerWait))
	m.mu.Unlock()
	if len(marked) == 0 {
		return false
	}
	live := map[string]bool{}
	for _, k := range []admission.Kind{admission.KindPrepare, admission.KindToolsUpdate} {
		for _, e := range m.own(k) {
			if e.HelperPID != 0 && !m.vmGone(e) {
				live[e.ID] = true
			}
		}
	}
	any := false
	for _, id := range marked {
		if live[id] {
			any = true
		} else {
			m.markDockerWait(id, false)
		}
	}
	return any
}

// dialJob dials the job's ctl.sock in its checked socket directory (this login's, 0700, not a link).
func (m *macOSVMs) dialJob(ctx context.Context, entryID string) (*macvm.ControlClient, error) {
	sc := macvm.SockConfig(m.sockRoot)
	sc.Fstat = m.sockFstat
	dir, err := sc.CheckRunDir(macvm.JobSockKey(entryID))
	if err != nil {
		return nil, err
	}
	var cc *macvm.ControlClient
	for i := range jobDialTries {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cc, err = macvm.DialControl(dctx, filepath.Join(dir, macvm.ControlSock))
		cancel()
		if err == nil || i == jobDialTries-1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cc, err
}

// jobNetAttached reports whether the job's stage-2 boot already has its network.
func (m *macOSVMs) jobNetAttached(ctx context.Context, entryID string) (bool, error) {
	cc, err := m.dialJob(ctx, entryID)
	if err != nil {
		return false, err
	}
	defer cc.Close()
	st, err := cc.Status(ctx)
	return st.Network == macvm.NetAttached, err
}

// sendJobNet sends attach to the job's stage-2 boot as net_fd.
func (m *macOSVMs) sendJobNet(ctx context.Context, entryID string, attach *os.File) error {
	cc, err := m.dialJob(ctx, entryID)
	if err != nil {
		return err
	}
	defer cc.Close()
	return cc.SendNetFD(ctx, attach)
}

// sweepPrep removes the stage-2 networks a crashed runnerd left (design 3.4, crash recovery), keeping
// those of every job still in the ledger: its helper may live, or wait for its network.
func (m *macOSVMs) sweepPrep(ctx context.Context) {
	if m.prepSweep == nil || m.ledger == nil {
		return
	}
	counted, err := m.ledger.Counted()
	if err != nil {
		slog.Warn("macOS VM base: leftover stage-2 networks were not swept", "error", err.Error())
		return
	}
	keep := make([]string, 0, len(counted))
	for _, e := range counted {
		keep = append(keep, e.ID)
	}
	n, err := m.prepSweep(ctx, keep)
	if err != nil {
		slog.Warn("macOS VM base: sweep leftover stage-2 networks", "error", err.Error())
	}
	if n > 0 {
		slog.Info("macOS VM base: removed leftover stage-2 network objects", "count", n)
	}
}

// updateLockSuffix is the tools update lock beside each base dir (internal/macvm/tools.go
// updateLockPath): held for an update's or a re-prepare's whole run.
const updateLockSuffix = ".update.lock"

// sweepStaging is runnerd's start-up sweep (design 3.4, crash recovery), before it offers macOS VMs: a
// <slot>.update staging dir a crashed tools update or re-prepare left is deleted, for each slot whose
// update lock this runner gets without waiting. A held lock is a live update, which owns the dir.
func (m *macOSVMs) sweepStaging() {
	des, err := os.ReadDir(filepath.Dir(m.layout.BaseDir(0)))
	if err != nil {
		return
	}
	for _, de := range des {
		name, ok := strings.CutSuffix(de.Name(), ".update")
		slot, err := strconv.Atoi(name)
		if !ok || err != nil || strconv.Itoa(slot) != name {
			continue
		}
		if err := sweepSlotStaging(m.layout.BaseDir(slot)); err != nil {
			slog.Warn("macOS VM base: delete a crashed update's staging copy", "slot", slot, "error", err.Error())
		}
	}
}

// sweepSlotStaging deletes base's staging dir under its update lock, taken with LOCK_NB; a lock another
// process holds leaves it.
func sweepSlotStaging(base string) error {
	f, err := os.OpenFile(base+updateLockSuffix, os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil // a live update owns it
		}
		return err
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	return os.RemoveAll(base + ".update")
}

// checkIPSW is the absolute path of the restore image --ipsw names, or why it cannot be used: the helper
// runs from another directory and reads it as this login.
func checkIPSW(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("--ipsw %q: %w", p, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("--ipsw: cannot read the restore image: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("--ipsw %q is not a regular file", abs)
	}
	return abs, nil
}
