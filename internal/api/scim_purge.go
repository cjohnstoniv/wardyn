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
	"github.com/cjohnstoniv/wardyn/internal/placement"
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
//     operator, user-subject grants and assignments deleted, runners and their unused registration
//     tokens revoked, the person's invitations deleted and their name cleared from others' invitations
//     and from service identities, then one person.deprovision row. A service identity they still
//     sponsor keeps its step pending, naming it, until it is handed to another sponsor or retired.
//
// The grants and assignments are deleted by a direct store call, not the GOV4 apply path. That is the one
// ungoverned delete of deny rows, and it is safe because the tombstone's subject can never authenticate
// again. Drives are listed in the audit row and never reclaimed.

const (
	jobStepErase      = "erase"
	jobStepWorkspaces = "workspaces"
	jobStepGrants     = "grants"
	jobStepRunners    = "runners"
	jobStepInvites    = "invitations"
	jobStepSponsor    = "service_identities"

	// scimSweepBatch caps how many identities one sweep tick handles of each kind; the rest wait a tick.
	scimSweepBatch = 50
	// scimResumeIdle is how long an identity's ledger must sit untouched before the sweeper resumes it,
	// so it never races the request that is still working through it.
	scimResumeIdle = 10 * time.Minute

	scimSweeperSlot = "sweeper"
)

// scimPurgeScopes is the erasure a purge asks for (Q-SCIM5): the person's credentials, the masking
// copies of them, the components they saved, their run setups (templates and template grants) and
// their own Self-Hosted runner pools.
// The record scopes (audit fields, run tasks, outputs, recordings) stay a deliberate
// POST /people/{principal}/erasure.
var scimPurgeScopes = []erasure.Scope{erasure.Credentials, erasure.MaskCopies, erasure.Components, erasure.RunSetups, erasure.RunnerPools}

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
	forms, err := s.deprovisionForms(ctx, st, ident, store.JobKindPurge)
	if err != nil {
		return err
	}
	if forms.unproved != "" {
		return s.refuseUnproved(ctx, st, ident, forms.unproved)
	}
	forms = forms.destructive()
	isEmail := func(t string) bool { return strings.Contains(t, "@") && !slices.Contains(forms.bound, t) }
	principals := slices.DeleteFunc(slices.Clone(forms.targets), isEmail)
	emails := slices.DeleteFunc(slices.Clone(forms.targets), func(t string) bool { return !isEmail(t) })
	keys := []store.JobKey{{Step: jobStepWorkspaces}, {Step: jobStepGrants}, {Step: jobStepRunners}, {Step: jobStepInvites}, {Step: jobStepSponsor}}
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
	// The owed rows are durable on the ledger, so a drain that cannot finish never holds the purge's steps.
	if err := s.drainRefusalAudits(ctx, st, ident, store.JobKindPurge, jobs); err != nil {
		slog.WarnContext(ctx, "api: an owed refusal row was not written and stays owed", slog.String("identity", id.String()), slog.Any("err", err))
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
		s.purgeStep(ctx, st, id, jobs, store.JobKey{Step: jobStepRunners}, func() (map[string]int, error) {
			return s.revokeLeaverRunners(ctx, principals)
		}),
		s.purgeStep(ctx, st, id, jobs, store.JobKey{Step: jobStepInvites}, func() (map[string]int, error) {
			return eraseLeaverInvitations(ctx, s.cfg.Store, principals)
		}),
		s.purgeStep(ctx, st, id, jobs, store.JobKey{Step: jobStepSponsor}, func() (map[string]int, error) {
			return eraseLeaverSponsorships(ctx, s.cfg.Store, principals)
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
	if err := s.auditPurge(ctx, st, id, forms); err != nil {
		slog.WarnContext(ctx, "api: a purge's audit row was not written and stays pending", slog.String("identity", id.String()), slog.Any("err", err))
		return fmt.Errorf("%w: %w", errDeprovisionIncomplete, err)
	}
	return nil
}

// refuseUnproved stops a purge whose leaver's entra form c is not proved theirs. The refusal is held on the
// ledger's audit step, where /scim/status lists it; a retry that finds it there writes no second denied row.
// A row neither the store nor the audit spool accepted is owed (oweRefusalAudit) until a drain writes it.
// No retry finishes it, so the refusal says what an admin does instead.
func (s *Server) refuseUnproved(ctx context.Context, st scimStore, ident store.PrincipalIdentity, c string) error {
	id := ident.ID
	remedy := "an admin erases what is under that form by subject"
	if ident.Principal == "" {
		remedy = "an admin binds the SCIM user to the person's sign-in, or removes the other issuer's sign-in bound to that form"
	}
	refused := fmt.Errorf("%w: the person's entra form %s is not proved theirs; %s", errDeprovisionHeld, c, remedy)
	key := store.JobKey{Step: store.JobStepAuditDeprovision}
	jobs, err := st.ListDeprovisionJobs(ctx, id, store.JobKindPurge)
	if err != nil {
		return err
	}
	if err := s.drainRefusalAudits(ctx, st, ident, store.JobKindPurge, jobs); err != nil {
		return err
	}
	var owed error
	if j, ok := jobByKey(jobs, key.Step, key.Target); (!ok || j.LastError != refused.Error()) &&
		!s.recordDeprovisionRefusal(ctx, ident, store.JobKindPurge, "denied", reasonOwnerAmbiguous, c) {
		owed = oweRefusalAudit(ctx, st, id, store.JobKindPurge, store.JobKey{Step: store.JobStepRefusalAudit + "denied/" + reasonOwnerAmbiguous, Target: c})
	}
	return errors.Join(refused, owed, st.FailDeprovisionJob(ctx, id, store.JobKindPurge, key, refused))
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

// eraseForPurge erases one principal's credentials, masking copies, components and run setups. A scope whose backing is
// not configured on this server holds nothing of the person's, so it is skipped rather than failing
// the purge; a configured scope that fails keeps the step pending.
func (s *Server) eraseForPurge(ctx context.Context, principal string) (map[string]int, error) {
	orch := s.erasureOrchestrator(nil, nil)
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
	if m, ok := rep.Details[erasure.RunSetups].(store.RunSetupsErased); ok {
		detail["templates_erased"] = m.Templates
	}
	if m, ok := rep.Details[erasure.RunnerPools].(map[string]any); ok {
		detail["runner_pools_deleted"], _ = m["runner_pools"].(int)
	}
	return detail, err
}

// revokeLeaverRunners revokes every runner the owners hold through the runner revoke path, as DELETE
// /runners/{id} does, and every registration token still redeemable for them, as DELETE
// /runners/tokens/{id} does, then clears the owners as the revoker of any runner and the minter of any
// token for someone else. A store without runners holds none.
func (s *Server) revokeLeaverRunners(ctx context.Context, owners []string) (map[string]int, error) {
	detail := map[string]int{"runners_revoked": 0, "runner_tokens_revoked": 0, "runner_attribution_cleared": 0}
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return detail, nil
	}
	now := s.cfg.Now().UTC()
	for _, owner := range owners {
		runners, err := st.ListRunnersByOwner(ctx, owner)
		if err != nil {
			return nil, err
		}
		for _, rn := range runners {
			if rn.State == types.RunnerRevoked {
				continue
			}
			rev, _, err := s.revokeRunner(ctx, rn.ID, types.ActorSystem, scimActor, "")
			if rev.Runner.ID != uuid.Nil {
				detail["runners_revoked"]++
			}
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
		}
		tokens, err := st.ListUnusedRunnerRegistrationTokensPage(ctx, now, owner, store.Page{})
		if err != nil {
			return nil, err
		}
		for _, t := range tokens {
			t, err := st.RevokeRunnerRegistrationToken(ctx, t.ID, now)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, scimActor, placement.ActionRunnerTokenRevoke, t.ID.String(), "success",
				mustJSON(map[string]any{"owner": t.Owner, "minted_by": t.MintedBy, "expires_at": t.ExpiresAt})))
			detail["runner_tokens_revoked"]++
		}
		cleared, err := st.EraseRunnerAttributionOf(ctx, owner)
		if err != nil {
			return nil, err
		}
		detail["runner_attribution_cleared"] += cleared
	}
	return detail, nil
}

// eraseLeaverInvitations deletes the invitations that target the principals and clears them from the
// ones they created or revoked. A store without invitations holds none.
func eraseLeaverInvitations(ctx context.Context, st store.Store, principals []string) (map[string]int, error) {
	detail := map[string]int{"invitations_deleted": 0, "invitations_cleared": 0}
	pg, ok := st.(store.PG)
	if !ok {
		return detail, nil
	}
	for _, p := range principals {
		deleted, cleared, err := pg.EraseInvitationsOf(ctx, p)
		if err != nil {
			return nil, err
		}
		detail["invitations_deleted"] += deleted
		detail["invitations_cleared"] += cleared
	}
	return detail, nil
}

// errStillSponsoring keeps a purge's service-identity step pending: a sponsor is what the identity's
// tokens and runs are checked against, so it is handed over or the identity retired, never cleared.
var errStillSponsoring = errors.New("the person still sponsors service identities")

// eraseLeaverSponsorships clears the principals as the creator, suspender or retirer of a service
// identity, and fails, naming them, while one still sponsors an identity that is not retired.
func eraseLeaverSponsorships(ctx context.Context, st store.Store, principals []string) (map[string]int, error) {
	detail := map[string]int{"service_identities_cleared": 0}
	pg, ok := st.(store.PG)
	if !ok {
		return detail, nil
	}
	var sponsored []string
	for _, p := range principals {
		cleared, names, err := pg.EraseServiceIdentityAttributionOf(ctx, p)
		if err != nil {
			return nil, err
		}
		detail["service_identities_cleared"] += cleared
		sponsored = append(sponsored, names...)
	}
	if len(sponsored) > 0 {
		return nil, fmt.Errorf("%w; hand each to another sponsor (PATCH /service-identities/{name}) or retire it (DELETE): %s",
			errStillSponsoring, strings.Join(sponsored, ", "))
	}
	return detail, nil
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

// personGrantedDrives is the names of the drives allocated to any of subjects as a user, sorted.
func (s *Server) personGrantedDrives(ctx context.Context, subjects []string) ([]string, error) {
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

// personDrives is the names of the drives allocated to any of subjects as a user and of the drives any
// of them created (member drives), sorted. They are listed in the purge's audit row and never reclaimed.
func (s *Server) personDrives(ctx context.Context, subjects []string) ([]string, error) {
	names, err := s.personGrantedDrives(ctx, subjects)
	if err != nil {
		return nil, err
	}
	drives, err := s.cfg.Store.ListUserDrives(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range memberDrivesOf(drives, subjects) {
		if !slices.Contains(names, d.Name) {
			names = append(names, d.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// personMemberDriveIDs is the ids of the drives any of subjects created, sorted. A member drive's name is
// unique only among its owner's, so the purge row also records ids and the status read follows those.
func (s *Server) personMemberDriveIDs(ctx context.Context, subjects []string) ([]string, error) {
	drives, err := s.cfg.Store.ListUserDrives(ctx)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, d := range memberDrivesOf(drives, subjects) {
		ids = append(ids, d.ID.String())
	}
	slices.Sort(ids)
	return ids, nil
}

// memberDrivesOf is the member drives in drives whose owner is any of subjects, tombstoned ones included.
func memberDrivesOf(drives []types.UserDriveListItem, subjects []string) []types.UserDriveListItem {
	return slices.DeleteFunc(slices.Clone(drives), func(d types.UserDriveListItem) bool {
		return d.OwnerSubject == "" || !slices.ContainsFunc(subjects, func(t string) bool { return strings.EqualFold(t, d.OwnerSubject) })
	})
}

// auditPurge writes the person.deprovision row of a purge, once, after every other step is done. A
// failed write leaves the step pending, so the retry writes it.
func (s *Server) auditPurge(ctx context.Context, st scimStore, id uuid.UUID, forms leaverForms) error {
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
	drives, err := s.personDrives(ctx, forms.targets)
	if err != nil {
		return err
	}
	granted, err := s.personGrantedDrives(ctx, forms.targets)
	if err != nil {
		return err
	}
	memberIDs, err := s.personMemberDriveIDs(ctx, forms.targets)
	if err != nil {
		return err
	}
	key := store.JobKey{Step: store.JobStepAuditDeprovision}
	data := map[string]any{
		"kind": store.JobKindPurge, "credentials_erased": counts["credentials_erased"], "masks_fenced": counts["masks_fenced"], "components_erased": counts["components_erased"],
		"runner_pools_deleted": counts["runner_pools_deleted"], "templates_erased": counts["templates_erased"], "workspaces_reassigned": counts["workspaces_reassigned"], "grants_deleted": counts["grants_deleted"],
		"assignments_deleted": counts["assignments_deleted"], "email_rows_kept": counts["email_rows_kept"], "drives": drives,
		"granted_drives": granted,
	}
	if len(memberIDs) > 0 {
		data["member_drive_ids"] = memberIDs
	}
	if ne, ok := notErasedDetail(forms.notErased); ok {
		data["not_erased"] = ne
	}
	ev := s.auditEvent(nil, types.ActorSystem, scimActor, "person.deprovision", id.String(), "success", mustJSON(data))
	if err := s.recordAuditStrict(ctx, ev); err != nil {
		return errors.Join(err, st.FailDeprovisionJob(ctx, id, store.JobKindPurge, key, err))
	}
	return st.FinishDeprovisionJob(ctx, id, store.JobKindPurge, key, nil)
}

// drainOwedRefusals drains the refusal rows one identity's ledger of one kind still owes.
func (s *Server) drainOwedRefusals(ctx context.Context, st scimStore, o store.PendingRefusalAudit) error {
	ident, err := st.GetIdentity(ctx, o.IdentityID)
	if err != nil {
		return err
	}
	jobs, err := st.ListDeprovisionJobs(ctx, o.IdentityID, o.Kind)
	if err != nil {
		return err
	}
	return s.drainRefusalAudits(ctx, st, ident, o.Kind, jobs)
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
		for _, d := range due {
			// A schedule with no proof that the identity's current deactivation is its own suspension's came
			// with another identity's suspension, or from a version that did not record the proof; it is left
			// for an admin, and /scim/status keeps it listed as deactivated.
			if !d.Suspended {
				slog.WarnContext(ctx, "api: scim: not purging a deactivated identity whose current deactivation is not proved its own suspension's",
					slog.String("identity", d.ID.String()))
				continue
			}
			if purged, err := s.purgeIdentity(ctx, st, d.ID, scimSweeperSlot, true); err != nil {
				errs = append(errs, err)
			} else if purged {
				slog.InfoContext(ctx, "api: scim: purged a suspended person past the purge delay", slog.String("identity", d.ID.String()))
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
	// An owed refusal row is drained whatever became of the person since: a reactivated person or a member
	// added back is no longer leaver or mover work above (run last, so a row those resumes just drained or
	// touched is not idle here and their resume is not held by this touch), but the row is owed all the same.
	owed, err := st.PendingRefusalAudits(ctx, scimResumeIdle, scimSweepBatch)
	errs = append(errs, err)
	for _, o := range owed {
		errs = append(errs, s.drainOwedRefusals(ctx, st, o))
	}
	return errors.Join(errs...)
}
