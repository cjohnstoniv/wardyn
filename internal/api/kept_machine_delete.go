// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A kept machine's lifecycle after its create: the runner's stop report keeps
// it, the Mac's inventory keeps one whose report never came, and every delete
// (the person's, an admin's, erasure, expiry, a kill by someone other than its
// owner) runs one protocol, the member drives' one for a Mac disk: a
// tombstone, then the Mac's kept_machine delete, deferred while the Mac is
// away and finished when it is back, and record-only once the runner is no
// longer registered (a revoked runner erases its kept machines itself).
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Who asked for a kept machine's delete, machine.delete's by.
const (
	keptMachineByOwner         = "owner"
	keptMachineByAdmin         = "admin"
	keptMachineByErasure       = "erasure"
	keptMachineByExpiry        = "expiry"
	keptMachineByNonOwnerKill  = "non_owner_kill"
	keptMachineByRevokedRunner = "revoked_runner"
)

// How a kept machine's session stopped, machine.keep's stop.
const (
	keptMachineStopGraceful   = "graceful"
	keptMachineStopHard       = "hard"
	keptMachineStopUnreported = "unreported" // the Mac's inventory showed it stopped; no stop report came
)

// The scrub outcomes (runner.KeptScrub.Outcome) the organisation reads.
const (
	keptScrubDone   = "done"
	keptScrubFailed = "failed"
	keptScrubNotRun = "not_run"
)

// keptMachineOutcomeUnconfirmed is machine.delete's outcome when the Mac's answer was lost or was an error
// other than a refusal: the folder may be gone already, so the tombstone stays and the sweep retries the
// delete, which is idempotent, until the Mac confirms it.
const keptMachineOutcomeUnconfirmed = "unconfirmed"

// keptMachineDeleteRefused reports whether err is the Mac's definite refusal, given before it removed
// anything: a typed refused reply (or this side's own, for a runner that keeps no machines).
func keptMachineDeleteRefused(err error) bool {
	var we *runnerwire.Error
	return errors.As(err, &we) && we.Code == runnerwire.CodeRefused
}

// keptMachineUnstartedAfter is how long after its keep run ended a machine still being created, which its
// Mac does not list, is dropped as never made: long enough for the Mac's next caps to list a clone.
const keptMachineUnstartedAfter = 10 * time.Minute

// keptMachineDeletion is what deleteKeptMachine did: outcome is machine.delete's (deleted,
// already_absent, record_only, deferred, unconfirmed or failed); err is a row that could not be finished.
type keptMachineDeletion struct {
	outcome string
	err     error
}

// keptMachineDeleteConfirmed reports whether outcome ends the delete: the Mac deleted the machine, held
// none, or is no longer registered. A caller says the machine "was deleted" only then.
func keptMachineDeleteConfirmed(outcome string) bool {
	return outcome == runnerwire.KeptMachineDeleted || outcome == runnerwire.KeptMachineAlreadyAbsent ||
		outcome == driveReclaimOutcomeRecordOnly
}

// keptMachineRunner reads runner id, store.ErrNotFound when it is not registered.
func (s *Server) keptMachineRunner(ctx context.Context, id uuid.UUID) (types.Runner, error) {
	st, ok := s.cfg.Store.(interface {
		GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error)
	})
	if !ok {
		return types.Runner{}, errors.New("api: the store keeps no runners")
	}
	return st.GetRunner(ctx, id)
}

// keptMachinesHeldHere reports whether this replica holds runnerID's session, online with caps that keep
// machines, so kept_machine reaches the Mac from here. Another replica's sweep finishes the rest.
func (s *Server) keptMachinesHeldHere(runnerID uuid.UUID) bool {
	caps, ok := s.runnerLastCaps(runnerID)
	return ok && caps.MacOSVM != nil && caps.MacOSVM.KeptMachines && s.runnerOnline(runnerID)
}

// keptMachineInventory is the kept machines runnerID's last caps list, by id; ok only when this replica
// holds the runner online and its caps carry an inventory.
func (s *Server) keptMachineInventory(runnerID uuid.UUID) (map[string]runnerwire.MacVMKeptMachine, bool) {
	caps, ok := s.runnerLastCaps(runnerID)
	if !ok || !s.runnerOnline(runnerID) || caps.MacOSVM == nil || caps.MacOSVM.Kept == nil {
		return nil, false
	}
	inv := map[string]runnerwire.MacVMKeptMachine{}
	for _, m := range caps.MacOSVM.Kept.Machines {
		inv[m.ID] = m
	}
	return inv, true
}

// keptMachineDeleteReady reports whether m's delete can run from here now: its runner is no longer
// registered, or this replica holds its Mac and the Mac has not booted it.
func (s *Server) keptMachineDeleteReady(ctx context.Context, m types.KeptMachine) bool {
	rn, err := s.keptMachineRunner(ctx, m.RunnerID)
	if errors.Is(err, store.ErrNotFound) || err == nil && rn.RevokedAt != nil {
		return true
	}
	inv, held := s.keptMachineInventory(m.RunnerID)
	return err == nil && held && s.keptMachinesHeldHere(m.RunnerID) && !inv[m.ID.String()].InUse
}

// deleteKeptMachine finishes the delete of a tombstoned machine: its Mac deletes the folder, then the row
// goes. Past the tombstone the delete finishes even if the caller goes away. A Mac no longer registered
// keeps nothing of Wardyn's to ask (its runner erased its kept machines when revoked), so only the row
// goes; an away Mac keeps the tombstone for its return. The Mac's definite refusal, given before it
// removed anything, puts the machine back to kept, except a kill's tombstone, which stays until the delete
// succeeds. Any other error keeps the tombstone (the Mac may have deleted the folder before its answer was
// lost, or failed after), and the sweep retries until deleted or already_absent. Every attempt is audited.
func (s *Server) deleteKeptMachine(ctx context.Context, ks store.KeptMachineStore, d store.KeptMachineDeletion) keptMachineDeletion {
	ctx = context.WithoutCancel(ctx)
	sctx, cancel := context.WithTimeout(ctx, driveReclaimTimeout)
	defer cancel()
	m := d.Machine
	var res keptMachineDeletion
	rn, err := s.keptMachineRunner(sctx, m.RunnerID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && rn.RevokedAt != nil:
		res.outcome = driveReclaimOutcomeRecordOnly
	case err != nil:
		res.outcome = driveReclaimOutcomeFailed
	case !s.keptMachinesHeldHere(m.RunnerID):
		res.outcome = driveReclaimOutcomeDeferred
	default:
		out, kerr := s.runnerEndpoint(m.RunnerID).sub.KeptMachine(sctx,
			runnerwire.KeptMachineArgs{Op: runnerwire.KeptMachineOpDelete, ID: m.ID.String(), By: d.By})
		res.outcome = out.Outcome
		if kerr == nil && !keptMachineDeleteConfirmed(res.outcome) {
			res.outcome = keptMachineOutcomeUnconfirmed // an answer this side does not know is no confirmation
		}
		if kerr != nil {
			slog.WarnContext(ctx, "api: a kept machine's delete failed on its Mac", slog.String("machine_id", m.ID.String()),
				slog.String("runner_id", m.RunnerID.String()), slog.String("error", kerr.Error()))
			res.outcome = keptMachineOutcomeUnconfirmed
			if keptMachineDeleteRefused(kerr) {
				res.outcome = driveReclaimOutcomeFailed
			}
		}
	}
	switch {
	case keptMachineDeleteConfirmed(res.outcome):
		if ferr := ks.FinishKeptMachineDelete(ctx, m.ID, d.At); ferr != nil && !errors.Is(ferr, store.ErrNotFound) {
			res.err = ferr
		}
	case res.outcome == driveReclaimOutcomeDeferred, res.outcome == keptMachineOutcomeUnconfirmed:
		// The tombstone stays: no run takes the machine, and the sweep finishes the delete.
	default:
		if aerr := ks.AbortKeptMachineDelete(ctx, m.ID, d.At); aerr != nil && !errors.Is(aerr, store.ErrNotFound) {
			slog.ErrorContext(ctx, "api: clear a kept machine's delete tombstone",
				slog.String("machine_id", m.ID.String()), slog.String("error", aerr.Error()))
		}
	}
	s.auditKeptMachineDelete(ctx, d, res.outcome)
	return res
}

// auditKeptMachineDelete writes machine.delete for one attempt: a confirmed or deferred delete is a
// success; refused (a run holds the machine), unconfirmed and failed are failures.
func (s *Server) auditKeptMachineDelete(ctx context.Context, d store.KeptMachineDeletion, outcome string) {
	result := "failure"
	if keptMachineDeleteConfirmed(outcome) || outcome == driveReclaimOutcomeDeferred {
		result = "success"
	}
	m := d.Machine
	data := map[string]any{"machine_id": m.ID, "owner": m.OwnerSubject, "runner_id": m.RunnerID, "by": d.By, "outcome": outcome}
	if d.By == keptMachineByNonOwnerKill {
		data["killer"] = d.Killer
	}
	actorType, actor := auditActorFromContext(ctx)
	s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "machine.delete", m.ID.String(), result, mustJSON(data)))
}

// recordKeptMachineStop records runnerID's stop report for the run that holds a kept machine: the
// machine is kept, with whether the stop scrubbed it (machine.keep), and the scrub's own report
// (machine.scrub). It means the sandbox stopped, whatever the run's state. A report for a run that no
// longer holds the machine, or one already recorded, changes nothing. The machine expires when the runner's
// record says (r.KeptExpiresAt, clamped), never by the site setting at this later moment; a stop that
// wrote no record (r.KeptExpiryPending) records nothing here.
func (s *Server) recordKeptMachineStop(ctx context.Context, runnerID uuid.UUID, r runner.MacOSVMDisk) {
	ks, ok := s.cfg.Store.(store.KeptMachineStore)
	if !ok {
		return
	}
	m, err := ks.GetKeptMachineByRun(ctx, r.RunID)
	if err != nil || m.RunnerID != runnerID {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "wardynd: read the kept machine of a stop report", slog.String("run_id", r.RunID.String()), slog.Any("err", err))
		}
		return
	}
	if r.KeptExpiryPending && r.KeptExpiresAt.IsZero() {
		// The Mac wrote no record at this stop, so no deadline exists yet; recording one from the site's
		// days would differ from the one its recovery gives. The sweep keeps the machine from its inventory
		// (reconcileKeptMachine) once the run is over and the Mac lists it.
		return
	}
	st := store.KeptMachineStop{ID: m.ID, RunnerID: runnerID, RunID: r.RunID, DiskBytes: m.DiskBytes, ExpiresAt: s.keptStopExpiry(ctx, r),
		Scrubbed: r.Scrub != nil && r.Scrub.Outcome == keptScrubDone}
	if inv, ok := s.keptMachineInventory(runnerID); ok {
		if on, listed := inv[m.ID.String()]; listed {
			st.DiskBytes = on.Bytes
		}
	}
	stop := keptMachineStopHard
	if r.Scrub != nil && r.Scrub.Outcome != keptScrubNotRun {
		stop = keptMachineStopGraceful
	}
	if err := s.keepKeptMachine(ctx, ks, st, stop, r.Scrub); err != nil {
		slog.WarnContext(ctx, "wardynd: record a kept machine's stop", slog.String("run_id", r.RunID.String()), slog.Any("err", err))
	}
}

// keptStopExpiry is the expiry of the machine a stop report r closes: the runner's own record of it
// (r.KeptExpiresAt), clamped to the stop (r.At, or now when that is zero or ahead of this clock) and the
// longest idle allowance after it. A report without one (an older runner) counts the site's idle days from
// now instead.
func (s *Server) keptStopExpiry(ctx context.Context, r runner.MacOSVMDisk) time.Time {
	if r.KeptExpiresAt.IsZero() {
		return s.keptMachineExpiry(ctx)
	}
	now := s.cfg.Now().UTC()
	at := r.At.UTC()
	if at.IsZero() || at.After(now) {
		at = now
	}
	return clampKeptExpiry(r.KeptExpiresAt.UTC(), at, at.AddDate(0, 0, types.KeptMachineIdleDaysMax))
}

// clampKeptExpiry is exp held within [lo, hi].
func clampKeptExpiry(exp, lo, hi time.Time) time.Time {
	if exp.Before(lo) {
		return lo
	}
	if exp.After(hi) {
		return hi
	}
	return exp
}

// keptMachineExpiry is when a machine that stops now expires: the site's idle days from now, as the
// runner counts them in kept.json.
func (s *Server) keptMachineExpiry(ctx context.Context) time.Time {
	days := types.KeptMachineIdleDaysDefault
	if sc, err := s.cfg.Store.GetSiteConfig(ctx); err == nil {
		days = sc.MacOSVM.KeptMachineIdleDaysOrDefault()
	}
	return s.cfg.Now().UTC().AddDate(0, 0, days)
}

// keepKeptMachine keeps the machine st's run holds and writes machine.keep, then machine.scrub when
// scrub is the stop's own report. A run that no longer holds it changes nothing and writes nothing.
func (s *Server) keepKeptMachine(ctx context.Context, ks store.KeptMachineStore, st store.KeptMachineStop, stop string, scrub *runner.KeptScrub) error {
	kept, err := ks.RecordKeptMachineStop(ctx, st)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	runID := st.RunID
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "machine.keep", kept.ID.String(), "success", mustJSON(map[string]any{
		"machine_id": kept.ID, "run_id": runID, "runner_id": st.RunnerID, "stop": stop, "scrubbed": kept.Scrubbed,
		"bytes": kept.DiskBytes, "expires_at": kept.ExpiresAt,
	})))
	if scrub == nil {
		return nil
	}
	result := "success"
	if scrub.Outcome == keptScrubFailed {
		result = "failure"
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "machine.scrub", kept.ID.String(), result, mustJSON(map[string]any{
		"machine_id": kept.ID, "run_id": runID, "when": scrub.When, "outcome": scrub.Outcome, "removed": scrub.Removed, "ca_sha256": scrub.CAs,
	})))
	return nil
}

// sweepKeptMachines is the kept machines' share of the runner-ready kick and the run watcher's sweep,
// on every replica: it closes a revoked runner's machines, reconciles each Mac this replica holds with
// its inventory, settles the kills' delete intents, starts the deletes of expired machines and finishes
// the deletes that waited for their Mac. A machine skipped now is met again on the next sweep.
func (s *Server) sweepKeptMachines(ctx context.Context) error {
	ks, ok := s.cfg.Store.(store.KeptMachineStore)
	if !ok {
		return nil
	}
	ctx = withActor(ctx, types.ActorSystem, "wardynd")
	all, _, err := ks.ListKeptMachines(ctx, store.KeptMachineFilter{}, store.Page{})
	if err != nil {
		return err
	}
	return errors.Join(s.reconcileKeptMachines(ctx, ks, all), s.settleKeptMachineKillIntents(ctx, ks),
		s.expireKeptMachines(ctx, ks), s.finishKeptMachineDeletes(ctx, ks))
}

// reconcileKeptMachines closes every machine of a revoked runner, record-only (the runner erases them on
// its side), and reconciles the machines of each Mac this replica holds with its caps inventory.
func (s *Server) reconcileKeptMachines(ctx context.Context, ks store.KeptMachineStore, all []types.KeptMachine) error {
	byRunner := map[uuid.UUID][]types.KeptMachine{}
	for _, m := range all {
		byRunner[m.RunnerID] = append(byRunner[m.RunnerID], m)
	}
	var errs []error
	for runnerID, ms := range byRunner {
		if rn, err := s.keptMachineRunner(ctx, runnerID); err == nil && rn.RevokedAt != nil {
			closed, cerr := ks.CloseKeptMachinesOfRunner(ctx, runnerID)
			for _, m := range closed {
				s.auditKeptMachineDelete(ctx, store.KeptMachineDeletion{Machine: m, By: keptMachineByRevokedRunner}, driveReclaimOutcomeRecordOnly)
			}
			errs = append(errs, cerr)
			continue
		}
		inv, ok := s.keptMachineInventory(runnerID)
		if !ok {
			continue
		}
		for _, m := range ms {
			errs = append(errs, s.reconcileKeptMachine(ctx, ks, m, inv))
		}
	}
	return errors.Join(errs...)
}

// reconcileKeptMachine settles one machine a run holds whose run has ended or lost its sandbox, against
// its Mac's inventory: listed and not in use, the stop report never came, so it is kept with the Mac's
// own scrub and size (machine.keep, stop unreported); not listed, still being created and its run long
// over, it was never made and its row is dropped. A machine whose run is live is left to that run.
func (s *Server) reconcileKeptMachine(ctx context.Context, ks store.KeptMachineStore, m types.KeptMachine, inv map[string]runnerwire.MacVMKeptMachine) error {
	if m.RunID == nil || m.State != types.KeptMachineCreating && m.State != types.KeptMachineInUse {
		return nil
	}
	run, err := s.cfg.Store.GetRun(ctx, *m.RunID)
	gone := errors.Is(err, store.ErrNotFound)
	if err != nil && !gone {
		return err
	}
	if !gone && !run.State.IsTerminal() && run.LostReason == "" {
		return nil
	}
	on, listed := inv[m.ID.String()]
	switch {
	case listed && !on.InUse:
		exp := on.ExpiresAt
		if exp.IsZero() {
			exp = s.keptMachineExpiry(ctx) // a listing with no expiry is not one that has passed
		}
		if hi := s.cfg.Now().UTC().AddDate(0, 0, types.KeptMachineIdleDaysMax); exp.After(hi) {
			exp = hi // the Mac's word, within the longest allowance
		}
		return s.keepKeptMachine(ctx, ks, store.KeptMachineStop{ID: m.ID, RunnerID: m.RunnerID, RunID: *m.RunID,
			Scrubbed: on.Scrubbed, DiskBytes: on.Bytes, ExpiresAt: exp}, keptMachineStopUnreported, nil)
	case !listed && m.State == types.KeptMachineCreating &&
		(gone || run.EndedAt != nil && s.cfg.Now().Sub(*run.EndedAt) > keptMachineUnstartedAfter):
		if err := ks.DropUnstartedKeptMachine(ctx, m.ID, *m.RunID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return nil
}

// claimKill is the kill's claim (claimKillTransition). A person who is not the run's creator, or a
// service identity, killing a run that is past its queue and not yet ended is the incident lever, and
// must not leave a person's machine behind: that claim goes through KillRunMarkingKeptMachine, which
// writes the delete intent on the machine the run holds in the claim's own transaction. A kill that
// ends the run therefore always leaves its delete for the sweep, even if this replica stops right
// after, and the intent blocks a resume until then; a lost claim marks nothing. Containment waits on
// no write but the claim. The owner's own kill and every system kill mark nothing: the machine is kept
// as a hard stop. The zero deletion means no intent.
func (s *Server) claimKill(ctx context.Context, run types.AgentRun, killerType types.ActorType, killer string) (bool, store.KeptMachineDeletion, error) {
	ks, ok := s.cfg.Store.(store.KeptMachineStore)
	nonOwner := killerType == serviceActorType || killerType == types.ActorHuman && killer != run.CreatedBy
	if !ok || !nonOwner || run.State.IsTerminal() || run.State == types.RunQueued {
		applied, err := s.claimKillTransition(ctx, run)
		return applied, store.KeptMachineDeletion{}, err
	}
	var intent store.KeptMachineDeletion
	applied, err := s.claimKillTransitionWith(ctx, run, func() (bool, error) {
		release := s.FenceRunStarts(ctx, run.ID)
		applied, d, err := ks.KillRunMarkingKeptMachine(ctx, run.ID, run.State, keptMachineByNonOwnerKill, killer,
			types.CanonicalUserSubject(killer))
		release()
		s.runStateMoved(run.ID, run.State, types.RunKilled, applied)
		intent = d
		return applied, err
	})
	return applied, intent, err
}

// finishKeptMachineKill tombstones the machine a won kill marked and runs its delete, once the teardown
// has stopped the VM. A Mac that still has it booted, or is away, leaves the tombstone to the sweep, and
// a tombstone that could not be written leaves the intent to it.
func (s *Server) finishKeptMachineKill(ctx context.Context, intent store.KeptMachineDeletion) {
	ks, ok := s.cfg.Store.(store.KeptMachineStore)
	if !ok || intent.At.IsZero() {
		return
	}
	d, err := ks.BeginKeptMachineDeleteForKill(ctx, intent.Machine.ID, intent.At)
	if err != nil || !s.keptMachineDeleteReady(ctx, d.Machine) {
		return // no tombstone from this intent (the claim was lost, or a delete finished it), or not yet
	}
	if res := s.deleteKeptMachine(ctx, ks, d); res.err != nil {
		slog.ErrorContext(ctx, "api: finish a kill's kept machine delete", slog.String("machine_id", d.Machine.ID.String()), slog.Any("err", res.err))
	}
}

// settleKeptMachineKillIntents settles each kill's delete intent no delete has started from, as a kill
// whose replica stopped before its tombstone left it: the run was KILLED (an intent is written only with
// that claim), so the delete starts, and runs now when its Mac is ready (else finishKeptMachineDeletes
// runs it); a run that is gone or not KILLED leaves no kill to finish, and the intent is cleared.
func (s *Server) settleKeptMachineKillIntents(ctx context.Context, ks store.KeptMachineStore) error {
	intents, err := ks.ListKeptMachineDeleteIntents(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range intents {
		if d.Machine.RunID == nil {
			continue
		}
		run, err := s.cfg.Store.GetRun(ctx, *d.Machine.RunID)
		switch {
		case err != nil && !errors.Is(err, store.ErrNotFound):
		case err == nil && run.State == types.RunKilled:
			var tomb store.KeptMachineDeletion
			if tomb, err = ks.BeginKeptMachineDeleteForKill(ctx, d.Machine.ID, d.At); err == nil && s.keptMachineDeleteReady(ctx, tomb.Machine) {
				err = s.deleteKeptMachine(ctx, ks, tomb).err
			}
		default:
			err = ks.ClearKeptMachineDeleteIntent(ctx, d.Machine.ID, d.At)
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// expireKeptMachines deletes each kept machine past its expiry.
func (s *Server) expireKeptMachines(ctx context.Context, ks store.KeptMachineStore) error {
	expired, err := ks.ListExpiredKeptMachines(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, m := range expired {
		errs = append(errs, s.expireKeptMachine(ctx, ks, m).err)
	}
	return errors.Join(errs...)
}

// expireKeptMachine starts and runs the delete of m, past its expiry. A resume that meets an expired
// machine calls it too, and says the machine was deleted only when keptMachineDeleteConfirmed holds for
// the outcome; until then it will be deleted. An empty outcome: the machine is in use or another delete
// has it.
func (s *Server) expireKeptMachine(ctx context.Context, ks store.KeptMachineStore, m types.KeptMachine) keptMachineDeletion {
	d, err := ks.BeginKeptMachineDelete(ctx, "", m.ID, keptMachineByExpiry)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrKeptMachineInUse), errors.Is(err, store.ErrKeptMachineDeleting):
		return keptMachineDeletion{}
	case err != nil:
		return keptMachineDeletion{err: err}
	}
	return s.deleteKeptMachine(ctx, ks, d)
}

// finishKeptMachineDeletes finishes each tombstoned machine's delete once this replica holds its Mac and
// the Mac has not booted it, or its runner is no longer registered. The tombstone is taken over once it
// is older than a minute, keeping its first by, and the delete runs as that one did.
func (s *Server) finishKeptMachineDeletes(ctx context.Context, ks store.KeptMachineStore) error {
	waiting, err := ks.ListKeptMachineDeletes(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, w := range waiting {
		if !s.keptMachineDeleteReady(ctx, w.Machine) {
			continue
		}
		d, err := ks.BeginKeptMachineDelete(ctx, "", w.Machine.ID, w.By)
		switch {
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrKeptMachineDeleting), errors.Is(err, store.ErrKeptMachineInUse):
			continue
		case err != nil:
			errs = append(errs, err)
			continue
		}
		errs = append(errs, s.deleteKeptMachine(ctx, ks, d).err)
	}
	return errors.Join(errs...)
}

// eraseKeptMachinesOf is the kept machines' part of the member_drives erasure scope: every machine the
// person keeps is deleted through deleteKeptMachine, as their own delete would be. A machine a run holds,
// one whose Mac is away and one that fails stay, and fail the scope by name; the rest are still deleted,
// and a retry finishes. It returns how many went.
func (s *Server) eraseKeptMachinesOf(ctx context.Context, person string) (int, error) {
	ks, ok := s.cfg.Store.(store.KeptMachineStore)
	if !ok {
		return 0, nil
	}
	owner := types.CanonicalUserSubject(person)
	mine, err := ks.ListKeptMachinesOf(ctx, owner)
	if err != nil {
		return 0, err
	}
	deleted := 0
	var errs []error
	for _, m := range mine {
		d, err := ks.BeginKeptMachineDelete(ctx, owner, m.ID, keptMachineByErasure)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue // deleted since the list
		case errors.Is(err, store.ErrKeptMachineInUse):
			errs = append(errs, fmt.Errorf("kept machine %q was kept: a run holds it", m.Name))
			continue
		case errors.Is(err, store.ErrKeptMachineDeleting):
			errs = append(errs, fmt.Errorf("kept machine %q is being deleted by another request", m.Name))
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("kept machine %q: %w", m.Name, err))
			continue
		}
		res := s.deleteKeptMachine(ctx, ks, d)
		switch {
		case res.err != nil:
			errs = append(errs, fmt.Errorf("kept machine %q: %w", m.Name, res.err))
		case keptMachineDeleteConfirmed(res.outcome):
			deleted++
		case res.outcome == driveReclaimOutcomeDeferred:
			errs = append(errs, fmt.Errorf("kept machine %q is being deleted: its Mac is offline, and it is removed when the Mac reconnects", m.Name))
		case res.outcome == keptMachineOutcomeUnconfirmed:
			errs = append(errs, fmt.Errorf("kept machine %q is being deleted: its Mac did not confirm the delete, which is retried", m.Name))
		default:
			errs = append(errs, fmt.Errorf("kept machine %q was kept: its Mac did not delete it", m.Name))
		}
	}
	return deleted, errors.Join(errs...)
}
