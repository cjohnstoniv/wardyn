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
}

// KeptToolsUpdater updates the guest agent and pinned harness CLIs on a kept machine's disk, in its
// folder, when the machine's tools are behind this Mac's. It takes the machine's lock itself (ErrKeptLocked
// when a boot holds it), so the runner calls it unlocked, before the boot.
type KeptToolsUpdater interface {
	UpdateKeptTools(ctx context.Context, keptDir string) error
}

// keptScrubNotRun is a stop scrub's outcome on a hard stop, which runs none (runner.KeptScrub.Outcome).
const keptScrubNotRun = "not_run"

// keptDefaultIdleDays is the site's default kept_machine_idle_days, used when a spec carries none: a
// kept machine always has an expiry this Mac enforces on its own.
const keptDefaultIdleDays = 30

// keptIdleDays is the idle days a kept machine's expiry is counted with: the spec's, or the default.
func keptIdleDays(days int) int {
	if days <= 0 {
		return keptDefaultIdleDays
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
)

// keptRefusal is a refusal the person reads as it is.
func keptRefusal(sentence string) error { return errors.New(sentence) }

func keptSaidNotFound(id uuid.UUID) error {
	return keptRefusal("You have no kept machine " + id.String() + ".")
}

func keptSaidExpired(days int) error {
	return keptRefusal(fmt.Sprintf("Your machine wasn't used for %d days, so it was deleted. Start a new one.", days))
}

func keptSaidMacLimit(limit, need int64) error {
	return keptRefusal(fmt.Sprintf("This Mac keeps machines up to %d GB, and another would need up to %d GB more. "+
		"Delete one with `wardyn machines delete`, or raise the limit on that Mac.", limit>>30, (need+(1<<30)-1)>>30))
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
// resume's tools update, and the machine's own manifest in place of the base's.
func (d *Driver) refuseMacVMStart(ctx context.Context, spec runner.SandboxSpec) (macvm.Manifest, *macvm.KeptRun, error) {
	man, err := d.refuseMacVMSpec(spec)
	if err != nil {
		return man, nil, err
	}
	kept, err := d.keptStart(ctx, spec, &man)
	return man, kept, err
}

// keptStart is a kept machine session's part of a create, before the helper starts: the Mac's own checks
// of the machine, and for a resume the machine's tools update and its own manifest in man (what the
// run's size and harnesses are checked against, and what its disk reservation counts). It answers what
// run.json carries, nil for a run with no kept machine. The checks are advisory: the helper takes the
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
		held, err := c.keptWorstBytes()
		if err != nil {
			return nil, err
		}
		if need := keptWorst(*man); held+need > c.LimitBytes {
			return nil, keptSaidMacLimit(c.LimitBytes, need)
		}
		return c.keptRun(spec), nil
	}
	rec, err := macvm.ReadKept(dir, k.ID.String())
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, macvm.ErrKeptIDMismatch):
		return nil, keptSaidNotFound(k.ID)
	case err != nil:
		return nil, fmt.Errorf("docker: kept machine %s: %w", k.ID, err)
	case rec.OwnerHash != k.OwnerHash || rec.RunnerID != c.RunnerID:
		return nil, keptSaidNotFound(k.ID)
	case !rec.ExpiresAt.IsZero() && time.Now().After(rec.ExpiresAt):
		c.removeExpiredKept(dir, k.ID)
		return nil, keptSaidExpired(keptIdleDays(k.IdleDays))
	}
	if locked, err := macvm.KeptLocked(dir); err != nil || locked {
		return nil, cmp.Or(err, keptRefusal(keptSaidLocked))
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

// keptRun is the run.json block of spec's kept machine.
func (c *MacVMConfig) keptRun(spec runner.SandboxSpec) *macvm.KeptRun {
	k := spec.KeptMachine
	return &macvm.KeptRun{ID: k.ID.String(), Mode: k.Mode, OwnerHash: k.OwnerHash, RunnerID: c.RunnerID,
		Base: cmp.Or(spec.MacOSBase, macvm.StandardBase), IdleDays: keptIdleDays(k.IdleDays)}
}

// updateKeptTools runs the tools update on a resumed machine's folder before its boot. The runner holds no
// lock around it: the update takes the machine's lock itself, so no boot runs beside it, and the boot that
// follows takes it again. A machine whose update fails does not start; one another boot holds is locked.
func (c *MacVMConfig) updateKeptTools(ctx context.Context, spec runner.SandboxSpec, dir string) error {
	if c.Tools == nil {
		return nil
	}
	spec.NotifyWaiting(runner.MacOSStartDetail(runner.MacOSCloning))
	if err := c.Tools.UpdateKeptTools(ctx, dir); errors.Is(err, macvm.ErrKeptLocked) {
		return keptRefusal(keptSaidLocked)
	} else if err != nil {
		slog.Warn("docker: update a kept machine's tools", slog.String("machine_id", spec.KeptMachine.ID.String()), slog.String("err", err.Error()))
		return keptRefusal(keptSaidTools)
	}
	return nil
}

// keptStartErr words the helper's refusal of a kept machine's boot, which reaches the runner as the
// helper's status text; any other error is answered as it is. An expired machine is deleted.
func (c *MacVMConfig) keptStartErr(spec runner.SandboxSpec, err error) error {
	k := spec.KeptMachine
	if k == nil || err == nil {
		return err
	}
	switch said := err.Error(); {
	case strings.Contains(said, macvm.ErrKeptLocked.Error()):
		return keptRefusal(keptSaidLocked)
	case strings.Contains(said, macvm.ErrKeptOwner.Error()):
		return keptSaidNotFound(k.ID)
	case strings.Contains(said, macvm.ErrKeptIncomplete.Error()):
		return keptRefusal(keptSaidIncomplete)
	case strings.Contains(said, macvm.ErrKeptExpired.Error()):
		c.removeExpiredKept(c.keptDir(k.ID), k.ID)
		return keptSaidExpired(keptIdleDays(k.IdleDays))
	}
	return err
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
	if _, err := removeKept(dir); err != nil {
		slog.Warn("docker: delete a kept machine whose create failed", slog.String("machine_id", k.ID.String()), slog.String("err", err.Error()))
	}
}

// removeExpiredKept deletes a machine past its expiry; a machine a boot holds is left to the next sweep.
func (c *MacVMConfig) removeExpiredKept(dir string, id uuid.UUID) {
	if _, err := removeKept(dir); err != nil {
		slog.Warn("docker: delete an expired kept machine", slog.String("machine_id", id.String()), slog.String("err", err.Error()))
		return
	}
	slog.Info("docker: deleted an expired kept machine", slog.String("machine_id", id.String()))
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

// keptStopped records a kept machine's stop once its VM is down, under the machine's lock so a racing
// resume cannot interleave: the stop, whether its scrub finished, and the new expiry. A machine another
// boot already holds, or that is gone, is left as it is.
func (c *MacVMConfig) keptStopped(k *macvm.KeptRun, scrub *runner.KeptScrub, now time.Time) {
	dir := c.Layout.KeptMachineDir(k.ID)
	log := slog.With(slog.String("machine_id", k.ID))
	unlock, err := macvm.LockKept(dir)
	if err != nil {
		log.Warn("docker: record a kept machine's stop", slog.String("err", err.Error()))
		return
	}
	defer unlock()
	rec, err := macvm.ReadKept(dir, k.ID)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("docker: record a kept machine's stop", slog.String("err", err.Error()))
		}
		return
	}
	// A boot the helper refused leaves another owner's machine, or an expired one, as it was.
	if rec.OwnerHash != k.OwnerHash || rec.RunnerID != k.RunnerID || !rec.ExpiresAt.IsZero() && now.After(rec.ExpiresAt) {
		return
	}
	rec.LastStop, rec.StopKind = now, macvm.KeptStopHard
	if scrub.Outcome != keptScrubNotRun {
		rec.StopKind = macvm.KeptStopGraceful
	}
	rec.Scrubbed = scrub.Outcome == guestagent.ScrubDone
	rec.ExpiresAt = now.AddDate(0, 0, keptIdleDays(k.IdleDays))
	if err := macvm.WriteKept(dir, rec); err != nil {
		log.Warn("docker: record a kept machine's stop", slog.String("err", err.Error()))
	}
}

// endKeptMacVM is a kept machine session's end once its helper is gone and its run folder deleted: the
// machine's record, and the run's disk report, kept with the stop scrub's outcome (not_run for a hard stop).
func (d *Driver) endKeptMacVM(c *MacVMConfig, v *vmRun, k *macvm.KeptRun, scrub *runner.KeptScrub) {
	if scrub == nil {
		scrub = &runner.KeptScrub{When: guestagent.ScrubAtStop, Outcome: keptScrubNotRun}
	}
	now := time.Now().UTC()
	c.keptStopped(k, scrub, now)
	d.macDisks.Store(v.runID, runner.MacOSVMDisk{RunID: v.runID, State: runner.MacOSVMDiskKept, Reason: runner.MacOSVMDiskKeptMachine, At: now, Scrub: scrub})
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
	gone, err := removeKept(dir)
	switch {
	case errors.Is(err, macvm.ErrKeptLocked):
		return "", fmt.Errorf("docker: kept machine %s is running on this Mac", id)
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
		dir := filepath.Join(c.Layout.KeptDir(), de.Name())
		var probe macvm.KeptRecord
		if b, err := os.ReadFile(filepath.Join(dir, macvm.KeptFile)); err != nil || json.Unmarshal(b, &probe) != nil {
			continue
		}
		rec, err := macvm.ReadKept(dir, probe.ID)
		if err != nil || c.Layout.KeptMachineDir(rec.ID) != dir {
			continue
		}
		out = append(out, keptFolder{dir: dir, rec: rec})
	}
	return out, nil
}

// isKeptKey reports whether name is a kept machine's folder: its key, 12 hex digits (macvm.RunKey). The
// kept folder also holds each machine's lock and a tools update's staging, <key>.update and its lock, which
// are never machines.
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

// removeKept deletes a kept machine, holding its lock throughout as macvm.RemoveKept does, and with it a
// tools update's staging the update left (no update runs while the lock is held): the staging and its
// lock, the folder, then the folder's lock file. ErrKeptLocked while a boot or an update holds it; false
// with no error when there was no folder.
func removeKept(dir string) (bool, error) {
	unlock, err := macvm.LockKept(dir)
	if err != nil {
		return false, err
	}
	defer unlock()
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

// sweepKeptUpdate deletes a tools update's staging a kept machine's update left, only while it can hold the
// machine's lock: a machine a boot or an update holds keeps it.
func sweepKeptUpdate(dir string) error {
	if _, err := os.Lstat(dir + keptUpdateSuffix); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	unlock, err := macvm.LockKept(dir)
	if errors.Is(err, macvm.ErrKeptLocked) {
		return nil
	} else if err != nil {
		return err
	}
	defer unlock()
	if err := removeKeptUpdate(dir); err != nil && !errors.Is(err, macvm.ErrKeptLocked) {
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

// keptWorstBytes is the worst case of every kept machine on this Mac, what the limit is checked against.
func (c *MacVMConfig) keptWorstBytes() (int64, error) {
	folders, err := c.keptFolders()
	var n int64
	for _, f := range folders {
		n += keptWorstOf(f.dir)
	}
	return n, err
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
		inUse, _ := macvm.KeptLocked(f.dir)
		m := runnerwire.MacVMKeptMachine{ID: f.rec.ID, Bytes: n, InUse: inUse, Scrubbed: f.rec.Scrubbed, ExpiresAt: f.rec.ExpiresAt}
		if inUse {
			m.RunID = runs[f.rec.ID]
		}
		out.WorstBytes += keptWorstOf(f.dir)
		out.AllocatedBytes += n
		out.Machines = append(out.Machines, m)
	}
	return out, nil
}

// SweepKeptMachines deletes this Mac's kept machines that are past their expiry, offline too, and those
// another registration of this Mac made: a machine is bound to the registration that made it. A machine
// a boot holds is left to the next sweep. wardyn-runnerd runs it at start and with its periodic checks.
func (d *Driver) SweepKeptMachines() error {
	c := d.macVMConfig()
	if c == nil {
		return nil
	}
	folders, err := c.keptFolders()
	errs := []error{err}
	now := time.Now()
	for _, f := range folders {
		expired := !f.rec.ExpiresAt.IsZero() && now.After(f.rec.ExpiresAt)
		if !expired && f.rec.RunnerID == c.RunnerID {
			if err := sweepKeptUpdate(f.dir); err != nil {
				errs = append(errs, fmt.Errorf("docker: delete kept machine %s's update staging: %w", f.rec.ID, err))
			}
			continue
		}
		gone, err := removeKept(f.dir)
		switch {
		case errors.Is(err, macvm.ErrKeptLocked):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("docker: delete kept machine %s: %w", f.rec.ID, err))
		case gone:
			slog.Info("docker: deleted a kept machine", slog.String("machine_id", f.rec.ID), slog.Bool("expired", expired))
		}
	}
	c.changed()
	return errors.Join(errs...)
}

// EraseKeptMachines deletes every kept machine on this Mac, for a revoked runner: a VM booted from one is
// torn down first, then every machine folder goes, recorded or not, under its lock and with its update
// staging. Any other folder left in the kept folder goes too.
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
		case de.IsDir():
			if err := os.RemoveAll(filepath.Join(c.Layout.KeptDir(), de.Name())); err != nil {
				errs = append(errs, fmt.Errorf("docker: erase %s in the kept folder: %w", de.Name(), err))
			}
		}
	}
	for key := range keys {
		if _, err := removeKept(filepath.Join(c.Layout.KeptDir(), key)); err != nil {
			errs = append(errs, fmt.Errorf("docker: erase kept machine folder %s: %w", key, err))
		}
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
