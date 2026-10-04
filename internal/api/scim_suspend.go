// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A suspension is three steps, each idempotent and recorded in deprovision_jobs, and the request
// that asked for it answers 5xx until every step is done, so the identity provider retries:
//
//  1. one transaction (store.SuspendIdentity): the session cutoff for every form the person is
//     known by, the identity deactivation, the authority epoch bump and people.deactivated_at;
//  2. revokePersonCredentials per form, after the bump, so every credential minted under the old
//     epoch is swept (a later mint is refused by the owner guard, not swept);
//  3. killRunCascade on every non-terminal run those forms own, and on every KILLED run whose
//     teardown the ledger still holds as pending.
//
// Step 1 commits first. Steps 2 and 3 are attempted on every retry whether or not the other failed,
// and the two audit rows are written only once both are done.

const (
	jobStepSweep       = "sweep"
	jobStepKillRun     = "kill_run"
	jobStepAuditDeact  = "audit_deactivate"
	jobStepAuditDeprov = "audit_deprovision"

	// teardownAttempts bounds how often one run is re-read after a lost KILLED compare-and-swap.
	teardownAttempts = 3
)

// errDeprovisionIncomplete is a suspension that has not finished: the identity is deactivated and
// every pending step is on the ledger, but the caller must retry.
var errDeprovisionIncomplete = errors.New("deprovisioning is incomplete; retry the request")

// scimStore is what the SCIM routes need of the store: the identity rows, the leaver operations
// and the people rows.
type scimStore interface {
	store.LeaverStore
	store.PrincipalIdentityStore
}

// leaverForms are every form a person is known by.
type leaverForms struct {
	// bound are the principals whose identity rows the suspension deactivates: what a sign-in
	// bound, and the object-id people principal. Deactivation never follows an email.
	bound []string
	// targets are bound plus every sub the emails resolve to (when no row binds a principal) plus
	// every email alias: the forms the cutoff, the credential sweep and the kill sweep run under.
	targets []string
}

func (s *Server) leaverForms(ctx context.Context, st scimStore, ident store.PrincipalIdentity) (leaverForms, error) {
	var f leaverForms
	f.bound = nonEmptyForms(ident.Principal)
	if ident.ObjectID != "" {
		tenant := ident.TenantID
		if tenant == "" && s.cfg.SCIM != nil {
			tenant = s.cfg.SCIM.Tenant
		}
		f.bound = nonEmptyForms(append(f.bound, entraPrincipalPrefix+tenant+":"+ident.ObjectID)...)
	}
	aliases, err := st.IdentityAliasValues(ctx, ident.ID)
	if err != nil {
		return f, err
	}
	emails := slices.DeleteFunc(append(aliases, strings.ToLower(ident.EmailLower), strings.ToLower(ident.ScimUserName)),
		func(v string) bool { return !strings.Contains(v, "@") })
	f.targets = slices.Concat(f.bound, nonEmptyForms(emails...))
	if ident.Principal == "" {
		subs, err := st.PrincipalsByEmail(ctx, emails)
		if err != nil {
			return f, err
		}
		f.targets = append(f.targets, subs...)
	}
	f.targets = nonEmptyForms(f.targets...)
	return f, nil
}

func nonEmptyForms(in ...string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

func jobByKey(jobs []store.DeprovisionJob, step, target string) (store.DeprovisionJob, bool) {
	i := slices.IndexFunc(jobs, func(j store.DeprovisionJob) bool { return j.Step == step && j.Target == target })
	if i < 0 {
		return store.DeprovisionJob{}, false
	}
	return jobs[i], true
}

// suspendIdentity runs the suspension of identity id to completion, or returns
// errDeprovisionIncomplete with whatever is left pending on the ledger. A suspension whose ledger is
// already done writes nothing new, so an identity provider that repeats the request costs nothing.
func (s *Server) suspendIdentity(ctx context.Context, st scimStore, id uuid.UUID, slot string) error {
	ctx = withActor(ctx, types.ActorSystem, scimActor)
	ident, err := st.GetIdentity(ctx, id)
	if err != nil {
		return err
	}
	forms, err := s.leaverForms(ctx, st, ident)
	if err != nil {
		return err
	}
	jobs, err := st.ListDeprovisionJobs(ctx, id, store.JobKindSuspend)
	if err != nil {
		return err
	}
	if cutoff, ok := jobByKey(jobs, store.JobStepCutoff, ""); ident.DeactivatedAt == nil || !ok || !cutoff.Done {
		if _, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: id, Principals: forms.bound, CutoffSubs: forms.targets}); err != nil {
			return err
		}
	}
	keys := []store.JobKey{{Step: jobStepAuditDeact}, {Step: jobStepAuditDeprov}}
	for _, t := range forms.targets {
		keys = append(keys, store.JobKey{Step: jobStepSweep, Target: t})
	}
	if err := st.EnsureDeprovisionJobs(ctx, id, store.JobKindSuspend, keys); err != nil {
		return err
	}
	stepErr := errors.Join(
		s.sweepStep(ctx, st, id, forms.targets),
		s.killStep(ctx, st, id, forms.targets),
	)
	if stepErr != nil {
		slog.WarnContext(ctx, "api: a suspension step failed and stays pending", slog.String("identity", id.String()), slog.Any("err", stepErr))
		return fmt.Errorf("%w: %w", errDeprovisionIncomplete, stepErr)
	}
	if err := s.auditSuspension(ctx, st, id, slot); err != nil {
		slog.WarnContext(ctx, "api: a suspension's audit rows were not written and stay pending", slog.String("identity", id.String()), slog.Any("err", err))
		return fmt.Errorf("%w: %w", errDeprovisionIncomplete, err)
	}
	return nil
}

// sweepStep is step 2: every pending target's tokens revoked and keys deleted.
func (s *Server) sweepStep(ctx context.Context, st scimStore, id uuid.UUID, targets []string) error {
	jobs, err := st.ListDeprovisionJobs(ctx, id, store.JobKindSuspend)
	if err != nil {
		return err
	}
	var errs []error
	for _, t := range targets {
		if j, ok := jobByKey(jobs, jobStepSweep, t); ok && j.Done {
			continue
		}
		key := store.JobKey{Step: jobStepSweep, Target: t}
		res, err := s.revokePersonCredentials(ctx, t)
		if err != nil {
			errs = append(errs, errors.Join(err, st.FailDeprovisionJob(ctx, id, store.JobKindSuspend, key, err)))
			continue
		}
		errs = append(errs, st.FinishDeprovisionJob(ctx, id, store.JobKindSuspend, key,
			map[string]int{"tokens_revoked": res.Tokens, "keys_deleted": res.Keys}))
	}
	return errors.Join(errs...)
}

// killStep is step 3. Every non-terminal run a target owns joins the ledger as pending, and so does
// any run the ledger counted done that is live again; then every pending run, including one already
// KILLED whose teardown failed, is torn down.
func (s *Server) killStep(ctx context.Context, st scimStore, id uuid.UUID, targets []string) error {
	jobs, err := st.ListDeprovisionJobs(ctx, id, store.JobKindSuspend)
	if err != nil {
		return err
	}
	var add []store.JobKey
	for _, t := range targets {
		runs, err := st.ListNonTerminalRunsBy(ctx, t)
		if err != nil {
			return err
		}
		for _, run := range runs {
			key := store.JobKey{Step: jobStepKillRun, Target: run.ID.String()}
			switch j, ok := jobByKey(jobs, key.Step, key.Target); {
			case !ok:
				add = append(add, key)
			case j.Done:
				if err := st.ReopenDeprovisionJob(ctx, id, store.JobKindSuspend, key); err != nil {
					return err
				}
			}
		}
	}
	if err := st.EnsureDeprovisionJobs(ctx, id, store.JobKindSuspend, add); err != nil {
		return err
	}
	if jobs, err = st.ListDeprovisionJobs(ctx, id, store.JobKindSuspend); err != nil {
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
		key := store.JobKey{Step: j.Step, Target: j.Target}
		if err := s.teardownRun(ctx, st, runID); err != nil {
			errs = append(errs, errors.Join(err, st.FailDeprovisionJob(ctx, id, store.JobKindSuspend, key, err)))
			continue
		}
		errs = append(errs, st.FinishDeprovisionJob(ctx, id, store.JobKindSuspend, key, map[string]int{"runs_killed": 1}))
	}
	return errors.Join(errs...)
}

// teardownRun kills a run and confirms the teardown: the cascade's error map must be empty, and a
// lost compare-and-swap is read as "someone else moved the run" and re-read, never as success. A run
// that has ended some other way than KILLED tore itself down. A KILLED run is killed again (the
// KILLED to KILLED repair), which is how a failed teardown is repaired.
func (s *Server) teardownRun(ctx context.Context, st scimStore, runID uuid.UUID) error {
	for range teardownAttempts {
		run, err := s.cfg.Store.GetRun(ctx, runID)
		if err != nil {
			return fmt.Errorf("read run %s: %w", runID, err)
		}
		if run.State.IsTerminal() && run.State != types.RunKilled {
			return nil
		}
		applied, killErrs, err := s.killRunCascade(ctx, run, types.ActorSystem, scimActor, map[string]any{"reason": "scim_suspend"})
		if err != nil {
			return fmt.Errorf("kill run %s: %w", runID, err)
		}
		if !applied {
			continue
		}
		if len(killErrs) > 0 {
			return fmt.Errorf("run %s is killed but its teardown failed: %v", runID, sortedKeys(killErrs))
		}
		return nil
	}
	return fmt.Errorf("run %s kept changing state under the kill", runID)
}

// auditSuspension writes scim.user.deactivate and person.deprovision, each once, after every other
// step is done. A failed write leaves its row pending, so the retry writes it.
func (s *Server) auditSuspension(ctx context.Context, st scimStore, id uuid.UUID, slot string) error {
	jobs, err := st.ListDeprovisionJobs(ctx, id, store.JobKindSuspend)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, j := range jobs {
		for k, v := range j.Detail {
			counts[k] += v
		}
	}
	rows := []struct {
		step, action string
		data         map[string]any
	}{
		{jobStepAuditDeact, "scim.user.deactivate", map[string]any{"slot": slot}},
		{jobStepAuditDeprov, "person.deprovision", map[string]any{
			"kind": store.JobKindSuspend, "sessions_cut": counts["sessions_cut"], "tokens_revoked": counts["tokens_revoked"],
			"keys_deleted": counts["keys_deleted"], "runs_killed": counts["runs_killed"],
		}},
	}
	var errs []error
	for _, row := range rows {
		if j, ok := jobByKey(jobs, row.step, ""); ok && j.Done {
			continue
		}
		key := store.JobKey{Step: row.step}
		ev := s.auditEvent(nil, types.ActorSystem, scimActor, row.action, id.String(), "success", mustJSON(row.data))
		if err := s.recordAuditStrict(ctx, ev); err != nil {
			errs = append(errs, errors.Join(err, st.FailDeprovisionJob(ctx, id, store.JobKindSuspend, key, err)))
			continue
		}
		errs = append(errs, st.FinishDeprovisionJob(ctx, id, store.JobKindSuspend, key, nil))
	}
	return errors.Join(errs...)
}

// recordAuditStrict is recordAudit for a row a retry depends on: the write's error comes back, so a
// step that could not be recorded stays pending.
func (s *Server) recordAuditStrict(ctx context.Context, ev types.AuditEvent) error {
	if s.cfg.Audit == nil {
		return nil
	}
	return s.cfg.Audit.Record(ctx, ev)
}
