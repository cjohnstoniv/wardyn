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

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A suspension is three steps, each idempotent and recorded in deprovision_jobs, and the request
// that asked for it answers 5xx until every step is done, so the identity provider retries:
//
//  1. one transaction (store.SuspendIdentity): the session cutoff for every form the person is
//     known by (less a bound person's email that another principal holds), the identity deactivation, the authority epoch bump and people.deactivated_at;
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

// errDeprovisionHeld is a purge that stops before its destructive steps until an admin acts: a retry does
// not finish it. The request still answers 5xx.
var errDeprovisionHeld = errors.New("deprovisioning is held until an admin acts; a retry does not finish it")

// scimStore is what the SCIM routes need of the store: the identity rows, the leaver operations
// and the groups.
type scimStore interface {
	store.LeaverStore
	store.PrincipalIdentityStore
	store.ScimGroupStore
	store.PrincipalDirectoryStore
}

// leaverForms are every form a person is known by.
type leaverForms struct {
	// bound are the principals whose identity rows the suspension deactivates: what a sign-in
	// bound, and the object-id people principal. Deactivation never follows an email.
	bound []string
	// targets are bound plus every email alias that is the person's plus, when no row binds a principal,
	// the one principal each email names: the forms the cutoff, the credential sweep and the kill sweep run under.
	targets []string
	// shared are the email forms of a bound person that another principal holds (store.HeldEmails). A
	// cutoff or session cut keyed by one would reach that principal's credentials too, so none is written;
	// the person's own are reached through bound and the identity deactivation.
	shared []string
	// notOwned is the leaver's entra:<tenant>:<object> form when deriveOwner does not prove it theirs, or when
	// another issuer's sign-in is bound to it and no row binds the leaver. A suspension is the leaver's cutoff
	// and still cuts it (it stays one of bound), but schedules no purge of it; a purge's destructive steps
	// leave it alone (destructive).
	notOwned string
	// notErased are the principals the form names besides the leaver (another issuer's sign-in with the same
	// ids, or a person of their own), with the form itself: a purge lists them as not erased
	// (notErasedDetail), and a suspension, which cut the form, lists them as also_named.
	notErased []string
	// unproved is the form when the directory names no one by it, or when it is notOwned and no row binds the
	// leaver: a purge refuses rather than leave what may be under it unnamed.
	unproved string
	// refusal is set when the emails cannot be resolved (reasonIdentityDirectoryUnreadable, with the error)
	// or when the emails of a person no row binds name more than one principal (reasonOwnerAmbiguous): the
	// forms then name nobody safely, targets are bound alone, and a suspension or a purge refuses.
	refusal string
}

// destructive are the forms a purge's destructive steps act on: these less notOwned.
func (f leaverForms) destructive() leaverForms {
	drop := func(v string) bool { return f.notOwned != "" && v == f.notOwned }
	f.bound, f.targets = slices.DeleteFunc(slices.Clone(f.bound), drop), slices.DeleteFunc(slices.Clone(f.targets), drop)
	return f
}

// cutTargets are the forms a cutoff or a session cut is written under: targets less shared.
func (f leaverForms) cutTargets() []string {
	return slices.DeleteFunc(slices.Clone(f.targets), func(t string) bool { return slices.Contains(f.shared, strings.ToLower(t)) })
}

// owns says a token is the person's own: its principal is one of their forms. A bound person's email
// alias is not an owner, since another principal can hold the address.
func (f leaverForms) owns(t types.APIToken) bool {
	return slices.ContainsFunc(f.targets, func(form string) bool { return strings.EqualFold(form, t.Principal) })
}

// proveForm sets notOwned, notErased and unproved for c, the entra form of ident.
func (f *leaverForms) proveForm(ctx context.Context, st scimStore, ident store.PrincipalIdentity, c string) error {
	if ident.Principal == "" {
		rows, err := st.IdentitiesByPrincipal(ctx, c)
		if slices.ContainsFunc(rows, func(r store.PrincipalIdentity) bool { return r.Issuer != ident.Issuer }) {
			f.notOwned, f.unproved = c, c
		}
		return err
	}
	if c == ident.Principal {
		return nil
	}
	v, others, err := deriveOwner(ctx, st, ident.Principal, c)
	switch {
	case err != nil:
		return err
	case v == derivedOthers:
		f.notOwned, f.notErased = c, nonEmptyForms(others...)
	case v == derivedUnknown:
		f.notOwned, f.unproved = c, c
	}
	return nil
}

func (s *Server) leaverForms(ctx context.Context, st scimStore, ident store.PrincipalIdentity) (leaverForms, error) {
	var f leaverForms
	f.bound = nonEmptyForms(ident.Principal)
	if ident.ObjectID != "" {
		tenant := ident.TenantID
		if tenant == "" && s.cfg.SCIM != nil {
			tenant = s.cfg.SCIM.Tenant
		}
		// The form SCIM names is always cut: a suspension fails toward cutting. A row bound to a principal
		// proves the form as an erasure does (deriveOwner), since another issuer's sign-in can share the tenant
		// and object id, and only a proved form is erased by a purge. A row no sign-in bound yet is known by its
		// form alone, unless another issuer's sign-in is bound to the form: nothing then tells the leaver's
		// form from that principal, and a purge refuses.
		c := entraPrincipalPrefix + tenant + ":" + ident.ObjectID
		f.bound = nonEmptyForms(append(f.bound, c)...)
		if err := f.proveForm(ctx, st, ident, c); err != nil {
			f.notOwned, f.unproved, f.refusal = c, c, reasonIdentityDirectoryUnreadable
			return f, err
		}
	}
	aliases, err := st.IdentityAliasValues(ctx, ident.ID)
	if err != nil {
		f.refusal, f.targets = reasonIdentityDirectoryUnreadable, f.bound
		return f, err
	}
	emails := slices.DeleteFunc(append(aliases, strings.ToLower(ident.EmailLower), strings.ToLower(ident.ScimUserName)),
		func(v string) bool { return !strings.Contains(v, "@") })
	// Each email is resolved as erasure resolves a name (store.PrincipalsNamed), so a principal always wins
	// over an email claim. A bound person's email that names only other principals (a person an admin
	// created under that address, say) is never one of their forms. A person no row binds is known by their
	// emails, so the one principal they name joins the forms; when the person's emails name more than one
	// principal, together or one email alone, they name nobody.
	var own, subs []string
	for _, e := range nonEmptyForms(emails...) {
		named, err := st.PrincipalsNamed(ctx, e)
		if err != nil {
			f.refusal = reasonIdentityDirectoryUnreadable
			return f, err
		}
		switch {
		case ident.Principal != "":
			if len(named) == 0 || slices.ContainsFunc(named, func(p string) bool { return slices.Contains(f.bound, p) }) {
				own = append(own, e)
			}
		case len(named) > 1:
			f.refusal = reasonOwnerAmbiguous
		default:
			own, subs = append(own, e), append(subs, named...)
		}
	}
	if ident.Principal == "" && len(slices.DeleteFunc(nonEmptyForms(subs...), func(p string) bool { return slices.Contains(f.bound, p) })) > 1 {
		f.refusal = reasonOwnerAmbiguous
	}
	if f.refusal != "" {
		f.targets = f.bound
		return f, nil
	}
	f.targets = nonEmptyForms(slices.Concat(f.bound, own, subs)...)
	if ident.Principal != "" {
		if f.shared, err = st.HeldEmails(ctx, ident.ID, f.bound, own); err != nil {
			f.refusal, f.targets, f.shared = reasonIdentityDirectoryUnreadable, f.bound, nil
			return f, err
		}
	}
	return f, nil
}

// deprovisionForms is leaverForms for a suspension or a purge of kind. Forms with a refusal reach no one:
// the step stops before it touches anything and leaves a person.deprovision row saying why, and the
// request answers 5xx so the identity provider retries.
func (s *Server) deprovisionForms(ctx context.Context, st scimStore, ident store.PrincipalIdentity, kind string) (leaverForms, error) {
	forms, err := s.leaverForms(ctx, st, ident)
	if forms.refusal == "" {
		return forms, err
	}
	_, err = s.refuseDeprovision(ctx, ident, kind, forms.refusal, err, false)
	return forms, err
}

// refuseDeprovision leaves the person.deprovision row saying why a step of kind stops, and returns the error
// the request answers 5xx with: err when the forms could not be read, errDeprovisionIncomplete otherwise.
// recorded is a step the ledger already holds pending with this reason: its row was written when it first
// stopped, so a retry answers the same without another. owed names the ledger step that must keep the row
// owed (oweRefusalAudit) when neither the store nor the audit spool accepted it, and is empty otherwise.
func (s *Server) refuseDeprovision(ctx context.Context, ident store.PrincipalIdentity, kind, reason string, err error, recorded bool) (owed string, _ error) {
	outcome := "failure"
	if err == nil {
		outcome, err = "denied", fmt.Errorf("%w: the person's emails name more than one principal", errDeprovisionIncomplete)
	}
	if recorded || s.recordDeprovisionRefusal(ctx, ident, kind, outcome, reason, "") {
		return "", err
	}
	return store.JobStepRefusalAudit + outcome + "/" + reason, err
}

// recordDeprovisionRefusal writes the person.deprovision row of a refused step, naming the unproved form when a purge
// stopped on one, and says whether it was durably accepted.
func (s *Server) recordDeprovisionRefusal(ctx context.Context, ident store.PrincipalIdentity, kind, outcome, reason, unproved string) bool {
	data := map[string]any{"kind": kind, "reason": reason}
	if unproved != "" {
		data["unproved"] = unproved
	}
	return s.recordAuditDurably(ctx, s.auditEvent(nil, types.ActorSystem, scimActor, "person.deprovision", ident.ID.String(), outcome, mustJSON(data)))
}

// errRefusalAuditOwed is the ledger error of a refusal row that is still owed.
var errRefusalAuditOwed = errors.New("the refusal's audit row is not yet written")

// oweRefusalAudit leaves the ledger owing the refusal row key names: a pending row of its own, whatever an
// earlier row of the same name says, that no resolution, completion or reset clears until
// drainRefusalAudits writes it.
func oweRefusalAudit(ctx context.Context, st scimStore, id uuid.UUID, kind string, key store.JobKey) error {
	return errors.Join(st.EnsureDeprovisionJobs(ctx, id, kind, []store.JobKey{key}),
		st.ReopenDeprovisionJob(ctx, id, kind, key),
		st.FailDeprovisionJob(ctx, id, kind, key, errRefusalAuditOwed))
}

// drainRefusalAudits writes the refusal rows jobs (one kind's ledger, or one group removal's) still owe,
// before the step goes on to cut or complete anything. A row durably accepted marks its ledger row done; one
// that is not stays owed. A purge's owed row is keyed by the unproved form it stopped on.
func (s *Server) drainRefusalAudits(ctx context.Context, st scimStore, ident store.PrincipalIdentity, kind string, jobs []store.DeprovisionJob) error {
	var errs []error
	for _, j := range jobs {
		owed, ok := strings.CutPrefix(j.Step, store.JobStepRefusalAudit)
		if !ok || j.Done {
			continue
		}
		outcome, reason, _ := strings.Cut(owed, "/")
		unproved := ""
		if kind == store.JobKindPurge {
			unproved = j.Target
		}
		key := store.JobKey{Step: j.Step, Target: j.Target}
		if s.recordDeprovisionRefusal(ctx, ident, kind, outcome, reason, unproved) {
			errs = append(errs, st.FinishDeprovisionJob(ctx, ident.ID, kind, key, nil))
		} else {
			errs = append(errs, st.FailDeprovisionJob(ctx, ident.ID, kind, key, errRefusalAuditOwed))
		}
	}
	return errors.Join(errs...)
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
	ident, forms, keys, err := s.suspendStepOne(ctx, st, id)
	// A sign-in that bound the row after it was read is seen under the suspension's lock: read it again, so
	// the newly bound principal is cut, swept and has its runs killed. A binding never changes once made,
	// so the second plan is the last one.
	if errors.Is(err, store.ErrIdentityRebound) {
		ident, forms, keys, err = s.suspendStepOne(ctx, st, id)
	}
	if err != nil {
		return err
	}
	if err := st.EnsureDeprovisionJobs(ctx, id, store.JobKindSuspend, keys); err != nil {
		return err
	}
	stepErr := errors.Join(
		s.sweepStep(ctx, st, id, ident, forms),
		s.killStep(ctx, st, id, forms.targets),
	)
	if stepErr != nil {
		slog.WarnContext(ctx, "api: a suspension step failed and stays pending", slog.String("identity", id.String()), slog.Any("err", stepErr))
		return fmt.Errorf("%w: %w", errDeprovisionIncomplete, stepErr)
	}
	if err := s.auditSuspension(ctx, st, id, slot, forms.notErased); err != nil {
		slog.WarnContext(ctx, "api: a suspension's audit rows were not written and stay pending", slog.String("identity", id.String()), slog.Any("err", err))
		return fmt.Errorf("%w: %w", errDeprovisionIncomplete, err)
	}
	return nil
}

// suspendStepOne reads the identity and its forms and, unless a finished suspension already covers it,
// runs step 1 with them. It returns what steps 2 and 3 run under.
func (s *Server) suspendStepOne(ctx context.Context, st scimStore, id uuid.UUID) (store.PrincipalIdentity, leaverForms, []store.JobKey, error) {
	ident, err := st.GetIdentity(ctx, id)
	if err != nil {
		return ident, leaverForms{}, nil, err
	}
	forms, err := s.leaverForms(ctx, st, ident)
	if err != nil && forms.refusal == "" {
		return ident, forms, nil, err
	}
	jobs, jobsErr := st.ListDeprovisionJobs(ctx, id, store.JobKindSuspend)
	if jobsErr != nil {
		if forms.refusal != "" {
			_, err = s.refuseDeprovision(ctx, ident, store.JobKindSuspend, forms.refusal, err, false)
		}
		return ident, forms, nil, errors.Join(err, jobsErr)
	}
	if err := s.drainRefusalAudits(ctx, st, ident, store.JobKindSuspend, jobs); err != nil {
		return ident, forms, nil, err
	}
	cutoff, ok := jobByKey(jobs, store.JobStepCutoff, "")
	stepOne := ident.DeactivatedAt == nil || !ok || !cutoff.Done
	if forms.refusal != "" {
		// The forms the identity row itself proves are still cut: the row and its own binding, never an
		// email, and not the object-id form while another sign-in may hold it. Step 1 stays pending with the
		// reason and no other step runs, so a retry that resolves every form runs the whole suspension.
		proven := forms.destructive().bound
		bound := 0
		if ident.Principal != "" {
			bound = 1
		}
		// A step 1 already stopped for this reason, under the same binding and with as many forms cut, has
		// nothing new to cut or record: the retry only touches its ledger row, so the sweeper still waits
		// before it looks again. A row the first stop could not write is owed in a ledger row of its own.
		again := ident.DeactivatedAt != nil && ok && !cutoff.Done && cutoff.LastError == forms.refusal &&
			cutoff.Detail[store.CutoffDetailBound] == bound && cutoff.Detail["sessions_cut"] == len(proven)
		var owed string
		owed, err = s.refuseDeprovision(ctx, ident, store.JobKindSuspend, forms.refusal, err, again)
		if owed != "" {
			err = errors.Join(err, oweRefusalAudit(ctx, st, id, store.JobKindSuspend, store.JobKey{Step: owed}))
		}
		if again {
			key := store.JobKey{Step: store.JobStepCutoff}
			return ident, forms, nil, errors.Join(err, st.FailDeprovisionJob(ctx, id, store.JobKindSuspend, key, errors.New(forms.refusal)))
		}
		if stepOne {
			_, partialErr := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: id, Principal: ident.Principal,
				Principals: proven, CutoffSubs: proven, Partial: forms.refusal})
			err = errors.Join(err, partialErr)
		}
		return ident, forms, nil, err
	}
	keys := []store.JobKey{{Step: jobStepAuditDeact}, {Step: jobStepAuditDeprov}}
	for _, t := range forms.targets {
		keys = append(keys, store.JobKey{Step: jobStepSweep, Target: t})
	}
	if stepOne {
		// The pending steps commit with the deactivation, so a crash right after it leaves them for the sweeper.
		plan := store.SuspendPlan{IdentityID: id, Principal: ident.Principal, Principals: forms.bound, PurgePrincipals: forms.destructive().bound,
			CutoffSubs: forms.cutTargets(), PendingJobs: keys}
		if s.cfg.SCIM != nil {
			plan.PurgeAfter = s.cfg.SCIM.PurgeAfter
		}
		if _, err := st.SuspendIdentity(ctx, plan); err != nil {
			return ident, forms, nil, err
		}
	}
	return ident, forms, keys, nil
}

// sweepStep is step 2: every pending target's tokens revoked and keys deleted.
func (s *Server) sweepStep(ctx context.Context, st scimStore, id uuid.UUID, ident store.PrincipalIdentity, forms leaverForms) error {
	targets := forms.targets
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
		res, err := s.sweepTarget(ctx, t, forms, ident)
		if err != nil {
			errs = append(errs, errors.Join(err, st.FailDeprovisionJob(ctx, id, store.JobKindSuspend, key, err)))
			continue
		}
		errs = append(errs, st.FinishDeprovisionJob(ctx, id, store.JobKindSuspend, key,
			map[string]int{"tokens_revoked": res.Tokens, "keys_deleted": res.Keys}))
	}
	return errors.Join(errs...)
}

// sweepTarget sweeps one form. A bound person's email is not an owner: another principal can hold the same
// address (a recycled one), so under an email the cutoff is written only when no other principal holds it,
// and the tokens and keys the sweep takes are those whose principal is one of the person's own forms. A
// person no row binds is known by nothing but their emails, so those run the full sequence.
func (s *Server) sweepTarget(ctx context.Context, target string, forms leaverForms, ident store.PrincipalIdentity) (personRevocation, error) {
	if ident.Principal == "" || !strings.Contains(target, "@") || slices.Contains(forms.bound, target) {
		return s.revokePersonCredentials(ctx, target)
	}
	if !slices.Contains(forms.shared, strings.ToLower(target)) {
		if err := s.cfg.SessionRevocations.RevokeSub(ctx, target); err != nil {
			return personRevocation{}, err
		}
	}
	res := personRevocation{Stamped: true}
	var tokenErr, keyErr error
	res.Tokens, tokenErr = s.revokeAPITokensMatching(ctx, target, forms.owns)
	res.Keys, _, _, _, keyErr = s.deleteSSHKeysFor(ctx, target, false)
	return res, errors.Join(tokenErr, keyErr)
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
func (s *Server) auditSuspension(ctx context.Context, st scimStore, id uuid.UUID, slot string, alsoNamed []string) error {
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
	if len(alsoNamed) > 0 {
		rows[1].data["also_named"] = alsoNamed
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
	// A row the audit spool holds is durably accepted: its drain writes it, so the step is done.
	if err := s.cfg.Audit.Record(ctx, ev); !errors.Is(err, audit.ErrSpooled) {
		return err
	}
	return nil
}
