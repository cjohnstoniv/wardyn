// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// killStep is step 3. Every non-terminal run a target owns joins the ledger as pending, and so does
// any run the ledger counted done that is live again; then every pending run, including one already
// KILLED whose teardown failed, is torn down. The runs of every service identity a target sponsors join
// too: the identity goes sponsor_inactive with the person, and its runs end with theirs. A failed read of
// those identities, or of one identity's runs, never holds back the person's own runs: they are killed, and
// the step fails, so the ledger's audit rows stay pending and the retry reads the identities again.
func (s *Server) killStep(ctx context.Context, o leaverOrigin, st scimStore, id uuid.UUID, targets []string) error {
	jobs, servicesErr, err := s.joinKillRuns(ctx, o, st, id, targets)
	if err != nil {
		return err
	}
	var pending []uuid.UUID
	for _, j := range jobs {
		if runID, perr := uuid.Parse(j.Target); perr == nil && j.Step == jobStepKillRun && !j.Done {
			pending = append(pending, runID)
		}
	}
	spared, err := runsMadeAfterBound(ctx, st, pending)
	if err != nil {
		return err
	}
	var errs []error
	for _, j := range jobs {
		if j.Step != jobStepKillRun || j.Done {
			continue
		}
		runID, perr := uuid.Parse(j.Target)
		if perr != nil {
			continue
		}
		errs = append(errs, s.killPendingRun(ctx, o, st, id, j, runID, spared[runID]))
	}
	return errors.Join(servicesErr, errors.Join(errs...))
}

// joinKillRuns is killStep's first stage: the ledger's kill rows brought up to date with the runs the targets
// and the identities they sponsor own, and the ledger read again. servicesErr is the failed read of the
// sponsored identities or of one's runs, which the step reports after the person's own runs are killed; err
// is a failure that ends the step at once.
func (s *Server) joinKillRuns(ctx context.Context, o leaverOrigin, st scimStore, id uuid.UUID, targets []string) (jobs []store.DeprovisionJob, servicesErr, err error) {
	jobs, err = st.ListDeprovisionJobs(ctx, id, store.JobKindSuspend)
	if err != nil {
		return nil, nil, err
	}
	services, servicesErr := s.sponsoredServicePrincipals(ctx, targets)
	add, servicesErr, err := collectKillKeys(ctx, o, st, id, jobs, targets, services, servicesErr)
	if err != nil {
		return nil, nil, err
	}
	if led, at, guarded := o.portalLedger(ctx, st, id); guarded {
		if err := led.EnsureSuspensionJobsAt(ctx, id, add, at); err != nil {
			return nil, nil, err
		}
	} else if err := st.EnsureDeprovisionJobs(ctx, id, store.JobKindSuspend, add); err != nil {
		return nil, nil, err
	}
	if jobs, err = st.ListDeprovisionJobs(ctx, id, store.JobKindSuspend); err != nil {
		return nil, nil, err
	}
	return jobs, servicesErr, nil
}

// collectKillKeys reads the non-terminal runs of every target, then of every sponsored service identity, reopens
// the finished rows of runs that are live again and returns the keys of the runs the ledger does not hold yet.
// servicesErr comes back with the read failures of the service identities' runs joined onto it.
func collectKillKeys(ctx context.Context, o leaverOrigin, st scimStore, id uuid.UUID, jobs []store.DeprovisionJob, targets, services []string, servicesErr error) (add []store.JobKey, joined, err error) {
	for i, t := range append(slices.Clip(targets), services...) {
		runs, err := st.ListNonTerminalRunsBy(ctx, t)
		if err != nil && i >= len(targets) {
			// Nor does a sponsored identity whose runs cannot be read: the rest are killed, and the step fails.
			servicesErr = errors.Join(servicesErr, err)
			continue
		}
		if err != nil {
			return nil, servicesErr, err
		}
		for _, run := range runs {
			key, isNew, err := reopenKillRow(ctx, o, st, id, jobs, run.ID)
			if err != nil {
				return nil, servicesErr, err
			}
			if isNew {
				add = append(add, key)
			}
		}
	}
	return add, servicesErr, nil
}

// reopenKillRow is the ledger's answer for one live run: its key and true when no row holds it yet, a finished
// row reopened (false), or nothing to do (false).
func reopenKillRow(ctx context.Context, o leaverOrigin, st scimStore, id uuid.UUID, jobs []store.DeprovisionJob, runID uuid.UUID) (store.JobKey, bool, error) {
	key := store.JobKey{Step: jobStepKillRun, Target: runID.String()}
	switch j, ok := jobByKey(jobs, key.Step, key.Target); {
	case !ok:
		return key, true, nil
	case j.Done:
		if led, at, guarded := o.portalLedger(ctx, st, id); guarded {
			// The row this pass read, and only while its suspension is still the identity's: a later
			// suspension's finished row is not reopened by a pass that read an earlier one's.
			key.ID = j.ID
			return key, false, led.ReopenSuspensionJobAt(ctx, id, key, at)
		}
		return key, false, st.ReopenDeprovisionJob(ctx, id, store.JobKindSuspend, key)
	}
	return key, false, nil
}

// killPendingRun is killStep's second stage for one pending row: it closes the row of a run made after a
// reactivation, or claims the row, tears the run down and finishes or fails the row. The error is the row's
// own, which killStep joins with the others.
func (s *Server) killPendingRun(ctx context.Context, o leaverOrigin, st scimStore, id uuid.UUID, j store.DeprovisionJob, runID uuid.UUID, spared bool) error {
	key := store.JobKey{Step: j.Step, Target: j.Target}
	if o.portal {
		key.ID = j.ID
	}
	if spared && j.EventID == uuid.Nil {
		// A run the database stamped after the tail's bound, read while the person is active again, was made
		// after a reactivation: the suspension never owed its kill, so its row, whether this pass or an earlier
		// one added it, closes with no teardown and no kill counted. A row that already pins a run.kill receipt is
		// not spared: a claim pins it in the transaction that killed the run, so that kill was applied (whichever
		// worker made it, alive or not) and its teardown and receipt are still owed; the pass below takes it over.
		slog.InfoContext(ctx, "api: a suspension left a run made after its deactivation", slog.String("identity", id.String()), slog.String("run", j.Target))
		return finishStep(ctx, st, id, key, map[string]int{"runs_killed": 0})
	}
	tctx := ctx
	if led, at, guarded := o.portalLedger(ctx, st, id); guarded {
		// Before any runner I/O: the row is still the one this pass read, pending, under the current suspension.
		if err := led.ClaimSuspensionJobAt(ctx, id, key, at); err != nil {
			return err
		}
		// And the run's KILLED transition, with a kept machine's delete intent, is made in a transaction that
		// locks that row again (claimKill), so a replacement cannot land between this check and the transition.
		tctx = withKillGuard(ctx, killGuard{led: led, id: id, key: key, at: at})
	}
	if err := s.teardownRun(tctx, o, st, runID); err != nil {
		return failKillRow(ctx, tctx, st, id, key, err)
	}
	return finishKillRow(ctx, tctx, st, id, key)
}

// failKillRow is the row's answer to a teardown that failed with err.
func failKillRow(ctx, tctx context.Context, st scimStore, id uuid.UUID, key store.JobKey, err error) error {
	if errors.Is(err, store.ErrKillHeld) {
		// Another worker's claim of this occurrence owns the teardown and its receipt; its finish, or the end
		// of its lease, is what this row waits for.
		return err
	}
	// The failure is the attempt's own: it names the claim this attempt acquired, none when the error came
	// before any claim (the run could not be read, the claim statement failed), and the row takes it only
	// while that is still the row's claim and the row is pending.
	fkey := key
	if g, ok := killGuardOf(tctx); ok {
		fkey.Claim = killClaimOf(g)
	}
	failed := errors.Join(err, st.FailDeprovisionJob(ctx, id, store.JobKindSuspend, fkey, err))
	if g, ok := killGuardOf(tctx); ok && !errors.Is(err, store.ErrSuperseded) {
		// A teardown that failed after its claim leaves the claim for its retry to take at once.
		failed = errors.Join(failed, g.led.ReleaseSuspensionKillAt(ctx, id, key, killClaimOf(g)))
	}
	return failed
}

// finishKillRow finishes the row of a run whose teardown succeeded, counting one kill.
func finishKillRow(ctx, tctx context.Context, st scimStore, id uuid.UUID, key store.JobKey) error {
	fkey := key
	if g, ok := killGuardOf(tctx); ok {
		fkey.Claim = killClaimOf(g) // a finish by a worker whose claim another took over is refused
	}
	ferr := finishStep(ctx, st, id, fkey, map[string]int{"runs_killed": 1})
	if g, ok := killGuardOf(tctx); ok && errors.Is(ferr, store.ErrReceiptNotStored) {
		ferr = errors.Join(ferr, g.led.ReleaseSuspensionKillAt(ctx, id, key, killClaimOf(g))) // the receipt is not stored: the retry writes it
	}
	return ferr
}
