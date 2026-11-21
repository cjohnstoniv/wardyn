// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// What a provider write does about service grants, for both doors (PUT /model-providers and PUT /site-config
// naming model_providers) and the grant's own DELETE: a body that leaves a grant out keeps it, a grant is
// dropped only by an explicit removal, and every removal that leaves a secret record to delete writes a claim
// on the retired uid in the same statement as the change. The doors hold the site-config document lock from
// before the plan to after the in-line delete.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const serviceGrantsNotActivatedMsg = "Service grants can be set up once every server runs 0.10.2 and a super admin has switched 0.10.2 on; see Operations, Stopped-writer upgrade"

// serviceGrantRefusal is a write the plan refused: the status, reason and sentence to answer with.
type serviceGrantRefusal struct {
	status int
	reason string
	msg    string
}

// write answers the refusal and reports whether there was one. A nil refusal writes nothing.
func (f *serviceGrantRefusal) write(w http.ResponseWriter) bool {
	if f == nil {
		return false
	}
	writeErrorReason(w, f.status, f.reason, f.msg)
	return true
}

// serviceGrantPlan is a provider write's grant change and the claims its in-line clean-up works through.
type serviceGrantPlan struct {
	change store.ServiceGrantChange
	order  []string // claim uids, in a stable order
}

// Reasons recorded on a claim: why the grant, and with it the record, was removed.
const (
	cleanupReasonGrantRemoved    = "grant_removed"
	cleanupReasonProviderRemoved = "provider_removed"
	cleanupReasonOIDCRemoved     = "oidc_removed"
	cleanupReasonInert           = "inert_entry"
)

// planServiceGrantDoor is planServiceGrantWrite for a door: it answers a refusal or a failure itself, and ok=false
// means the answer is written.
func (s *Server) planServiceGrantDoor(w http.ResponseWriter, r *http.Request, existing, block *types.ModelProviders, wholeBlock bool) (serviceGrantPlan, bool) {
	plan, refusal, err := s.planServiceGrantWrite(r.Context(), existing, block, wholeBlock, "", "")
	if refusal.write(w) {
		return plan, false
	}
	if err != nil {
		writeServerError(w, r, "plan the service grants", err)
		return plan, false
	}
	return plan, true
}

// planServiceGrantWrite prepares a write of block over existing (both as GetSiteConfig returns them, grants
// attached to rows that have an oidc block). It carries a stored grant onto each retained row whose body names
// none, refuses a grant on a uid that still has a clean-up claim, and works out which grants the write drops and
// the claim each leaves. wholeBlock is false when the door does not write model_providers at all, which changes
// nothing. It reads the secret store's revision of every record that is to go, so a failed read fails the write.
func (s *Server) planServiceGrantWrite(ctx context.Context, existing, block *types.ModelProviders, wholeBlock bool, why, removed string) (serviceGrantPlan, *serviceGrantRefusal, error) {
	var plan serviceGrantPlan
	gs, ok := s.serviceGrantStore()
	if !ok || !wholeBlock {
		return plan, nil, nil
	}
	if block != nil {
		if refusal := carryServiceGrants(existing, block, removed); refusal != nil {
			return plan, refusal, nil
		}
	}
	doc, err := gs.ServiceGrantDoc(ctx)
	if err != nil {
		return plan, nil, err
	}
	// Before anything destructive (a credential purge, a secret delete, a claim): a grant written before the
	// switch-on is refused here, under the document lock the door holds. The store's own statement keeps the
	// same condition for a writer that did not come through this planner.
	if !doc.Activated && blockHoldsServiceGrant(block) {
		return plan, &serviceGrantRefusal{http.StatusConflict, reasonServiceGrantsNotActivated, serviceGrantsNotActivatedMsg}, nil
	}
	if refusal := pendingCleanupRefusal(doc, block); refusal != nil {
		return plan, refusal, nil
	}
	drops, inert := droppedServiceGrantUIDs(doc, existing, block)
	plan.change.Drop = drops
	claims, err := s.serviceGrantClaims(ctx, drops, existing, block, why, inert)
	if err != nil {
		return plan, nil, err
	}
	plan.change.Claims = claims
	plan.order = sortedCleanupUIDs(claims)
	return plan, nil, nil
}

// blockHoldsServiceGrant reports whether any row of block names a service grant.
func blockHoldsServiceGrant(block *types.ModelProviders) bool {
	if block == nil {
		return false
	}
	return slices.ContainsFunc(block.Providers, func(p types.ModelProvider) bool { return p.OIDC != nil && p.OIDC.ServiceGrant != nil })
}

// carryServiceGrants keeps a stored grant on every row of block that keeps its uid and its oidc block and
// carries no service_grant object, whether the member was absent or null. The row whose uid is removed is the
// one an explicit removal names: its grant is not carried, so it is dropped and claimed. The carried grant is validated with
// the row as written.
func carryServiceGrants(existing, block *types.ModelProviders, removed string) *serviceGrantRefusal {
	if existing == nil {
		return nil
	}
	held := map[string]*types.ProviderServiceGrant{}
	for _, p := range existing.Providers {
		if p.OIDC != nil && p.OIDC.ServiceGrant != nil {
			held[p.UID] = p.OIDC.ServiceGrant
		}
	}
	for i := range block.Providers {
		p := &block.Providers[i]
		g, ok := held[p.UID]
		if !ok || p.OIDC == nil || p.OIDC.ServiceGrant != nil || p.UID == "" || p.UID == removed {
			continue
		}
		o := *p.OIDC
		carried := *g
		carried.ServiceIdentities = slices.Clone(g.ServiceIdentities)
		o.ServiceGrant = &carried
		p.OIDC = &o
		if err := validateServiceGrant(*p); err != nil {
			return &serviceGrantRefusal{http.StatusBadRequest, reasonSiteConfigInvalid, "invalid model providers: " + err.Error()}
		}
	}
	return nil
}

// pendingCleanupRefusal is the 409 for a row that would hold a grant while a claim names its uid.
func pendingCleanupRefusal(doc store.ServiceGrantDoc, block *types.ModelProviders) *serviceGrantRefusal {
	if block == nil {
		return nil
	}
	for _, p := range block.Providers {
		if _, pending := doc.Cleanups[p.UID]; pending && p.OIDC != nil && p.OIDC.ServiceGrant != nil {
			return &serviceGrantRefusal{http.StatusConflict, reasonServiceGrantCleanupPending, serviceGrantCleanupPendingMsg(p.ID)}
		}
	}
	return nil
}

func serviceGrantCleanupPendingMsg(id string) string {
	return fmt.Sprintf("provider %q still has a removed service grant's secret to delete; wait for the clean-up, or retry it with POST /service-grant-cleanups/{uid}/retry, before giving it a grant again", id)
}

// droppedServiceGrantUIDs are the stored grants no row of block still holds: those of a removed row, of a row
// whose id or kind changed (a new uid), of a row whose oidc block was removed, and every inert entry. An entry
// whose row keeps its oidc block but names no grant after the carry-forward is inert too, and is dropped, so a
// restored oidc block never revives it.
//
// A stored entry whose row, as the previous attached roster shows it, held no grant is inert whatever the new
// body proposes: an older server removed the row's oidc block and left the entry. It is dropped and claimed
// even when the body gives that uid a grant again, so the earlier generation's secret never serves the new one;
// the new grant mints only after a fresh secret is written. inert names those uids.
func droppedServiceGrantUIDs(doc store.ServiceGrantDoc, existing, block *types.ModelProviders) (drops []string, inert map[string]bool) {
	prior := map[string]bool{}
	if existing != nil {
		for _, p := range existing.Providers {
			prior[p.UID] = p.OIDC != nil && p.OIDC.ServiceGrant != nil
		}
	}
	inert = map[string]bool{}
	held := map[string]bool{}
	if block != nil {
		for _, p := range block.Providers {
			if p.OIDC != nil && p.OIDC.ServiceGrant != nil {
				held[p.UID] = true
			}
		}
	}
	for uid := range doc.Grants {
		if !prior[uid] {
			inert[uid] = true
		}
		if !held[uid] || inert[uid] {
			drops = append(drops, uid)
		}
	}
	slices.Sort(drops)
	return drops, inert
}

// serviceGrantClaims reads each dropped record's revision and returns the claims to write: none for a record
// the store reports absent, one with no revision for a store that keeps none.
func (s *Server) serviceGrantClaims(ctx context.Context, drops []string, existing, block *types.ModelProviders, why string, inert map[string]bool) (map[string]store.ServiceGrantCleanup, error) {
	claims := map[string]store.ServiceGrantCleanup{}
	for _, uid := range drops {
		rev, ok, err := s.serviceGrantRevision(ctx, serviceGrantSecretName(uid))
		if err != nil {
			return nil, fmt.Errorf("read the service grant secret's revision: %w", err)
		}
		if ok && rev == "" {
			continue // no record, nothing to delete
		}
		id, reason := removedGrantFacts(uid, existing, block, why)
		if inert[uid] {
			reason = cleanupReasonInert
		}
		claims[uid] = store.ServiceGrantCleanup{ProviderID: id, Reason: reason, Revision: rev, Since: s.cfg.Now().UTC()}
	}
	return claims, nil
}

// removedGrantFacts names the provider a grant had and why it went.
func removedGrantFacts(uid string, existing, block *types.ModelProviders, why string) (id, reason string) {
	if existing != nil {
		for _, p := range existing.Providers {
			if p.UID == uid {
				id = p.ID
			}
		}
	}
	if why != "" {
		return id, why
	}
	if block != nil {
		for _, p := range block.Providers {
			if p.UID == uid {
				return id, cleanupReasonOIDCRemoved
			}
		}
	}
	if id == "" {
		return id, cleanupReasonInert
	}
	return id, cleanupReasonProviderRemoved
}

// putSiteConfigGrants is the doors' save: through the grant-aware store when it has one.
func (s *Server) putSiteConfigGrants(ctx context.Context, cfg types.SiteConfig, change store.ServiceGrantChange) (types.SiteConfig, error) {
	if gs, ok := s.serviceGrantStore(); ok {
		return gs.PutSiteConfigGrants(ctx, cfg, change)
	}
	return s.cfg.Store.PutSiteConfig(ctx, cfg)
}

// writeSiteConfigSaveError answers a failed save and reports whether it did: 409 service_grants_not_activated
// for a grant written before the switch-on, 500 for any other failure. A nil error writes nothing.
func writeSiteConfigSaveError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrServiceGrantsNotActivated):
		writeErrorReason(w, http.StatusConflict, reasonServiceGrantsNotActivated, serviceGrantsNotActivatedMsg)
	default:
		writeServerError(w, r, "put site config", err)
	}
	return true
}

// finishServiceGrantWrite runs after the save, still under the document lock: it deletes each claimed record
// with the revision read before the save and settles the claim. It returns the warnings for the answer and
// "done", "failed" or "" for the audit row's secret_cleanup. A record that could not be deleted keeps its claim
// for the clean-up worker; a store that keeps no revision gets no delete at all.
func (s *Server) finishServiceGrantWrite(ctx context.Context, plan serviceGrantPlan, warnings []string) ([]string, string) {
	gs, ok := s.serviceGrantStore()
	if !ok || len(plan.order) == 0 {
		return warnings, ""
	}
	cleanup := "done"
	for _, uid := range plan.order {
		claim := plan.change.Claims[uid]
		if s.svcGrant.stop("after_commit") {
			return warnings, ""
		}
		if err := s.serviceGrantInlineDelete(ctx, gs, uid, claim); err != nil {
			cleanup = "failed"
			warnings = append(warnings, fmt.Sprintf("%s: the secret of provider %q (uid %s) was not deleted; it is listed and Wardyn retries it",
				reasonServiceGrantSecretKept, claim.ProviderID, uid))
			slog.WarnContext(ctx, "wardynd: a removed service grant's secret was not deleted", slog.String("uid", uid), slog.String("provider", claim.ProviderID))
		}
	}
	return warnings, cleanup
}

// serviceGrantInlineDelete is the door's one attempt, under the lock the door already holds.
func (s *Server) serviceGrantInlineDelete(ctx context.Context, gs store.ServiceGrantStore, uid string, claim store.ServiceGrantCleanup) error {
	if claim.Revision == "" {
		return errors.New("the secret store keeps no revision")
	}
	dctx, cancel := context.WithTimeout(ctx, serviceGrantCleanupAttemptBound)
	defer cancel()
	rev := claim.Revision
	if err := s.deleteServiceGrantRecord(dctx, serviceGrantSecretName(uid), &rev); err != nil {
		return err
	}
	removed, err := gs.SettleServiceGrantCleanup(ctx, uid)
	if err != nil {
		return err
	}
	if removed {
		s.auditServiceGrantSecretDelete(ctx, uid, claim.ProviderID, true)
	}
	return nil
}

// auditServiceGrantSecretDelete writes model_provider.service_grant.secret_delete.
func (s *Server) auditServiceGrantSecretDelete(ctx context.Context, uid, providerID string, cleanup bool) {
	data := map[string]any{"provider": providerID, "provider_uid": uid}
	if cleanup {
		data["cleanup"] = true
	}
	s.recordAudit(context.WithoutCancel(ctx), s.auditEvent(nil, types.ActorSystem, serviceGrantActor,
		"model_provider.service_grant.secret_delete", providerID, "success", mustJSON(data)))
}

// serviceGrantCleanupAttemptBound bounds each step of one clean-up attempt: the first read under the lock,
// the guarded delete, and the settling read. An attempt's three steps run one after another, so one attempt is
// bounded by three of these, and the secret store's own write bound (about 30 seconds) is longer than the
// delete's: a delete that the external store is still finishing when its context ends is found absent by the
// next attempt's first read, which settles the claim.
const serviceGrantCleanupAttemptBound = 10 * time.Second

// auditServiceGrantCleanup puts a write's secret clean-up on its audit datum: "done" when every claimed record
// was deleted in line, "failed" when one was left to the clean-up worker.
func auditServiceGrantCleanup(datum map[string]any, cleanup string) {
	if cleanup != "" {
		datum["secret_cleanup"] = cleanup
	}
}
