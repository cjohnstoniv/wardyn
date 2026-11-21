// Copyright 2026 The Wardyn Authors
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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The lifecycle of a person an issuer portal introduced. Their identity row's issuer is delegate:person:<k>,
// which no SCIM issuer URL can equal, so every SCIM entry point leaves the row alone (scimOwns, scimAuthority),
// and none of them is changed for it. The portal has its own authority test (portalAuthority), its own entry
// points, its own store selectors and its own sweep. Only the step helpers beneath them are shared, and they
// take the origin of the work as a value (leaverOrigin).
//
// The portal's forms are the principal alone: the person is never known by an alias or an email, so there is
// nothing for a form read to fail on, and a suspension has no refusal to leave behind.

// errNotPortalOwned is an identity that is not a portal person of a bound issuer: no portal lifecycle work of
// it is run or resumed.
var errNotPortalOwned = errors.New("the identity is not a person of a bound issuer portal")

// portalLifecycleActor is the actor of every row the portal sweep writes.
const portalLifecycleActor = "portal-lifecycle"

// portalAdminSlot names an admin request in the deactivation row's slot, where SCIM names a bearer slot.
const portalAdminSlot = "admin"

// portalLifecycleStore is what the portal's lifecycle needs of the store: the shared ledger and identity
// reads, the portal's own writes and selectors, and the introduction events.
type portalLifecycleStore interface {
	scimStore
	store.PortalLifecycleStore
	store.PortalIdentityStore
}

func (s *Server) portalLifecycleStore() (portalLifecycleStore, bool) {
	st, ok := s.cfg.Store.(portalLifecycleStore)
	return st, ok
}

// portalOrigin is the origin of a portal suspension or purge by actor. The workspace policy for a portal
// person is the workforce default, handing them to the operator: WARDYN_SCIM_LEAVER_WORKSPACES is a SCIM
// setting and is not read here.
func portalOrigin(actorType types.ActorType, actor string) leaverOrigin {
	return leaverOrigin{actorType: actorType, actor: actor, killReason: "person_suspend", deactivateAction: "identity.deactivate", purgeReason: "person_purge"}
}

// portalSweepOrigin is the origin of the portal sweep's work.
func portalSweepOrigin() leaverOrigin { return portalOrigin(types.ActorSystem, portalLifecycleActor) }

// portalSlot is the slot a deactivation row names for work done as o.
func portalSlot(o leaverOrigin) string {
	if o.actor == portalLifecycleActor {
		return scimSweeperSlot
	}
	return portalAdminSlot
}

// portalIssuerKeyOf is k of an identity row's issuer delegate:person:<k>.
func portalIssuerKeyOf(issuer string) (string, bool) {
	k, ok := strings.CutPrefix(issuer, types.PortalPersonPrefix)
	return k, ok && k != ""
}

// portalAuthority is nil only when ident's issuer is delegate:person:<k>, its principal is one of that issuer's
// people, and k has a portal_issuers row; errNotPortalOwned otherwise. It reads no SCIM configuration. Every
// portal entry point and retry calls it on the row it has just read or locked.
func (s *Server) portalAuthority(ctx context.Context, ident store.PrincipalIdentity) error {
	k, ok := portalIssuerKeyOf(ident.Issuer)
	if !ok || !strings.HasPrefix(ident.Principal, ident.Issuer+":") {
		return errNotPortalOwned
	}
	pg, ok := s.cfg.Store.(store.PG)
	if !ok {
		return errNotPortalOwned
	}
	bound, err := store.IssuerKeyBoundQ(ctx, pg.Pool, k)
	if err != nil {
		return err
	}
	if !bound {
		return errNotPortalOwned
	}
	return nil
}

// portalForms is the one principal as the bound, cut and swept target. It reads no aliases and no email.
func portalForms(ident store.PrincipalIdentity) leaverForms {
	p := nonEmptyForms(ident.Principal)
	return leaverForms{bound: p, targets: slices.Clone(p)}
}

// warnNotPortalOwned logs that the portal lifecycle left identity id alone.
func warnNotPortalOwned(ctx context.Context, id uuid.UUID) {
	slog.WarnContext(ctx, "api: portal lifecycle: leaving an identity that is not a person of a bound issuer portal alone", slog.String("identity", id.String()), slog.Any("err", errNotPortalOwned))
}

// runPortalSuspension runs the suspension of portal person id to completion, or returns
// errDeprovisionIncomplete with whatever is left pending on the ledger. purgeAfter is the admin's deadline
// (0 on a resume, so a stored deadline is kept); with resumeOf, the deactivated_at a sweep's resume was listed
// with, a person reactivated or suspended again since is not suspended again (store.ErrSuperseded).
func (s *Server) runPortalSuspension(ctx context.Context, st portalLifecycleStore, id uuid.UUID, o leaverOrigin, purgeAfter time.Duration, resumeOf *time.Time) error {
	ctx = withActor(ctx, o.actorType, o.actor)
	ident, forms, keys, err := s.portalSuspendStepOne(ctx, st, id, o, purgeAfter, resumeOf)
	if errors.Is(err, errNotPortalOwned) {
		warnNotPortalOwned(ctx, id)
		return nil
	}
	if superseded(ctx, err, id) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := st.EnsureDeprovisionJobs(ctx, id, store.JobKindSuspend, keys); err != nil {
		return err
	}
	// Every tail is bound to the deactivation step one left, the first request's too: a reactivation can commit as
	// soon as step one releases the row, and what the person makes after it is not this suspension's.
	if ident.DeactivatedAt != nil {
		ctx = withSuspensionBound(ctx, id, *ident.DeactivatedAt)
	}
	stepErr := errors.Join(
		s.sweepStep(ctx, st, id, ident, forms),
		s.killStep(ctx, o, st, id, forms.targets),
	)
	// A suspension begun after a reactivation owns the ledger now (finishStep): it finishes and audits itself.
	if superseded(ctx, stepErr, id) {
		return nil
	}
	if stepErr != nil {
		slog.WarnContext(ctx, "api: a portal suspension step failed and stays pending", slog.String("identity", id.String()), slog.Any("err", stepErr))
		return fmt.Errorf("%w: %w", errDeprovisionIncomplete, stepErr)
	}
	if err := unlessSuperseded(ctx, s.auditSuspension(ctx, o, st, id, portalSlot(o), nil), id); err != nil {
		slog.WarnContext(ctx, "api: a portal suspension's audit rows were not written and stay pending", slog.String("identity", id.String()), slog.Any("err", err))
		return fmt.Errorf("%w: %w", errDeprovisionIncomplete, err)
	}
	return nil
}

// portalSuspendStepOne reads the identity, checks it is a portal person's and, unless a finished suspension
// already covers it, runs the store's step one with the one principal. With resumeOf it is store.ErrSuperseded,
// and nothing is written, once the identity is no longer deactivated at that instant. The store repeats that
// check under the row lock, since a reactivation can commit between this read and the write.
func (s *Server) portalSuspendStepOne(ctx context.Context, st portalLifecycleStore, id uuid.UUID, o leaverOrigin, purgeAfter time.Duration, resumeOf *time.Time) (store.PrincipalIdentity, leaverForms, []store.JobKey, error) {
	ident, err := st.GetIdentity(ctx, id)
	if err != nil {
		return ident, leaverForms{}, nil, err
	}
	if err := s.portalAuthority(ctx, ident); err != nil {
		return ident, leaverForms{}, nil, err
	}
	if resumeOf != nil && (ident.DeactivatedAt == nil || !ident.DeactivatedAt.Equal(*resumeOf)) {
		return ident, leaverForms{}, nil, store.ErrSuperseded
	}
	forms := portalForms(ident)
	jobs, err := st.ListDeprovisionJobs(ctx, id, store.JobKindSuspend)
	if err != nil {
		return ident, forms, nil, err
	}
	// A refusal row an earlier version of the ledger still owes is drained before the cut, and never holds it.
	if err := s.drainRefusalAudits(ctx, o, st, ident, store.JobKindSuspend, jobs); err != nil {
		slog.WarnContext(ctx, "api: an owed refusal row was not written and stays owed", slog.String("identity", id.String()), slog.Any("err", err))
	}
	cutoff, ok := jobByKey(jobs, store.JobStepCutoff, "")
	keys := []store.JobKey{{Step: jobStepAuditDeact}, {Step: jobStepAuditDeprov}}
	for _, t := range forms.targets {
		keys = append(keys, store.JobKey{Step: jobStepSweep, Target: t})
	}
	if ident.DeactivatedAt == nil || !ok || !cutoff.Done {
		// The pending steps commit with the deactivation, so a crash right after it leaves them for the sweep.
		plan := store.SuspendPlan{
			IdentityID: id, Principal: ident.Principal, Principals: forms.bound, PurgePrincipals: forms.bound, CutoffSubs: forms.cutTargets(),
			PendingJobs: keys, PurgeAfter: purgeAfter, ResumeOf: resumeOf, PortalLifecycle: true,
		}
		res, err := st.SuspendIdentity(ctx, plan)
		if err != nil {
			return ident, forms, nil, err
		}
		// The tail is bound to the deactivation this step left on the row under its lock.
		if !res.DeactivatedAt.IsZero() {
			ident.DeactivatedAt = &res.DeactivatedAt
		}
	}
	return ident, forms, keys, nil
}

// resumePortalSuspension is the sweep's recovery of a suspension it listed (store.PendingPortalLeavers). It goes
// on only while the identity is still deactivated at the instant it was listed with.
func (s *Server) resumePortalSuspension(ctx context.Context, st portalLifecycleStore, p store.PendingLeaver) error {
	if p.DeactivatedAt == nil {
		return nil
	}
	return s.runPortalSuspension(ctx, st, p.ID, portalSweepOrigin(), 0, p.DeactivatedAt)
}

// drainPortalOwedRefusals drains the refusal rows one portal person's ledger of one kind still owes, whatever
// became of the person since.
func (s *Server) drainPortalOwedRefusals(ctx context.Context, st portalLifecycleStore, o store.PendingRefusalAudit) error {
	ident, err := st.GetIdentity(ctx, o.IdentityID)
	if err != nil {
		return err
	}
	if err := s.portalAuthority(ctx, ident); err != nil {
		if errors.Is(err, errNotPortalOwned) {
			warnNotPortalOwned(ctx, o.IdentityID)
			return nil
		}
		return err
	}
	jobs, err := st.ListDeprovisionJobs(ctx, o.IdentityID, o.Kind)
	if err != nil {
		return err
	}
	return s.drainRefusalAudits(ctx, portalSweepOrigin(), st, ident, o.Kind, jobs)
}

// SweepPortalLifecycle is one tick of the portal sweep, run on the elected sweeper leader once issuer portals are
// activated, whatever the SCIM setting: it purges every suspended portal person past the deadline an admin gave
// them, resumes any suspension or purge that stopped part way, drains the refusal rows still owed and delivers
// the introduction events never delivered. Its four selectors put the portal prefix in the SQL before LIMIT, so
// a workforce row never fills a batch, and every item goes back through portalAuthority on the row it reads.
// Idempotent, and a failure on one person does not stop the others.
func (s *Server) SweepPortalLifecycle(ctx context.Context) error {
	st, ok := s.portalLifecycleStore()
	if !ok {
		return nil
	}
	act, ok := s.cfg.Store.(portalActivation)
	if !ok {
		return nil
	}
	on, err := act.PortalIssuersActivated(ctx)
	if err != nil || !on {
		return err
	}
	o := portalSweepOrigin()
	ctx = withActor(ctx, o.actorType, o.actor)
	var errs []error
	due, err := st.PurgeDuePortalIdentities(ctx, s.cfg.Now().UTC(), scimSweepBatch)
	errs = append(errs, err)
	for _, d := range due {
		// A schedule with no proof that the current deactivation is the person's own suspension's is left for an
		// admin, as the workforce sweep leaves it.
		if !d.Suspended {
			slog.WarnContext(ctx, "api: portal lifecycle: not purging a deactivated person whose deactivation is not proved their own suspension's", slog.String("identity", d.ID.String()))
			continue
		}
		if purged, err := s.purgePortalIdentity(ctx, st, d.ID, store.PortalPurgeScheduled, o); err != nil {
			errs = append(errs, err)
		} else if purged {
			slog.InfoContext(ctx, "api: portal lifecycle: purged a suspended person past their deadline", slog.String("identity", d.ID.String()))
		}
	}
	pending, err := st.PendingPortalLeavers(ctx, scimSweepBatch)
	errs = append(errs, err)
	for _, p := range pending {
		if p.Purged {
			errs = append(errs, s.finishPortalPurge(ctx, st, p.ID, o))
		} else {
			errs = append(errs, s.resumePortalSuspension(ctx, st, p))
		}
	}
	// An owed refusal row is drained whatever became of the person since, so it runs after the resumes above,
	// as the workforce sweep orders it.
	owed, err := st.PendingPortalRefusalAudits(ctx, scimSweepBatch)
	errs = append(errs, err)
	for _, ow := range owed {
		errs = append(errs, s.drainPortalOwedRefusals(ctx, st, ow))
	}
	intros, err := st.PendingPortalIntroductions(ctx, scimSweepBatch)
	errs = append(errs, err)
	for _, p := range intros {
		ident, err := st.GetIdentity(ctx, p.IdentityID)
		if err == nil {
			err = s.portalAuthority(ctx, ident)
		}
		if err != nil {
			if !errors.Is(err, errNotPortalOwned) {
				errs = append(errs, err)
			}
			continue
		}
		s.deliverPortalIntroduction(ctx, st, p)
	}
	return errors.Join(errs...)
}

// portalLifecycleStatus is GET /admin/portal-lifecycle/status: the portal's half of what /scim/status shows
// for the workforce, and it answers whatever the SCIM setting is, so stuck portal jobs have a view when SCIM
// is off.
type portalLifecycleStatus struct {
	Deactivated        []portalStatusDeactivated `json:"deactivated"`
	Pending            []scimStatusPending       `json:"pending"`
	PendingSuspensions []portalStatusPending     `json:"pending_suspensions"`
	PendingPurges      []portalStatusPending     `json:"pending_purges"`
}

type portalStatusDeactivated struct {
	Person        string     `json:"person"`
	DeactivatedAt time.Time  `json:"deactivated_at"`
	PurgeAfter    *time.Time `json:"purge_after,omitempty"`
}

type portalStatusPending struct {
	Person        string     `json:"person"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
}

// mountPortalLifecycleStatusRoute mounts the read on securityOps, beside /scim/status.
func (s *Server) mountPortalLifecycleStatusRoute(securityOps chi.Router) {
	securityOps.Get("/admin/portal-lifecycle/status", s.handlePortalLifecycleStatus)
}

func (s *Server) handlePortalLifecycleStatus(w http.ResponseWriter, r *http.Request) {
	st, ok := s.portalLifecycleStore()
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonDelegationStoreUnavailable, "issuer portals require the Postgres store backend")
		return
	}
	ctx := r.Context()
	out := portalLifecycleStatus{
		Deactivated: []portalStatusDeactivated{}, Pending: []scimStatusPending{},
		PendingSuspensions: []portalStatusPending{}, PendingPurges: []portalStatusPending{},
	}
	labels := map[uuid.UUID]string{}
	label := func(id uuid.UUID) (string, error) {
		if l, ok := labels[id]; ok {
			return l, nil
		}
		ident, err := st.GetIdentity(ctx, id)
		if err != nil {
			return "", err
		}
		labels[id] = portalLabel(ident)
		return labels[id], nil
	}
	deactivated, err := st.ListDeactivatedPortalIdentities(ctx, scimStatusListCap)
	if err != nil {
		writeServerError(w, r, "list deactivated portal people", err)
		return
	}
	for _, ident := range deactivated {
		labels[ident.ID] = portalLabel(ident)
		out.Deactivated = append(out.Deactivated, portalStatusDeactivated{Person: labels[ident.ID], DeactivatedAt: ident.DeactivatedAt.UTC(), PurgeAfter: utcPtr(ident.PurgeAfter)})
	}
	failures, err := st.ListPortalDeprovisionFailures(ctx, scimStatusListCap)
	if err != nil {
		writeServerError(w, r, "list portal deprovision failures", err)
		return
	}
	for _, f := range failures {
		person, err := label(f.IdentityID)
		if err != nil {
			writeServerError(w, r, "read an unfinished portal person", err)
			return
		}
		out.Pending = append(out.Pending, scimStatusPending{Person: person, Step: f.Step, LastError: f.LastError})
	}
	pending, err := st.PendingPortalLeavers(ctx, scimStatusListCap)
	if err != nil {
		writeServerError(w, r, "list pending portal suspensions and purges", err)
		return
	}
	for _, p := range pending {
		person, err := label(p.ID)
		if err != nil {
			writeServerError(w, r, "read an unfinished portal person", err)
			return
		}
		row := portalStatusPending{Person: person, DeactivatedAt: utcPtr(p.DeactivatedAt)}
		if p.Purged {
			out.PendingPurges = append(out.PendingPurges, row)
		} else {
			out.PendingSuspensions = append(out.PendingSuspensions, row)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// portalLabel names a portal person on the status view: their principal, since a portal row carries no email
// or SCIM name.
func portalLabel(i store.PrincipalIdentity) string {
	if i.Principal != "" {
		return i.Principal
	}
	return i.ID.String()
}
