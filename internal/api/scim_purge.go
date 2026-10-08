// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A purge is a suspension plus the removal of what a leaver leaves behind. It runs when the identity
// provider sends DELETE, or when the purge sweeper finds a suspended person past WARDYN_SCIM_PURGE_AFTER:
//
//  1. store.MarkIdentityPurged, one transaction under the identity row FOR UPDATE: the row becomes a
//     permanent tombstone (reactivation refused, sign-in refused). The sweeper's re-check of "still
//     deactivated and due" is in the same transaction, so a reactivation that committed first wins and
//     one that comes later is refused;
//  2. the suspension, completed if it is not already (suspendIdentity);
//  3. every step below, each idempotent and recorded in deprovision_jobs under kind purge: stored
//     credentials and masking copies erased through erasure.Orchestrate, workspaces handed to the
//     operator, user-subject grants and assignments deleted, then one person.deprovision row.
//
// The grants and assignments are deleted by a direct store call, not the GOV4 apply path. That is the one
// ungoverned delete of deny rows, and it is safe because the tombstone's subject can never authenticate
// again. Drives are listed in the audit row and never reclaimed.

const (
	jobStepErase      = "erase"
	jobStepWorkspaces = "workspaces"
	jobStepGrants     = "grants"

	// scimSweepBatch caps how many identities one sweep tick handles of each kind; the rest wait a tick.
	scimSweepBatch = 50
	// scimResumeIdle is how long an identity's ledger must sit untouched before the sweeper resumes it,
	// so it never races the request that is still working through it.
	scimResumeIdle = 10 * time.Minute

	scimSweeperSlot = "sweeper"
)

// scimPurgeScopes is the erasure a purge asks for (Q-SCIM5): the person's credentials, the masking
// copies of them and the components they saved. The record scopes (audit fields, run tasks,
// outputs, recordings) stay a deliberate POST /people/{principal}/erasure.
var scimPurgeScopes = []erasure.Scope{erasure.Credentials, erasure.MaskCopies, erasure.Components}

func (s *Server) handleSCIMDeleteUser(w http.ResponseWriter, r *http.Request) {
	st, ident, ok := s.scimIdentity(w, r)
	if !ok {
		return
	}
	if _, err := s.purgeIdentity(r.Context(), st, ident.ID, scimSlot(r.Context()), false); err != nil {
		s.scimServerError(w, r, "purge the user", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// purgeIdentity purges identity id. With requireDue (the sweeper) it first re-checks, under the row
// lock, that the identity is still deactivated and past its purge_after, and reports false without
// touching anything when it is not. Without it (DELETE) the purge is unconditional and repeatable: a
// retry finishes the steps that are still pending and writes nothing new once they are all done.
func (s *Server) purgeIdentity(ctx context.Context, st scimStore, id uuid.UUID, slot string, requireDue bool) (bool, error) {
	ctx = withActor(ctx, types.ActorSystem, scimActor)
	marked, err := st.MarkIdentityPurged(ctx, id, requireDue)
	if err != nil {
		return false, err
	}
	if requireDue && !marked {
		return false, nil
	}
	return true, s.finishPurge(ctx, st, id, slot)
}

func (s *Server) finishPurge(ctx context.Context, st scimStore, id uuid.UUID, slot string) error {
	if err := s.suspendIdentity(ctx, st, id, slot); err != nil {
		return err
	}
	ident, err := st.GetIdentity(ctx, id)
	if err != nil {
		return err
	}
	forms, err := s.leaverForms(ctx, st, ident)
	if err != nil {
		return err
	}
	isEmail := func(t string) bool { return strings.Contains(t, "@") && !slices.Contains(forms.bound, t) }
	principals := slices.DeleteFunc(slices.Clone(forms.targets), isEmail)
	emails := slices.DeleteFunc(slices.Clone(forms.targets), func(t string) bool { return !isEmail(t) })
	keys := []store.JobKey{{Step: jobStepWorkspaces}, {Step: jobStepGrants}}
	for _, p := range principals {
		keys = append(keys, store.JobKey{Step: jobStepErase, Target: p})
	}
	if err := st.EnsureDeprovisionJobs(ctx, id, store.JobKindPurge, keys); err != nil {
		return err
	}
	jobs, err := st.ListDeprovisionJobs(ctx, id, store.JobKindPurge)
	if err != nil {
		return err
	}
	steps := []error{
		s.purgeStep(ctx, st, id, jobs, store.JobKey{Step: jobStepWorkspaces}, func() (map[string]int, error) {
			return s.reassignWorkspaces(ctx, forms.targets)
		}),
		s.purgeStep(ctx, st, id, jobs, store.JobKey{Step: jobStepGrants}, func() (map[string]int, error) {
			// An address another principal holds keeps its rows: it can be a recycled one, and those are the new holder's.
			grants, assignments, kept, err := st.DeleteUserSubjectRows(ctx, id, principals, emails)
			return map[string]int{"grants_deleted": int(grants), "assignments_deleted": int(assignments), "email_rows_kept": int(kept)}, err
		}),
	}
	for _, p := range principals {
		steps = append(steps, s.purgeStep(ctx, st, id, jobs, store.JobKey{Step: jobStepErase, Target: p}, func() (map[string]int, error) {
			return s.eraseForPurge(ctx, p)
		}))
	}
	if err := errors.Join(steps...); err != nil {
		slog.WarnContext(ctx, "api: a purge step failed and stays pending", slog.String("identity", id.String()), slog.Any("err", err))
		return fmt.Errorf("%w: %w", errDeprovisionIncomplete, err)
	}
	if err := s.auditPurge(ctx, st, id, forms.targets); err != nil {
		slog.WarnContext(ctx, "api: a purge's audit row was not written and stays pending", slog.String("identity", id.String()), slog.Any("err", err))
		return fmt.Errorf("%w: %w", errDeprovisionIncomplete, err)
	}
	return nil
}

// purgeStep runs fn unless the ledger already holds key done, and records the outcome.
func (s *Server) purgeStep(ctx context.Context, st scimStore, id uuid.UUID, jobs []store.DeprovisionJob, key store.JobKey, fn func() (map[string]int, error)) error {
	if j, ok := jobByKey(jobs, key.Step, key.Target); ok && j.Done {
		return nil
	}
	detail, err := fn()
	if err != nil {
		return errors.Join(err, st.FailDeprovisionJob(ctx, id, store.JobKindPurge, key, err))
	}
	return st.FinishDeprovisionJob(ctx, id, store.JobKindPurge, key, detail)
}

// eraseForPurge erases one principal's credentials, masking copies and components. A scope whose backing is
// not configured on this server holds nothing of the person's, so it is skipped rather than failing
// the purge; a configured scope that fails keeps the step pending.
func (s *Server) eraseForPurge(ctx context.Context, principal string) (map[string]int, error) {
	orch := s.erasureOrchestrator(nil)
	scopes := slices.DeleteFunc(slices.Clone(scimPurgeScopes), func(sc erasure.Scope) bool { return orch.Steps[sc] == nil })
	detail := map[string]int{}
	if len(scopes) == 0 {
		return detail, nil
	}
	rep, err := orch.Orchestrate(ctx, principal, scopes)
	if m, ok := rep.Details[erasure.Credentials].(map[string]any); ok {
		detail["credentials_erased"], _ = m["count"].(int)
	}
	if m, ok := rep.Details[erasure.MaskCopies].(map[string]any); ok {
		detail["masks_fenced"], _ = m["runs_fenced"].(int)
	}
	if m, ok := rep.Details[erasure.Components].(map[string]any); ok {
		detail["components_erased"], _ = m["components"].(int)
	}
	return detail, err
}

// reassignWorkspaces hands every workspace a target owns to the operator, as POST
// /workspaces/{id}/reassign does, unless WARDYN_SCIM_LEAVER_WORKSPACES is keep.
func (s *Server) reassignWorkspaces(ctx context.Context, targets []string) (map[string]int, error) {
	n := 0
	if s.cfg.SCIM != nil && s.cfg.SCIM.KeepWorkspaces {
		return map[string]int{"workspaces_reassigned": n}, nil
	}
	all, err := s.cfg.Store.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	for _, ws := range all {
		if ws.OwnedBy == "" || !slices.ContainsFunc(targets, func(t string) bool { return strings.EqualFold(t, ws.OwnedBy) }) {
			continue
		}
		if _, err := s.cfg.Store.SetWorkspaceOwner(ctx, ws.ID, ""); err != nil {
			return nil, err
		}
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, scimActor, "workspace.reassign", ws.ID.String(), "success",
			mustJSON(map[string]any{"from_owner": ws.OwnedBy, "reason": "scim_purge"})))
		n++
	}
	return map[string]int{"workspaces_reassigned": n}, nil
}

// personDrives is the names of the drives allocated to any of subjects as a user, sorted. They are
// listed in the purge's audit row and never reclaimed.
func (s *Server) personDrives(ctx context.Context, subjects []string) ([]string, error) {
	grants, err := s.cfg.Store.ListUserDriveGrants(ctx)
	if err != nil {
		return nil, err
	}
	drives, err := s.cfg.Store.ListUserDrives(ctx)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, g := range grants {
		if g.SubjectType != types.CapabilitySubjectUser || !slices.ContainsFunc(subjects, func(t string) bool { return strings.EqualFold(t, g.Subject) }) {
			continue
		}
		if i := slices.IndexFunc(drives, func(d types.UserDriveListItem) bool { return d.ID == g.DriveID }); i >= 0 && !slices.Contains(names, drives[i].Name) {
			names = append(names, drives[i].Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// auditPurge writes the person.deprovision row of a purge, once, after every other step is done. A
// failed write leaves the step pending, so the retry writes it.
func (s *Server) auditPurge(ctx context.Context, st scimStore, id uuid.UUID, subjects []string) error {
	jobs, err := st.ListDeprovisionJobs(ctx, id, store.JobKindPurge)
	if err != nil {
		return err
	}
	if j, ok := jobByKey(jobs, store.JobStepAuditDeprovision, ""); ok && j.Done {
		return nil
	}
	counts := map[string]int{}
	for _, j := range jobs {
		for k, v := range j.Detail {
			counts[k] += v
		}
	}
	drives, err := s.personDrives(ctx, subjects)
	if err != nil {
		return err
	}
	key := store.JobKey{Step: store.JobStepAuditDeprovision}
	data := map[string]any{
		"kind": store.JobKindPurge, "credentials_erased": counts["credentials_erased"], "masks_fenced": counts["masks_fenced"], "components_erased": counts["components_erased"],
		"workspaces_reassigned": counts["workspaces_reassigned"], "grants_deleted": counts["grants_deleted"],
		"assignments_deleted": counts["assignments_deleted"], "email_rows_kept": counts["email_rows_kept"], "drives": drives,
	}
	ev := s.auditEvent(nil, types.ActorSystem, scimActor, "person.deprovision", id.String(), "success", mustJSON(data))
	if err := s.recordAuditStrict(ctx, ev); err != nil {
		return errors.Join(err, st.FailDeprovisionJob(ctx, id, store.JobKindPurge, key, err))
	}
	return st.FinishDeprovisionJob(ctx, id, store.JobKindPurge, key, nil)
}

// SweepSCIMPurge is one tick of the purge sweeper, run on the elected sweeper leader: it purges every
// suspended person past their purge_after (unless WARDYN_SCIM_PURGE_AFTER is zero) and resumes any
// suspension, purge or group removal whose identity provider stopped retrying. Idempotent, and a
// failure on one identity does not stop the others.
func (s *Server) SweepSCIMPurge(ctx context.Context) error {
	st, ok := s.cfg.Store.(scimStore)
	if !ok || s.cfg.SCIM == nil {
		return nil
	}
	ctx = withActor(ctx, types.ActorSystem, scimActor)
	var errs []error
	if s.cfg.SCIM.PurgeAfter > 0 {
		due, err := st.PurgeDueIdentities(ctx, scimSweepBatch)
		errs = append(errs, err)
		for _, id := range due {
			if purged, err := s.purgeIdentity(ctx, st, id, scimSweeperSlot, true); err != nil {
				errs = append(errs, err)
			} else if purged {
				slog.InfoContext(ctx, "api: scim: purged a suspended person past the purge delay", slog.String("identity", id.String()))
			}
		}
	}
	pending, err := st.PendingLeavers(ctx, scimResumeIdle, scimSweepBatch)
	errs = append(errs, err)
	for _, p := range pending {
		if p.Purged {
			errs = append(errs, s.finishPurge(ctx, st, p.ID, scimSweeperSlot))
		} else {
			errs = append(errs, s.suspendIdentity(ctx, st, p.ID, scimSweeperSlot))
		}
	}
	// A mover stays active, so their group removals are not leaver work and are resumed on their own. A
	// group deleted since still has its member's steps finished, with its external id unknown.
	removals, err := st.PendingGroupRemovals(ctx, scimResumeIdle, scimSweepBatch)
	errs = append(errs, err)
	for _, p := range removals {
		g, err := st.GetScimGroup(ctx, p.GroupID)
		if errors.Is(err, store.ErrNotFound) {
			g, err = store.ScimGroup{ID: p.GroupID}, nil
		}
		if err == nil {
			err = s.removeGroupMember(ctx, st, g, p.IdentityID, scimSweeperSlot, true)
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
