// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// A kept machine is one person's macOS guest disk, kept on their own Mac between runs, in the Layout's
// kept folder and never in a run's. Each session of it is a new run (SandboxSpec.KeptMachine): the helper
// clones, resets or boots the machine's folder under the machine's lock, and the run folder holds only
// run.json and the log. Wardyn keeps what the person made in it and never what Wardyn delivered: a secret
// that would live inside the guest is refused here, and what else a run writes is scrubbed by the guest
// agent at a graceful stop and again at every boot. Every stop keeps the machine; only a delete (the
// organisation's kept_machine, an expiry, another registration, an erasure) removes it.

// KeptConfig is MacVMConfig's part for kept machines. wardyn-runnerd fills it.
type KeptConfig struct {
	// LimitBytes is the disk this Mac's kept machines may take, each counted at its worst case; zero
	// keeps no machines on this Mac.
	LimitBytes int64
	// Tools brings a kept machine's tools up to this Mac's before it resumes; nil leaves them as they are.
	Tools KeptToolsUpdater
	// keptNow is the clock expiries are read and written with; nil is the wall clock (tests set it).
	keptNow func() time.Time
	// keptListed, if set, runs between keptMacBytes's listing of the kept folder and its reads (tests set it).
	keptListed func()
	// eraseWait bounds an erasure's wait for each machine's lock; 0 is keptEraseWait (tests set it).
	eraseWait time.Duration
	// lockWait bounds a record write's or a person's action's wait for a machine's lock; 0 is keptLockWait
	// (tests set it).
	lockWait time.Duration
}

// lockWaitOr is lockWait, or keptLockWait.
func (k *KeptConfig) lockWaitOr() time.Duration { return cmp.Or(k.lockWait, keptLockWait) }

// The bounds on the runner's takes of a machine's lock, which a boot, a tools update, a stop's record or a
// sweep may hold for a moment: a record write or a person's action waits keptLockWait before it says the
// machine is busy, a sweep keptSweepLockWait (the next sweep retries), and an erasure keptEraseWait, long
// enough for a tools update to finish.
const (
	keptLockWait      = 15 * time.Second
	keptSweepLockWait = time.Second
	keptEraseWait     = 2 * time.Minute
)

// keptClock is now, by keptNow.
func (k *KeptConfig) keptClock() time.Time {
	if k.keptNow != nil {
		return k.keptNow().UTC()
	}
	return time.Now().UTC()
}

// KeptToolsUpdater updates the guest agent and pinned harness CLIs on a kept machine's disk, in its
// folder, when the machine's tools are behind this Mac's. It takes the machine's lock itself (ErrKeptLocked
// when a boot holds it), so the runner calls it unlocked, before the boot.
type KeptToolsUpdater interface {
	UpdateKeptTools(ctx context.Context, keptDir string) error
}

// keptScrubNotRun is a stop scrub's outcome on a hard stop, which runs none (runner.KeptScrub.Outcome).
const keptScrubNotRun = "not_run"

// keptIdleDays is the idle days a kept machine's expiry is counted with: the spec's, or the site default
// (macvm.KeptDefaultIdleDays), so a kept machine always has an expiry this Mac enforces on its own.
func keptIdleDays(days int) int {
	if days <= 0 {
		return macvm.KeptDefaultIdleDays
	}
	return days
}

// keptStopScrubWait bounds the guest's stop scrub; a scrub that does not finish leaves the machine
// unscrubbed, never the stop failed.
const keptStopScrubWait = 60 * time.Second

// keptMCPSecretEnv is the one secret env a kept machine's session may carry: the MCP broker routes. Each
// URL holds its run's token, which is dead once that run's proxy is gone, and the scrub strips them.
const keptMCPSecretEnv = "WARDYN_MCP_SERVERS_JSON"

// The sentences a kept machine's start is refused with (the person reads them as the runner says them).
const (
	keptSaidLocked     = "This machine is already running on this Mac."
	keptSaidTools      = "This machine's tools couldn't be updated, so it didn't start. Start it fresh, or try again."
	keptSaidIncomplete = "This machine's files on this Mac are incomplete, so it didn't start. Start it fresh, or delete it."
	keptSaidOff        = "This Mac doesn't keep machines."
	keptSaidOnlyLive   = "Only an interactive macOS VM run can keep or resume a machine."
	keptSaidBase       = "This Mac's base image changed while your run was starting. Start the run again."
)

// keptRefusal is a refusal the person reads as it is.
func keptRefusal(sentence string) error { return errors.New(sentence) }

func keptSaidNotFound(id uuid.UUID) error {
	return keptRefusal("You have no kept machine " + id.String() + ".")
}

// keptSaidExpired is an expired machine's refusal: deleted in the past tense only when it is gone.
func keptSaidExpired(days int, gone bool) error {
	if !gone {
		return keptRefusal(fmt.Sprintf("Your machine wasn't used for %d days, so it expired and will be deleted.", days))
	}
	return keptRefusal(fmt.Sprintf("Your machine wasn't used for %d days, so it was deleted. Start a new one.", days))
}

// keptLimitRefusal is the Mac's kept limit refusing a start; the person reads it as it is, also when the
// helper's wait carries it.
type keptLimitRefusal struct{ error }

func keptSaidMacLimit(limit, need int64) error {
	//lint:ignore ST1005 the sentence the person reads, as it is
	return keptLimitRefusal{fmt.Errorf("This Mac keeps machines up to %d GB, and another would need up to %d GB more. "+
		"Delete one with `wardyn machines delete`, or raise the limit on that Mac.", limit>>30, (need+(1<<30)-1)>>30)}
}

// keptDir is the folder of the kept machine id.
func (c *MacVMConfig) keptDir(id uuid.UUID) string { return c.Layout.KeptMachineDir(id.String()) }

// refuseKeptSpec refuses, before anything exists, a kept machine's session this Mac must not start: kept
// machines off here, a malformed spec, a run that is not interactive, or one that would put a secret
// inside the guest.
func refuseKeptSpec(c *MacVMConfig, spec runner.SandboxSpec) error {
	k := spec.KeptMachine
	switch {
	case k == nil:
		return nil
	case c == nil || c.LimitBytes <= 0:
		return keptRefusal(keptSaidOff)
	case k.ID == uuid.Nil || k.OwnerHash == "" || k.Mode != macvm.KeptNew && k.Mode != macvm.KeptResume && k.Mode != macvm.KeptFresh:
		return fmt.Errorf("docker: a kept machine needs its id, owner and a mode of new, resume or fresh (mode %q)", k.Mode)
	case !spec.Interactive:
		return keptRefusal(keptSaidOnlyLive)
	}
	return refuseKeptSecrets(spec)
}

// refuseKeptSecrets refuses a kept machine's session that would put a Wardyn secret inside the guest,
// where the kept disk would keep it: any secret env but the MCP routes, and any file delivered in
// runner.ComponentSecretDir. The organisation refuses these before dispatch; this is the Mac's own
// check. It names keys and paths only, never a value.
func refuseKeptSecrets(spec runner.SandboxSpec) error {
	if spec.KeptMachine == nil {
		return nil
	}
	var held []string
	for k := range spec.SecretEnv {
		if k != keptMCPSecretEnv {
			held = append(held, "secret env "+k)
		}
	}
	slices.Sort(held)
	for _, f := range spec.ManagedFiles {
		if p := path.Clean(f.Path); p == runner.ComponentSecretDir || strings.HasPrefix(p, runner.ComponentSecretDir+"/") {
			held = append(held, "secret file "+p)
		}
	}
	if len(held) == 0 {
		return nil
	}
	return fmt.Errorf("docker: a kept machine takes no secret that lives inside it, and this run carries %s", strings.Join(held, ", "))
}

// refuseMacVMStart is refuseMacVMSpec, then a kept machine's own start (keptStart): its checks, a
// resume's tools update, the machine's own manifest in place of the base's, and the disk the start reserves
// against the Mac's kept limit. release drops that reservation; the create calls it when it ends, by which
// time the machine's record counts it (or the create failed).
func (d *Driver) refuseMacVMStart(ctx context.Context, spec runner.SandboxSpec) (man macvm.Manifest, kept *macvm.KeptRun, release func(), err error) {
	release = func() {}
	if man, err = d.refuseMacVMSpec(spec); err != nil {
		return man, nil, release, err
	}
	if kept, err = d.keptStart(ctx, spec, &man); err != nil || kept == nil {
		return man, kept, release, err
	}
	if kept.Mode != macvm.KeptResume {
		release, err = d.macVMConfig().reserveKept(spec, &man, kept)
	}
	return man, kept, release, err
}

// keptStart is a kept machine session's part of a create, before the helper starts: the Mac's own checks
// of the machine (keptCheckResume), and for a resume the machine's tools update and its own manifest in man
// (what the run's size and harnesses are checked against, and what its ledger entry reserves). It answers
// what run.json carries, nil for a run with no kept machine. The checks are advisory: the helper takes the
// machine's lock at boot and refuses again (keptStartErr words its refusal).
func (d *Driver) keptStart(ctx context.Context, spec runner.SandboxSpec, man *macvm.Manifest) (*macvm.KeptRun, error) {
	k := spec.KeptMachine
	if k == nil {
		return nil, nil
	}
	c := d.macVMConfig()
	dir := c.keptDir(k.ID)
	if k.Mode == macvm.KeptNew {
		if _, err := os.Lstat(dir); err == nil {
			return nil, fmt.Errorf("docker: kept machine %s is already on this Mac", k.ID)
		}
		return c.keptRun(spec), nil
	}
	if err := c.keptCheckResume(spec, dir); err != nil {
		return nil, err
	}
	if k.Mode == macvm.KeptResume {
		if err := c.updateKeptTools(ctx, spec, dir); err != nil {
			return nil, err
		}
		km, err := macvm.ReadyBase(dir)
		if err != nil {
			return nil, keptRefusal(keptSaidIncomplete)
		}
		if err := macVMFits(spec, km); err != nil {
			return nil, err
		}
		*man = km
	}
	return c.keptRun(spec), nil
}

// keptOwned reads dir's record and refuses, as no such machine, one that is not there or not the spec's
// owner's on this runner.
func (c *MacVMConfig) keptOwned(dir string, k *runner.KeptMachineSpec) (macvm.KeptRecord, error) {
	rec, err := macvm.ReadKept(dir, k.ID.String())
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, macvm.ErrKeptIDMismatch):
		return rec, keptSaidNotFound(k.ID)
	case err != nil:
		return rec, fmt.Errorf("docker: kept machine %s: %w", k.ID, err)
	case rec.OwnerHash != k.OwnerHash || rec.RunnerID != c.RunnerID:
		return rec, keptSaidNotFound(k.ID)
	}
	return rec, nil
}

// keptCheckResume is the Mac's own check of a resume or reset, made holding the machine's lock, which it
// takes first: a machine a boot holds, or whose last session has not recorded its stop yet, is running.
// An interrupted session (its run folder gone, its stop never recorded) is ended first and never deleted
// for; only then is the machine's expiry read, and an expired machine deleted.
func (c *MacVMConfig) keptCheckResume(spec runner.SandboxSpec, dir string) error {
	k := spec.KeptMachine
	if _, err := c.keptOwned(dir, k); err != nil { // read unlocked first: no lock file for a machine not here
		return err
	}
	unlock, err := macvm.LockKeptWait(dir, c.lockWaitOr())
	if errors.Is(err, macvm.ErrKeptLocked) {
		return keptRefusal(keptSaidLocked)
	} else if err != nil {
		return fmt.Errorf("docker: kept machine %s: %w", k.ID, err)
	}
	defer unlock()
	rec, err := c.keptOwned(dir, k)
	if err != nil {
		return err
	}
	now := c.keptClock()
	switch {
	case c.keptHeldBy(rec) != "":
		return keptRefusal(keptSaidLocked)
	case rec.ActiveRun != "":
		if err := c.recoverKept(dir, &rec, now); err != nil {
			return fmt.Errorf("docker: kept machine %s: %w", k.ID, err)
		}
	case keptExpired(rec, now):
		_, err := removeKeptHeld(dir)
		c.logExpiredRemoval(k.ID.String(), err)
		return keptSaidExpired(macvm.KeptRecordIdleDays(rec), err == nil)
	}
	return nil
}

// keptExpired reports whether rec is past its expiry with no session holding it: a machine a run holds
// never expires under it.
func keptExpired(rec macvm.KeptRecord, now time.Time) bool {
	return rec.ActiveRun == "" && !rec.ExpiresAt.IsZero() && now.After(rec.ExpiresAt)
}

// keptHeldBy is the run a record's ActiveRun names while that run's folder is still on the Mac: a session
// that has not recorded its stop yet. "" when no run holds it that way.
func (c *MacVMConfig) keptHeldBy(rec macvm.KeptRecord) string {
	id, err := uuid.Parse(rec.ActiveRun)
	if err != nil {
		return ""
	}
	if _, err := os.Lstat(c.runDir(id)); err != nil {
		return ""
	}
	return rec.ActiveRun
}

// recoverKept ends an interrupted session's hold on a record whose run folder is gone with no stop
// recorded: the machine is unheld, with a fresh idle allowance from now (the one the record was last given,
// macvm.KeptRecordIdleDays), and never deleted for it. The caller holds dir's lock.
func (c *MacVMConfig) recoverKept(dir string, rec *macvm.KeptRecord, now time.Time) error {
	slog.Info("docker: a kept machine's session ended without recording its stop; the machine is kept",
		slog.String("machine_id", rec.ID), slog.String("run_id", rec.ActiveRun))
	macvm.KeptStopUpdate(rec, rec.ActiveRun, now, macvm.KeptRecordIdleDays(*rec))
	rec.LastStop, rec.StopKind, rec.Scrubbed = now, macvm.KeptStopHard, false
	return macvm.WriteKept(dir, *rec)
}

// logExpiredRemoval logs an expired machine's deletion, or why it did not happen.
func (c *MacVMConfig) logExpiredRemoval(id string, err error) {
	if err != nil {
		slog.Warn("docker: delete an expired kept machine", slog.String("machine_id", id), slog.String("err", err.Error()))
		return
	}
	slog.Info("docker: deleted an expired kept machine", slog.String("machine_id", id))
	c.changed()
}

// keptRun is the run.json block of spec's kept machine.
func (c *MacVMConfig) keptRun(spec runner.SandboxSpec) *macvm.KeptRun {
	k := spec.KeptMachine
	return &macvm.KeptRun{ID: k.ID.String(), Mode: k.Mode, OwnerHash: k.OwnerHash, RunnerID: c.RunnerID,
		Base: cmp.Or(spec.MacOSBase, macvm.StandardBase), IdleDays: keptIdleDays(k.IdleDays), Name: k.Name}
}

// updateKeptTools runs the tools update on a resumed machine's folder before its boot. The runner holds no
// lock around it: the update takes the machine's lock itself, so no boot runs beside it, and the boot that
// follows takes it again. An update that finds the lock held is tried again for up to keptLockWait, since
// the holder may be only a stop's record. A machine whose update fails does not start; one still held is
// locked.
func (c *MacVMConfig) updateKeptTools(ctx context.Context, spec runner.SandboxSpec, dir string) error {
	if c.Tools == nil {
		return nil
	}
	spec.NotifyWaiting(runner.MacOSStartDetail(runner.MacOSCloning))
	err := c.Tools.UpdateKeptTools(ctx, dir)
	for deadline := time.Now().Add(c.lockWaitOr()); errors.Is(err, macvm.ErrKeptLocked) && time.Now().Before(deadline) && ctx.Err() == nil; {
		time.Sleep(100 * time.Millisecond)
		err = c.Tools.UpdateKeptTools(ctx, dir)
	}
	if errors.Is(err, macvm.ErrKeptLocked) {
		return keptRefusal(keptSaidLocked)
	} else if err != nil {
		slog.Warn("docker: update a kept machine's tools", slog.String("machine_id", spec.KeptMachine.ID.String()), slog.String("err", err.Error()))
		return keptRefusal(keptSaidTools)
	}
	return nil
}

// keptStartErr words the helper's refusal of a kept machine's boot; any other error is answered as it is.
// The helper records its refusal's code in the run dir (macvm.ReadKeptRefusal); an older helper says it
// only in its error's text, which is matched when there is no code. An expired machine is deleted.
func (c *MacVMConfig) keptStartErr(spec runner.SandboxSpec, err error) error {
	k := spec.KeptMachine
	var limit keptLimitRefusal
	if k == nil || err == nil {
		return err
	} else if errors.As(err, &limit) {
		return limit
	}
	code, _ := macvm.ReadKeptRefusal(c.runDir(spec.RunID))
	if code == "" {
		code = keptRefusalInText(err)
	}
	switch code {
	case macvm.KeptRefusalLocked:
		return keptRefusal(keptSaidLocked)
	case macvm.KeptRefusalOwner, macvm.KeptRefusalIDMismatch:
		return keptSaidNotFound(k.ID)
	case macvm.KeptRefusalIncomplete:
		return keptRefusal(keptSaidIncomplete)
	case macvm.KeptRefusalBaseChanged:
		return keptRefusal(keptSaidBase)
	case macvm.KeptRefusalExpired:
		return keptSaidExpired(keptIdleDays(k.IdleDays), c.removeExpiredKept(c.keptDir(k.ID), k.ID))
	}
	return err
}

// keptRefusalInText is the refusal code an older helper's error names in its text, or "".
func keptRefusalInText(err error) string {
	said := err.Error()
	for _, e := range []error{macvm.ErrKeptLocked, macvm.ErrKeptOwner, macvm.ErrKeptIncomplete, macvm.ErrKeptExpired, macvm.ErrKeptIDMismatch, macvm.ErrKeptBaseChanged} {
		if strings.Contains(said, e.Error()) {
			return macvm.KeptRefusalCode(e)
		}
	}
	return ""
}

// undoKeptNew deletes a new kept machine whose create failed: the clone this create's helper made, and
// only a folder whose record names this owner on this runner. A folder still locked is left to the sweep.
func (c *MacVMConfig) undoKeptNew(spec runner.SandboxSpec) {
	k := spec.KeptMachine
	if k == nil || k.Mode != macvm.KeptNew {
		return
	}
	dir := c.keptDir(k.ID)
	if rec, err := macvm.ReadKept(dir, k.ID.String()); err != nil || rec.OwnerHash != k.OwnerHash || rec.RunnerID != c.RunnerID {
		return
	}
	if _, err := removeKept(dir, c.lockWaitOr()); err != nil {
		slog.Warn("docker: delete a kept machine whose create failed", slog.String("machine_id", k.ID.String()), slog.String("err", err.Error()))
	}
}

// removeExpiredKept deletes a machine past its expiry, holding its lock and reading the record again under
// it: a machine a boot or a session holds, or no longer expired, stays. It reports whether the machine is
// gone (deleted, or not on the Mac at all).
func (c *MacVMConfig) removeExpiredKept(dir string, id uuid.UUID) bool {
	unlock, err := macvm.LockKeptWait(dir, c.lockWaitOr())
	if err != nil {
		c.logExpiredRemoval(id.String(), err)
		return false
	}
	defer unlock()
	rec, err := macvm.ReadKept(dir, id.String())
	if errors.Is(err, fs.ErrNotExist) {
		_, serr := os.Lstat(dir)
		return errors.Is(serr, fs.ErrNotExist)
	} else if err != nil || c.keptHeldBy(rec) != "" || !keptExpired(rec, c.keptClock()) {
		return false
	}
	_, err = removeKeptHeld(dir)
	c.logExpiredRemoval(id.String(), err)
	return err == nil
}

// keptMachineOf is the kept machine a run's VM boots, from its run.json; nil for any other run.
func (c *MacVMConfig) keptMachineOf(v *vmRun) *macvm.KeptRun {
	cfg, err := readMacRun(v.dir, v.runID)
	if err != nil {
		return nil
	}
	return cfg.Kept
}

// keptStopScrub asks the guest to scrub what its run wrote, before a graceful stop of a kept machine's
// VM. A scrub that fails or does not answer is reported failed; the stop goes on either way.
func keptStopScrub(ctx context.Context, v *vmRun) *runner.KeptScrub {
	ctx, cancel := context.WithTimeout(ctx, keptStopScrubWait)
	defer cancel()
	rep, err := v.agent.Scrub(ctx)
	if err != nil {
		slog.Warn("docker: the kept machine's stop scrub", slog.String("run_id", v.runID.String()), slog.String("err", err.Error()))
		return &runner.KeptScrub{When: guestagent.ScrubAtStop, Outcome: guestagent.ScrubFailed}
	}
	s := runner.KeptScrub(rep)
	return &s
}

// keptStopped records a kept machine's stop once its VM is down and before its run folder goes, under the
// machine's lock so a racing resume or sweep cannot interleave. It binds to the session: the record changes
// only while its ActiveRun is this run (macvm.KeptStopUpdate, which also restarts the idle allowance, so a
// machine used past its old expiry is not deleted as it stops); then the stop, and whether its scrub
// finished. It answers the scrub to report, not_run for a hard stop, and the expiry it wrote to the record
// (zero when the record did not change), which the stop report carries so the organisation records the
// same deadline and never reads a later setting.
func (c *MacVMConfig) keptStopped(k *macvm.KeptRun, runID uuid.UUID, scrub *runner.KeptScrub) (*runner.KeptScrub, time.Time) {
	if scrub == nil {
		scrub = &runner.KeptScrub{When: guestagent.ScrubAtStop, Outcome: keptScrubNotRun}
	}
	dir := c.Layout.KeptMachineDir(k.ID)
	log := slog.With(slog.String("machine_id", k.ID), slog.String("run_id", runID.String()))
	unlock, err := macvm.LockKeptWait(dir, c.lockWaitOr())
	if err != nil {
		// The record keeps this run as its session; once the run folder is gone, the sweep ends the session
		// as a hard stop (recoverKept), and the next boot scrubs again.
		log.Warn("docker: record a kept machine's stop (the sweep records it)", slog.String("err", err.Error()))
		return scrub, time.Time{}
	}
	defer unlock()
	rec, err := macvm.ReadKept(dir, k.ID)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("docker: record a kept machine's stop", slog.String("err", err.Error()))
		}
		return scrub, time.Time{}
	}
	now := c.keptClock()
	if !macvm.KeptStopUpdate(&rec, runID.String(), now, keptIdleDays(k.IdleDays)) {
		return scrub, time.Time{} // the helper refused this boot, or another session holds the machine
	}
	rec.LastStop, rec.StopKind = now, macvm.KeptStopHard
	if scrub.Outcome != keptScrubNotRun {
		rec.StopKind = macvm.KeptStopGraceful
	}
	rec.Scrubbed = scrub.Outcome == guestagent.ScrubDone
	if err := macvm.WriteKept(dir, rec); err != nil {
		log.Warn("docker: record a kept machine's stop", slog.String("err", err.Error()))
		return scrub, time.Time{}
	}
	return scrub, rec.ExpiresAt
}

// keptMachineDisk is a kept machine session's disk report once its run folder is deleted: kept, with the
// stop scrub's outcome and the expiry the stop wrote to the record; a zero expiry (the record was not
// written) is reported as pending, not as no word.
func (d *Driver) keptMachineDisk(runID uuid.UUID, scrub *runner.KeptScrub, expiresAt time.Time) {
	d.macDisks.Store(runID, runner.MacOSVMDisk{RunID: runID, State: runner.MacOSVMDiskKept, Reason: runner.MacOSVMDiskKeptMachine,
		At: time.Now().UTC(), Scrub: scrub, KeptExpiresAt: expiresAt, KeptExpiryPending: expiresAt.IsZero()})
}

// DeleteKeptMachine deletes one kept machine from this Mac, holding its lock throughout (the
// organisation's kept_machine delete). A machine a boot holds is refused; one this Mac does not hold is
// already absent. by is who asked, for the log.
func (d *Driver) DeleteKeptMachine(_ context.Context, id uuid.UUID, by string) (string, error) {
	c := d.macVMConfig()
	if c == nil {
		return "", errors.New("docker: this runner keeps no machines")
	}
	dir := c.keptDir(id)
	_, err := macvm.ReadKept(dir, id.String())
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, macvm.ErrKeptIDMismatch):
		return runnerwire.KeptMachineAlreadyAbsent, nil
	case err != nil:
		return "", fmt.Errorf("docker: kept machine %s: %w", id, err)
	}
	gone, err := removeKept(dir, c.lockWaitOr())
	switch {
	case errors.Is(err, macvm.ErrKeptLocked):
		return "", runnerwire.Refuse("docker: kept machine %s is running on this Mac", id) // nothing was removed
	case err != nil:
		return "", fmt.Errorf("docker: delete kept machine %s: %w", id, err)
	case !gone:
		return runnerwire.KeptMachineAlreadyAbsent, nil
	}
	slog.Info("docker: deleted a kept machine", slog.String("machine_id", id.String()), slog.String("by", by))
	c.changed()
	return runnerwire.KeptMachineDeleted, nil
}

// keptFolder is one kept machine folder on this Mac, with its record.
type keptFolder struct {
	dir string
	rec macvm.KeptRecord
}

// keptFolders lists this Mac's kept machines by their records. A folder whose record is missing,
// unreadable or names another folder's id is skipped: nothing of it can be named.
func (c *MacVMConfig) keptFolders() ([]keptFolder, error) {
	des, err := os.ReadDir(c.Layout.KeptDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("docker: read the kept machines: %w", err)
	}
	var out []keptFolder
	for _, de := range des {
		if !de.IsDir() || !isKeptKey(de.Name()) {
			continue
		}
		if f, ok := c.keptFolderAt(filepath.Join(c.Layout.KeptDir(), de.Name())); ok {
			out = append(out, f)
		}
	}
	return out, nil
}

// keptFolderAt is the kept machine in dir by its record; false when it has none that names dir.
func (c *MacVMConfig) keptFolderAt(dir string) (keptFolder, bool) {
	var probe macvm.KeptRecord
	if b, err := os.ReadFile(filepath.Join(dir, macvm.KeptFile)); err != nil || json.Unmarshal(b, &probe) != nil {
		return keptFolder{}, false
	}
	rec, err := macvm.ReadKept(dir, probe.ID)
	if err != nil || c.Layout.KeptMachineDir(rec.ID) != dir {
		return keptFolder{}, false
	}
	return keptFolder{dir: dir, rec: rec}, true
}

// isKeptKey reports whether name is a kept machine's folder: its key, 12 hex digits (macvm.RunKey). The
// kept folder also holds each machine's lock, a tools update's staging (<key>.update and its lock), a
// start's reservation (<key>.reserve) and the reservations' lock (.reserve.lock), which are never machines.
func isKeptKey(name string) bool {
	if len(name) != macvm.RunKeyLen {
		return false
	}
	for _, r := range name {
		if !('0' <= r && r <= '9' || 'a' <= r && r <= 'f') {
			return false
		}
	}
	return true
}

// keptUpdateSuffix names a tools update's staging beside a kept machine's folder (<key>.update), and with
// ".lock" its lock.
const keptUpdateSuffix = ".update"

// removeKept deletes a kept machine, holding its lock throughout as macvm.RemoveKept does (removeKeptHeld),
// waiting up to wait for it. ErrKeptLocked while a boot or an update still holds it; false with no error
// when there was no folder.
func removeKept(dir string, wait time.Duration) (bool, error) {
	unlock, err := macvm.LockKeptWait(dir, wait)
	if err != nil {
		return false, err
	}
	defer unlock()
	return removeKeptHeld(dir)
}

// removeKeptHeld deletes a kept machine whose lock the caller holds, and with it a tools update's staging
// the update left (no update runs while the lock is held): the staging and its lock, the folder, then the
// folder's lock file.
func removeKeptHeld(dir string) (bool, error) {
	if err := removeKeptUpdate(dir); err != nil {
		return false, err
	}
	_, serr := os.Lstat(dir)
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	if err := os.Remove(dir + ".lock"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return true, err
	}
	return serr == nil, nil
}

// removeKeptUpdate deletes dir's tools update staging and its lock, holding that lock too; the caller holds
// dir's lock, so the staging is one a finished or crashed update left. ErrKeptLocked while an update has it.
func removeKeptUpdate(dir string) error {
	staging := dir + keptUpdateSuffix
	unlock, err := macvm.LockKept(staging)
	if err != nil {
		return err
	}
	defer unlock()
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.Remove(staging + ".lock"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// keptWorst is a kept machine's worst case on the disk: its disk's logical size plus what a run reserves
// beside it.
func keptWorst(man macvm.Manifest) int64 {
	return admission.RunDiskReservation(cmp.Or(man.DiskBytes, macvm.BaseDiskBytes))
}

// keptWorstOf is keptWorst of the machine in dir, by its own manifest copy (the base's size when absent).
func keptWorstOf(dir string) int64 {
	man, _ := macvm.ReadManifest(dir)
	return keptWorst(man)
}

// keptRunsByMachine maps each kept machine a live VM boots to that VM's run id, from the run folders.
func (c *MacVMConfig) keptRunsByMachine() map[string]string {
	des, _ := os.ReadDir(c.Layout.RunsDir())
	out := map[string]string{}
	for _, de := range des {
		cfg, err := macvm.ReadRunConfig(filepath.Join(c.Layout.RunsDir(), de.Name()))
		if err != nil || cfg.Kept == nil {
			continue
		}
		if e, ok := c.entry(cfg.LedgerEntry); ok && !c.vmGone(e) {
			out[cfg.Kept.ID] = cfg.RunID
		}
	}
	return out
}

// KeptMachines is this Mac's kept machines for its caps: the limit, every machine's worst case and what
// they take now, and each machine with the run booted from it. Nil when it keeps none.
func (d *Driver) KeptMachines() (*runnerwire.MacVMKept, error) {
	c := d.macVMConfig()
	if c == nil || c.LimitBytes <= 0 {
		return nil, nil
	}
	folders, err := c.keptFolders()
	if err != nil {
		return nil, err
	}
	runs := c.keptRunsByMachine()
	out := &runnerwire.MacVMKept{LimitBytes: c.LimitBytes}
	for _, f := range folders {
		n, _ := macvm.KeptAllocatedBytes(f.dir)
		// In use is read from the live runs and the record's session, never by probing the machine's lock,
		// which a boot taking it at that instant would find held. A record that still names a session is in
		// use even once its run folder is gone: until recoverKept clears it and writes the new deadline, its
		// expiry is the boot's, and the organisation must not keep or expire the machine by it.
		run := cmp.Or(runs[f.rec.ID], f.rec.ActiveRun)
		m := runnerwire.MacVMKeptMachine{ID: f.rec.ID, Bytes: n, InUse: run != "", RunID: run, Scrubbed: f.rec.Scrubbed, ExpiresAt: f.rec.ExpiresAt}
		out.WorstBytes += keptWorstOf(f.dir)
		out.AllocatedBytes += n
		out.Machines = append(out.Machines, m)
	}
	return out, nil
}

// SweepKeptMachines settles this Mac's kept machines, offline too, each under its lock (a machine a boot or
// an update holds is left to the next sweep, and so is one whose session has not recorded its stop yet):
// a machine another registration of this Mac made is deleted, being bound to the registration that made
// it; an interrupted session is ended and its machine kept; a machine past its expiry with no session is
// deleted; and a tools update's staging a finished update left goes. Reservations whose create is gone go
// too. wardyn-runnerd runs it at start and with its periodic checks.
func (d *Driver) SweepKeptMachines() error {
	c := d.macVMConfig()
	if c == nil {
		return nil
	}
	folders, err := c.keptFolders()
	errs := []error{err, c.sweepKeptReservations(), c.sweepKeptDebris()}
	now := c.keptClock()
	for _, f := range folders {
		if err := c.sweepKept(f.dir, f.rec.ID, now); err != nil {
			errs = append(errs, fmt.Errorf("docker: sweep kept machine %s: %w", f.rec.ID, err))
		}
	}
	c.changed()
	return errors.Join(errs...)
}

// sweepKept is SweepKeptMachines for one machine.
func (c *MacVMConfig) sweepKept(dir, id string, now time.Time) error {
	unlock, err := macvm.LockKeptWait(dir, keptSweepLockWait)
	if errors.Is(err, macvm.ErrKeptLocked) {
		return nil
	} else if err != nil {
		return err
	}
	defer unlock()
	rec, err := macvm.ReadKept(dir, id)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	switch {
	case c.keptHeldBy(rec) != "":
		return nil
	case rec.RunnerID != c.RunnerID:
		if _, err := removeKeptHeld(dir); err != nil {
			return err
		}
		slog.Info("docker: deleted a kept machine another registration of this Mac made", slog.String("machine_id", id))
		return nil
	case rec.ActiveRun != "":
		return c.recoverKept(dir, &rec, now)
	case keptExpired(rec, now):
		_, err := removeKeptHeld(dir)
		c.logExpiredRemoval(id, err)
		return err
	}
	if _, err := os.Lstat(dir + keptUpdateSuffix); err == nil {
		return removeKeptUpdate(dir)
	}
	return nil
}

// sweepKeptDebris removes a machine folder a create left with no record: its helper stopped between the
// clone and the record. Only a folder with no kept.json, no reservation, no holder of its lock and older
// than a create's wait for its helper goes; nothing of it can be named, so nothing else would remove it.
func (c *MacVMConfig) sweepKeptDebris() error {
	des, err := os.ReadDir(c.Layout.KeptDir())
	if err != nil {
		return nil
	}
	var errs []error
	for _, de := range des {
		dir := filepath.Join(c.Layout.KeptDir(), de.Name())
		fi, ierr := de.Info()
		if !de.IsDir() || !isKeptKey(de.Name()) || ierr != nil || time.Since(fi.ModTime()) <= macHelperStartWait ||
			keptExists(filepath.Join(dir, macvm.KeptFile)) || keptExists(dir+keptReserveSuffix) {
			continue
		}
		unlock, err := macvm.LockKept(dir)
		if err != nil {
			continue // held: a create or an update is in it
		}
		if !keptExists(filepath.Join(dir, macvm.KeptFile)) {
			if _, err := removeKeptHeld(dir); err != nil {
				errs = append(errs, fmt.Errorf("docker: remove a kept folder with no record %s: %w", de.Name(), err))
			} else {
				slog.Info("docker: removed a kept folder a create left with no record", slog.String("key", de.Name()))
			}
		}
		unlock()
	}
	return errors.Join(errs...)
}

// keptExists reports whether p is there.
func keptExists(p string) bool { _, err := os.Lstat(p); return err == nil }

// EraseKeptMachines deletes every kept machine on this Mac, for a revoked runner: a VM booted from one is
// torn down first, then every machine folder goes, recorded or not, under its lock and with its update
// staging; a tools update holding the lock is waited for (keptEraseWait), and a machine whose lock is still
// held is reported not erased. Any other folder left in the kept folder goes too, and so do the starts'
// reservations and the Mac's reservation lock, so the folder is left as the erase without the driver leaves it.
func (d *Driver) EraseKeptMachines(ctx context.Context) error {
	c := d.macVMConfig()
	if c == nil {
		return nil
	}
	des, _ := os.ReadDir(c.Layout.RunsDir())
	for _, de := range des {
		cfg, err := macvm.ReadRunConfig(filepath.Join(c.Layout.RunsDir(), de.Name()))
		if runID, perr := uuid.Parse(cfg.RunID); err == nil && perr == nil && cfg.Kept != nil {
			d.teardownVM(ctx, agentContainerName(runID), runID)
		}
	}
	kept, err := os.ReadDir(c.Layout.KeptDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("docker: read the kept machines: %w", err)
	}
	var errs []error
	keys := map[string]bool{}
	for _, de := range kept {
		switch key := strings.TrimSuffix(strings.TrimSuffix(de.Name(), ".lock"), keptUpdateSuffix); {
		case isKeptKey(key):
			keys[key] = true // the machine, its lock, its update staging or that staging's lock
		case isKeptKey(strings.TrimSuffix(de.Name(), keptReserveSuffix)):
			_ = os.Remove(filepath.Join(c.Layout.KeptDir(), de.Name())) // a start's reservation
		case de.IsDir():
			if err := os.RemoveAll(filepath.Join(c.Layout.KeptDir(), de.Name())); err != nil {
				errs = append(errs, fmt.Errorf("docker: erase %s in the kept folder: %w", de.Name(), err))
			}
		}
	}
	for key := range keys {
		if _, err := removeKept(filepath.Join(c.Layout.KeptDir(), key), cmp.Or(c.eraseWait, keptEraseWait)); err != nil {
			errs = append(errs, fmt.Errorf("docker: kept machine folder %s was not erased: %w", key, err))
		}
	}
	// Removed while held, as RemoveKept removes a machine's lock, so a start waiting on it takes a new one. It
	// holds no machine, so one still held after keptReserveWait is left.
	if unlock, err := c.lockKeptReserve(); err == nil {
		_ = os.Remove(filepath.Join(c.Layout.KeptDir(), keptReserveLock+".lock"))
		unlock()
	}
	c.changed()
	if len(errs) == 0 {
		slog.Info("docker: erased this Mac's kept machines", slog.Int("machines", len(keys)))
	}
	return errors.Join(errs...)
}

// keptDiskSaid adds what this Mac's kept machines hold to an admission's disk refusal.
func (c *MacVMConfig) keptDiskSaid(err error) error {
	short := (*admission.Refusal)(nil)
	if c.LimitBytes <= 0 || !errors.As(err, &short) || short.DiskNeeded <= 0 {
		return err
	}
	folders, _ := c.keptFolders()
	var n int64
	for _, f := range folders {
		b, _ := macvm.KeptAllocatedBytes(f.dir)
		n += b
	}
	if n == 0 {
		return err
	}
	//lint:ignore ST1005 the sentence the person reads after the admission's own
	return fmt.Errorf("%w. Its kept machines hold %d GB (`wardyn-runnerd macos-vm machines`).", err, n>>30)
}

// keptBootScrub is the guest's boot scrub of a kept machine, from its Init answer; nil for any other run.
func keptBootScrub(spec runner.SandboxSpec, res guestagent.InitResult) *runner.KeptScrub {
	if spec.KeptMachine == nil || res.Scrub == nil {
		return nil
	}
	s := runner.KeptScrub(*res.Scrub)
	return &s
}

// keptBootBase is the base spec's kept machine's record names after this run's boot (for a fresh start,
// the clone resetKept made). Nil for a run with no kept machine, or a record this boot did not write.
func (c *MacVMConfig) keptBootBase(spec runner.SandboxSpec) *runner.KeptBase {
	k := spec.KeptMachine
	if k == nil {
		return nil
	}
	rec, err := macvm.ReadKept(c.keptDir(k.ID), k.ID.String())
	if err != nil || rec.ActiveRun != spec.RunID.String() || rec.BasePreparedAt.IsZero() {
		return nil
	}
	return &runner.KeptBase{Name: rec.Base, PreparedAt: rec.BasePreparedAt}
}
