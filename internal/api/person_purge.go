// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A portal person's purge is the SCIM purge with the portal's own authority: the tombstone is marked under the
// row lock (store.MarkPortalIdentityPurged), the suspension is finished, and then each shared purge step runs
// under the portal's forms, the principal alone. It also holds the two reads that find a portal person.

// purgePortalIdentity purges portal person id as o. In PortalPurgeScheduled mode (the sweep) it first
// re-checks under the row lock that the person is still due, and reports false without touching anything
// when they are not. In PortalPurgeExplicit mode (the admin route) the purge is unconditional on the deadline
// and repeatable: a person already purged is a retry, which finishes the steps still pending. A person who is
// neither suspended nor purged is store.ErrPersonNotSuspended.
func (s *Server) purgePortalIdentity(ctx context.Context, st portalLifecycleStore, id uuid.UUID, mode store.PortalPurgeMode, o leaverOrigin) (bool, error) {
	ctx = withActor(ctx, o.actorType, o.actor)
	marked, err := st.MarkPortalIdentityPurged(ctx, id, mode)
	if err != nil {
		return false, err
	}
	if mode == store.PortalPurgeScheduled && !marked {
		return false, nil
	}
	return true, s.finishPortalPurge(ctx, st, id, o)
}

// finishPortalPurge runs a marked purge's steps, each idempotent and recorded in deprovision_jobs under kind
// purge: the suspension first (so a purge always finishes a suspension), then workspaces handed to the
// operator, grants and assignments deleted, runners and their registration tokens revoked, invitations
// deleted, sponsorships cleared and stored credentials erased, and last one person.deprovision row.
func (s *Server) finishPortalPurge(ctx context.Context, st portalLifecycleStore, id uuid.UUID, o leaverOrigin) error {
	ctx = withActor(ctx, o.actorType, o.actor)
	if err := s.runPortalSuspension(ctx, st, id, o, 0, nil); err != nil {
		return err
	}
	ident, err := st.GetIdentity(ctx, id)
	if err != nil {
		return err
	}
	if err := s.portalAuthority(ctx, ident); err != nil {
		if errors.Is(err, errNotPortalOwned) {
			warnNotPortalOwned(ctx, id)
			return nil
		}
		return err
	}
	forms := portalForms(ident)
	principals := forms.targets
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
	if err := s.drainRefusalAudits(ctx, o, st, ident, store.JobKindPurge, jobs); err != nil {
		slog.WarnContext(ctx, "api: an owed refusal row was not written and stays owed", slog.String("identity", id.String()), slog.Any("err", err))
	}
	steps := []error{
		s.purgeStep(ctx, st, id, jobs, store.JobKey{Step: jobStepWorkspaces}, func() (map[string]int, error) {
			return s.reassignWorkspaces(ctx, o, forms.targets)
		}),
		s.purgeStep(ctx, st, id, jobs, store.JobKey{Step: jobStepGrants}, func() (map[string]int, error) {
			n, err := st.DeleteUserSubjectRows(ctx, id, principals, nil)
			if err == nil {
				s.auditLeaverPoolPolicies(ctx, o, n.Pools)
			}
			return map[string]int{"grants_deleted": int(n.Grants), "assignments_deleted": int(n.Assignments), "email_rows_kept": int(n.Kept),
				"pools_narrowed": int(n.PoolsNarrowed), "pools_emptied": int(n.PoolsEmptied)}, err
		}),
		s.purgeStep(ctx, st, id, jobs, store.JobKey{Step: jobStepRunners}, func() (map[string]int, error) {
			return s.revokeLeaverRunners(ctx, o, principals)
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
		slog.WarnContext(ctx, "api: a portal purge step failed and stays pending", slog.String("identity", id.String()), slog.Any("err", err))
		return fmt.Errorf("%w: %w", errDeprovisionIncomplete, err)
	}
	if err := s.auditPurge(ctx, o, st, id, forms); err != nil {
		slog.WarnContext(ctx, "api: a portal purge's audit row was not written and stays pending", slog.String("identity", id.String()), slog.Any("err", err))
		return fmt.Errorf("%w: %w", errDeprovisionIncomplete, err)
	}
	return nil
}

// handlePurgePortalPerson is POST /people/{principal}/purge, super admin: the permanent tombstone of a
// suspended portal person. After it every exchange of that person is refused for good, whichever portal asks,
// and the same issuer and person id can never sign in again. A person already purged is a retry: it finishes any
// step still pending and answers 200.
func (s *Server) handlePurgePortalPerson(w http.ResponseWriter, r *http.Request) {
	st, ident, ok := s.resolvePortalPerson(w, r, "person.purge")
	if !ok {
		return
	}
	if ident.PurgedAt == nil && ident.DeactivatedAt == nil {
		s.denyPortalPerson(r, "person.purge", ident.Principal, reasonPersonNotSuspended)
		writeErrorReason(w, http.StatusConflict, reasonPersonNotSuspended, "the person is not suspended: suspend them first, then purge")
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "person.purge", ident.Principal, "success", mustJSON(map[string]any{"identity": ident.ID})))
	_, err := s.purgePortalIdentity(r.Context(), st, ident.ID, store.PortalPurgeExplicit, portalOrigin(actorType, actor))
	switch {
	case errors.Is(err, store.ErrPersonNotSuspended):
		// A reactivation that committed first wins.
		writeErrorReason(w, http.StatusConflict, reasonPersonNotSuspended, "the person is not suspended: suspend them first, then purge")
		return
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonPersonNotFound, "no such portal person")
		return
	case errors.Is(err, errDeprovisionIncomplete):
		writeErrorReason(w, http.StatusServiceUnavailable, reasonPortalLifecycleIncomplete, "the purge is recorded and some of its steps are still pending; retry the request")
		return
	case err != nil:
		writeServerError(w, r, "purge a portal person", err)
		return
	}
	s.writePortalPersonState(w, r, st, ident.ID)
}

// portalPersonView is how the reads and the lifecycle routes show a portal person.
type portalPersonView struct {
	Principal     string             `json:"principal"`
	State         string             `json:"state"`
	DisplayEmail  string             `json:"display_email"`
	DeactivatedAt *time.Time         `json:"deactivated_at,omitempty"`
	PurgeAfter    *time.Time         `json:"purge_after,omitempty"`
	PurgedAt      *time.Time         `json:"purged_at,omitempty"`
	PortalsUsed   []types.PortalUsed `json:"portals_used"`
}

const (
	portalStateActive      = "active"
	portalStateDeactivated = "deactivated"
	portalStatePurged      = "purged"
)

func portalState(deactivated, purged *time.Time) string {
	switch {
	case purged != nil:
		return portalStatePurged
	case deactivated != nil:
		return portalStateDeactivated
	}
	return portalStateActive
}

// handlePortalPersonLookup is GET /people/portal-person?issuer=<exact issuer>&person_id=<id>, security tier.
// An operator knows the issuer and the provider's person id, not the principal hash. It requires a binding for
// that exact issuer, computes the principal an exchange would, and reads the identity row. An issuer with no
// binding, a mis-spelling of a bound one included, is 404 portal_issuer_not_bound, so the operator knows to
// check the issuer string; a bound issuer with no such person is 404 person_not_found. A person never seen
// through any portal is never created by asking.
func (s *Server) handlePortalPersonLookup(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.cfg.Store.(store.PortalIdentityStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonDelegationStoreUnavailable, "issuer portals require the Postgres store backend")
		return
	}
	q := r.URL.Query()
	issuer, personID := q.Get("issuer"), q.Get("person_id")
	if issuer == "" || personID == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonPortalPersonParamInvalid, "issuer and person_id are both required")
		return
	}
	p, err := ps.LookupPortalPerson(r.Context(), issuer, personID)
	switch {
	case errors.Is(err, store.ErrPortalIssuerNotBound):
		writeErrorReason(w, http.StatusNotFound, reasonPortalIssuerNotBound, "no issuer portal is bound to that exact issuer: check its spelling")
		return
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonPersonNotFound, "that issuer is bound, and no portal has introduced a person with that id")
		return
	case err != nil:
		writeServerError(w, r, "look up a portal person", err)
		return
	}
	writeJSON(w, http.StatusOK, portalPersonOf(p))
}

func portalPersonOf(p types.PortalPerson) portalPersonView {
	used := p.PortalsUsed
	if used == nil {
		used = []types.PortalUsed{}
	}
	return portalPersonView{
		Principal: p.Principal, State: portalState(p.DeactivatedAt, p.PurgedAt), DisplayEmail: p.DisplayEmail,
		DeactivatedAt: utcPtr(p.DeactivatedAt), PurgedAt: utcPtr(p.PurgedAt), PortalsUsed: used,
	}
}

// handlePortalPersonPortals is GET /people/{principal}/portals, security tier: the portals one portal person
// used, each with its assigned unit (run_in_under) and when it was revoked, revoked history kept. A person is
// one per issuer and provider id, so only that person's own portals are listed.
func (s *Server) handlePortalPersonPortals(w http.ResponseWriter, r *http.Request) {
	st, ident, ok := s.resolvePortalPerson(w, r, "")
	if !ok {
		return
	}
	k, _ := portalIssuerKeyOf(ident.Issuer)
	pg, ok := s.cfg.Store.(store.PG)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonDelegationStoreUnavailable, "issuer portals require the Postgres store backend")
		return
	}
	bindings, err := store.PortalIssuersQ(r.Context(), pg.Pool)
	if err != nil {
		writeServerError(w, r, "read the issuer bindings", err)
		return
	}
	i := slices.IndexFunc(bindings, func(b types.PortalIssuer) bool { return b.IssuerKey == k })
	personID, valid := portalPersonIDOf(ident.Principal, k)
	if i < 0 || !valid {
		writeErrorReason(w, http.StatusNotFound, reasonPersonNotFound, "no such portal person")
		return
	}
	p, err := st.LookupPortalPerson(r.Context(), bindings[i].Issuer, personID)
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonPersonNotFound, "no such portal person")
		return
	}
	if err != nil {
		writeServerError(w, r, "read the portals a person used", err)
		return
	}
	writeJSON(w, http.StatusOK, portalPersonOf(p))
}

var portalBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// portalPersonIDOf is the provider's person id inside a portal principal delegate:person:<k>:<e>: the exact
// bytes e encodes. It is valid only when the principal is the one types.PortalPrincipal makes of them, so a
// spelling that is not canonical names nobody.
func portalPersonIDOf(principal, k string) (string, bool) {
	e, ok := strings.CutPrefix(principal, types.PortalPersonPrefix+k+":")
	if !ok || e == "" {
		return "", false
	}
	raw, err := portalBase32.DecodeString(strings.ToUpper(e))
	if err != nil || types.PortalPrincipal(k, string(raw)) != principal {
		return "", false
	}
	return string(raw), true
}
