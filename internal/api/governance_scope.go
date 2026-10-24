// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Unit admins (DESIGN-TREE §5.1): a person or an IdP group granted on a unit governs that unit and
// every unit below it, and nothing else. This file is the one predicate every unit-admin route asks,
// unitScope, and the middleware that lets a unit admin reach those routes at all. A security caller
// passes both untouched, so a security admin is never further restricted.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unitAdminSubjects is who the caller is for a unit-admin grant: their sub and email as user subjects
// (lowercased, as the store's subjectMatch reads them) and their groups. A grant gives governance
// reach, so the trust rules of the role mappings apply, failing toward less reach:
//   - an email sign-in does not vouch for is no subject (unverifiedEmailSubject);
//   - a stale or truncated group snapshot contributes no group, so a group-granted admin keeps their
//     member reach only and is never refused outright for it;
//   - a caller no admin tier may answer for (a device, a service identity, a delegated portal) and a
//     caller with no person (the admin token, local mode) hold no grant.
func (s *Server) unitAdminSubjects(ctx context.Context) (users, groups []string) {
	if neverOperator(ctx) || oidcHumanFromContext(ctx) == "" {
		return nil, nil
	}
	users, groups, stale := capabilitySubjects(ctx)
	if email := s.unverifiedEmailSubject(ctx); email != "" {
		users = slices.DeleteFunc(slices.Clone(users), func(u string) bool { return u == email })
	}
	if stale {
		groups = nil
	}
	return users, groups
}

// unitScopeTxKey carries a transaction the unit-scope reads must use (withUnitScopeTx).
type unitScopeTxKey struct{}

// withUnitScopeTx is ctx whose unitScope reads run on q: an approver re-checked inside the decision's
// transaction (recheckApprover, for a session and an API token alike) reads its scope there, not on a
// second connection and not from the request's memo.
func withUnitScopeTx(ctx context.Context, q store.Querier) context.Context {
	return context.WithValue(ctx, unitScopeTxKey{}, q)
}

// unitScopeMemoKey carries the per-request unit-scope memo (requireUnitScope installs it).
type unitScopeMemoKey struct{}

// unitScopeMemo is one request's scope answers, keyed on the unit, strictness and the identity asked
// about (an approver's identity is re-read inside a decision, so one request can ask for two).
type unitScopeMemo struct {
	mu     sync.Mutex
	in     map[unitScopeKey]bool
	grants map[string][]types.GovernanceUnitAdmin
}

type unitScopeKey struct {
	unit   uuid.UUID
	strict bool
	who    string
}

func scopeIdentity(users, groups []string) string {
	return strings.Join(users, "\x1f") + "\x1e" + strings.Join(groups, "\x1f")
}

// unitAdminStore is the store's grant table, or nil for a store that keeps none (no caller is then a
// unit admin).
func (s *Server) unitAdminStore() store.UnitAdminStore {
	st, _ := s.cfg.Store.(store.UnitAdminStore)
	return st
}

// unitScope reports whether the caller governs unit: a security caller always does; anyone else when
// they hold a grant on unit or a unit above it (strict: above it only). It reads one recursive
// statement per (unit, strict, identity), memoized for the request.
func (s *Server) unitScope(ctx context.Context, unit uuid.UUID, strict bool) (bool, error) {
	if q, ok := ctx.Value(unitScopeTxKey{}).(store.Querier); ok {
		return s.unitScopeQ(ctx, q, unit, strict)
	}
	if s.isSecurityOperator(ctx) {
		return true, nil
	}
	users, groups := s.unitAdminSubjects(ctx)
	st := s.unitAdminStore()
	if st == nil || len(users)+len(groups) == 0 {
		return false, nil
	}
	key := unitScopeKey{unit: unit, strict: strict, who: scopeIdentity(users, groups)}
	memo, _ := ctx.Value(unitScopeMemoKey{}).(*unitScopeMemo)
	if memo != nil {
		memo.mu.Lock()
		defer memo.mu.Unlock()
		if in, ok := memo.in[key]; ok {
			return in, nil
		}
	}
	in, err := st.UnitScope(ctx, unit, users, groups, strict)
	if err != nil {
		return false, err
	}
	if memo != nil {
		memo.in[key] = in
	}
	return in, nil
}

// unitScopeEvery is unitScope over every unit of units: false as soon as one is outside.
func (s *Server) unitScopeEvery(ctx context.Context, strict bool, units ...uuid.UUID) (bool, error) {
	for _, u := range units {
		in, err := s.unitScope(ctx, u, strict)
		if err != nil || !in {
			return false, err
		}
	}
	return true, nil
}

// callerUnitGrants is every grant naming the caller (none for a security caller's own reach, which
// needs none), memoized for the request.
func (s *Server) callerUnitGrants(ctx context.Context) ([]types.GovernanceUnitAdmin, error) {
	users, groups := s.unitAdminSubjects(ctx)
	st := s.unitAdminStore()
	if st == nil || len(users)+len(groups) == 0 {
		return nil, nil
	}
	who := scopeIdentity(users, groups)
	memo, _ := ctx.Value(unitScopeMemoKey{}).(*unitScopeMemo)
	if memo != nil {
		memo.mu.Lock()
		defer memo.mu.Unlock()
		if g, ok := memo.grants[who]; ok {
			return g, nil
		}
	}
	g, err := st.ListUnitAdminOf(ctx, users, groups)
	if err != nil {
		return nil, err
	}
	if memo != nil {
		memo.grants[who] = g
	}
	return g, nil
}

// unitTreeRows is the stored unit rows, read on one transaction, for the readers that only name units,
// walk parent links or compare what units state (the scope display, the approvers list, the boundary's
// lock conflicts). It is never authority: a composed row is turned into walls by governance_compose.go.
func (s *Server) unitTreeRows(ctx context.Context) (rows []types.GovernanceUnit, err error) {
	err = s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) error {
		rows, err = store.ListGovernanceUnitsQ(ctx, q)
		return err
	})
	return rows, err
}

// isUnitAdminCaller reports a caller on a unit-admin route who is not a security caller: they reached
// it through their grants, so the unit-admin rules (always-held widenings, the scope checks) apply.
func (s *Server) isUnitAdminCaller(ctx context.Context) bool { return !s.isSecurityOperator(ctx) }

// requireUnitScope gates the unit-admin routes: a security caller passes, a caller holding any grant
// passes (each route then checks its own target), and anyone else is refused. The refusal's sentence
// differs from the per-route one, so a reader of the audit log can tell "not a unit admin" from "not
// this unit".
func (s *Server) requireUnitScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), unitScopeMemoKey{}, &unitScopeMemo{
			in: map[unitScopeKey]bool{}, grants: map[string][]types.GovernanceUnitAdmin{},
		})
		r = r.WithContext(ctx)
		if s.isSecurityOperator(ctx) {
			next.ServeHTTP(w, r)
			return
		}
		grants, err := s.callerUnitGrants(ctx)
		if err != nil {
			writeServerError(w, r, "read unit admin grants", err)
			return
		}
		if len(grants) == 0 {
			s.refuse(w, r, authz.Deny(authz.ReasonUnitScopeDenied, "governance", "this needs a unit admin or a security admin"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refuseOutsideScope is the per-route refusal of a target outside the caller's subtree, in the
// registry's sentence (the one a change decision outside the approver's units answers with too).
func (s *Server) refuseOutsideScope(w http.ResponseWriter, r *http.Request, target string) {
	s.refuse(w, r, authz.Deny(authz.ReasonUnitScopeDenied, target, ""))
}

// requireUnitsInScope answers for a handler whether the caller governs every unit of units (strict:
// strictly above each), writing the refusal or the error and reporting false when not.
func (s *Server) requireUnitsInScope(w http.ResponseWriter, r *http.Request, target string, strict bool, units ...uuid.UUID) bool {
	in, err := s.unitScopeEvery(r.Context(), strict, units...)
	if err != nil {
		writeServerError(w, r, "read unit scope", err)
		return false
	}
	if !in {
		s.refuseOutsideScope(w, r, target)
	}
	return in
}

// errOutsideScope is a scope refusal decided inside a store transaction, carried out of it; the
// handler answers it with refuseOutsideScope.
var errOutsideScope = errors.New("api: the target is outside the caller's units")

// unitScopeQ is unitScope read on a write's own transaction q, after the write took the graph lock,
// so a concurrent re-parent cannot move the target out of the caller's subtree between the check and
// the write. Not memoized: each answer is that transaction's.
func (s *Server) unitScopeQ(ctx context.Context, q store.Querier, unit uuid.UUID, strict bool) (bool, error) {
	if s.isSecurityOperator(ctx) {
		return true, nil
	}
	users, groups := s.unitAdminSubjects(ctx)
	if s.unitAdminStore() == nil || len(users)+len(groups) == 0 {
		return false, nil
	}
	return store.UnitScopeQ(ctx, q, unit, users, groups, strict)
}

// requireUnitScopeQ is unitScopeQ over every unit of units as a write's refusal: errOutsideScope
// unless the caller governs each.
func (s *Server) requireUnitScopeQ(ctx context.Context, q store.Querier, strict bool, units ...uuid.UUID) error {
	for _, u := range units {
		in, err := s.unitScopeQ(ctx, q, u, strict)
		if err != nil {
			return err
		}
		if !in {
			return errOutsideScope
		}
	}
	return nil
}

// answerOutsideScope writes the scope refusal for an error carrying errOutsideScope and reports true;
// false for any other error.
func (s *Server) answerOutsideScope(w http.ResponseWriter, r *http.Request, target string, err error) bool {
	if !errors.Is(err, errOutsideScope) {
		return false
	}
	s.refuseOutsideScope(w, r, target)
	return true
}

// unitScopeApprover is the approver of a held change that touches units: a security caller, or a
// caller whose scope covers every one of them (strict for a grant). The proposer is excluded by the
// decision's distinct-human rule, never here. A scope that cannot be read fails closed.
func unitScopeApprover(strict bool, units ...uuid.UUID) govApprover {
	return govApprover{
		allowed: func(s *Server, r *http.Request) bool {
			in, err := s.unitScopeEvery(r.Context(), strict, units...)
			if err != nil {
				slog.ErrorContext(r.Context(), "api: read the unit scope of a governance change approver", "err", err)
			}
			return err == nil && in
		},
		reason: authz.ReasonUnitScopeDenied,
		scoped: true,
	}
}

// ── the unit-admin arms of the unit and membership writes ────────────────────

// unitWritePrecheck is a unit admin's scope over a unit write before it is decoded into a build: a PUT
// names a unit they govern, and a POST names a parent they govern. writeUnitInScope re-checks both on
// the write's own transaction.
func (s *Server) unitWritePrecheck(w http.ResponseWriter, r *http.Request, id uuid.UUID, status int, req governanceUnitRequest) bool {
	if status == http.StatusOK {
		return s.requireUnitsInScope(w, r, "governance.unit", false, id)
	}
	if req.comp.base == nil {
		// No parent, or the Default unit's: a top-level unit is a security admin's to create.
		s.refuseOutsideScope(w, r, "governance.unit")
		return false
	}
	return s.requireUnitsInScope(w, r, "governance.unit", false, *req.comp.base)
}

// writeUnitInScope is store.WriteGovernanceUnit for a unit admin: the same graph-locked write, on a
// transaction whose build re-checks their scope (scopedUnitBuild).
func (s *Server) writeUnitInScope(ctx context.Context, id uuid.UUID, build store.GovernanceUnitBuild) (types.GovernanceUnit, error) {
	var out types.GovernanceUnit
	err := s.writeGovernance(ctx, func(q store.Querier) (err error) {
		out, err = store.WriteGovernanceUnitQ(ctx, q, id, s.scopedUnitBuild(ctx, q, id, build))
		return err
	})
	return out, err
}

// scopedUnitBuild is build with a unit admin's scope re-checked on the write's transaction q, under
// the graph lock: the unit when it exists, before build runs, and once build has decided the row, its
// parent when the row is new or re-parented. A unit with no parent is never in a unit admin's scope.
// A security caller's build is returned unchanged.
func (s *Server) scopedUnitBuild(ctx context.Context, q store.Querier, id uuid.UUID, build store.GovernanceUnitBuild) store.GovernanceUnitBuild {
	if !s.isUnitAdminCaller(ctx) {
		return build
	}
	return func(all []types.GovernanceUnit, ms []types.GovernanceMembership) (types.GovernanceUnit, error) {
		cur, exists := unitsByID(all)[id]
		if exists {
			if err := s.requireUnitScopeQ(ctx, q, false, id); err != nil {
				return types.GovernanceUnit{}, err
			}
		}
		p, err := build(all, ms)
		if err != nil && !errors.Is(err, errHoldForApproval) {
			return p, err
		}
		if !exists || !sameJSON(cur.ParentID, p.ParentID) {
			if p.ParentID == nil {
				return p, errOutsideScope
			}
			if serr := s.requireUnitScopeQ(ctx, q, false, *p.ParentID); serr != nil {
				return p, serr
			}
		}
		return p, err
	}
}

// deleteUnitInScope is a unit admin's unit delete: a unit strictly below one they govern, re-checked
// under the graph lock the delete takes, and applied directly, since the store refuses a unit that
// still has members or units below it and an empty unit binds nobody. Its grants go with it.
func (s *Server) deleteUnitInScope(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ctx := r.Context()
	if !s.requireUnitsInScope(w, r, "governance.unit", true, id) {
		return
	}
	var name string
	scope := s.auditUnitScope(ctx, id, true) // read while the unit is there to walk from
	err := s.writeGovernance(ctx, func(q store.Querier) error {
		if err := store.LockGovernanceGraph(ctx, q); err != nil {
			return err
		}
		if err := s.requireUnitScopeQ(ctx, q, true, id); err != nil {
			return err
		}
		row, err := store.GetGovernanceUnitQ(ctx, q, id, true)
		if err != nil {
			return err
		}
		name = row.Name
		return store.DeleteGovernanceUnitQ(ctx, q, id)
	})
	if s.answerOutsideScope(w, r, "governance.unit", err) || s.writeUnitDeleteError(w, r, err) {
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.unit.delete", id.String(), "success", mustJSON(unitScopeAudit(map[string]any{"name": name}, r, scope))))
	w.WriteHeader(http.StatusNoContent)
}

// subjectBelongsInScope reports whether the subject (type and value) holds a membership, other than
// except, in a unit the caller governs: "people who already belong" (§5.1). A group subject belongs by
// a group row of the same group.
func (s *Server) subjectBelongsInScope(ctx context.Context, ms []types.GovernanceMembership, st types.CapabilitySubjectType,
	subject string, except uuid.UUID, scope func(uuid.UUID) (bool, error)) (bool, error) {
	for _, m := range ms {
		if m.ID == except || m.SubjectType != st || m.Subject != subject {
			continue
		}
		in, err := scope(m.UnitID)
		if err != nil || in {
			return in, err
		}
	}
	return false, nil
}

// membershipAddInScope is a unit admin's scope over a membership add: the unit is one they govern and
// the subject already belongs in their units. The add itself is always held (unitWriteMode).
func (s *Server) membershipAddInScope(w http.ResponseWriter, r *http.Request, a types.GovernanceMembership) bool {
	ctx := r.Context()
	if !s.requireUnitsInScope(w, r, "governance.membership", false, a.UnitID) {
		return false
	}
	ms, err := s.cfg.Store.ListGovernanceMemberships(ctx)
	var belongs bool
	if err == nil {
		belongs, err = s.subjectBelongsInScope(ctx, ms, a.SubjectType, a.Subject, uuid.Nil,
			func(u uuid.UUID) (bool, error) { return s.unitScope(ctx, u, false) })
	}
	if err != nil {
		writeServerError(w, r, "read unit scope", err)
		return false
	}
	if !belongs {
		s.refuseOutsideScope(w, r, "governance.membership")
	}
	return belongs
}

// deleteMembershipInScope is a unit admin's membership delete: the membership is in a unit they govern
// and its subject keeps another membership in their units, both read under the locks the delete takes.
// It applies directly (the person keeps every other context, so nothing they could reach widens),
// except a legacy row whose delete would let another legacy row take over (H3), which is held.
func (s *Server) deleteMembershipInScope(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ctx := r.Context()
	var removed types.GovernanceMembership
	err := s.writeGovernance(ctx, func(q store.Querier) error {
		row, err := lockedMembershipQ(ctx, q, id)
		if err != nil {
			return err
		}
		if err := s.requireUnitScopeQ(ctx, q, false, row.UnitID); err != nil {
			return err
		}
		all, err := store.ListGovernanceMembershipsQ(ctx, q)
		if err != nil {
			return err
		}
		keeps, err := s.subjectBelongsInScope(ctx, all, row.SubjectType, row.Subject, row.ID,
			func(u uuid.UUID) (bool, error) { return s.unitScopeQ(ctx, q, u, false) })
		if err != nil {
			return err
		}
		if !keeps {
			return errOutsideScope
		}
		if row.Legacy {
			units, err := store.ListGovernanceUnitsQ(ctx, q)
			if err != nil {
				return err
			}
			if len(legacyActivations(row, all, units)) > 0 {
				return errHoldForApproval
			}
		}
		removed = row
		return store.DeleteGovernanceMembershipQ(ctx, q, id)
	})
	if errors.Is(err, errHoldForApproval) {
		s.holdMembershipDelete(w, r, id)
		return
	}
	if s.answerOutsideScope(w, r, "governance.membership", err) || writeMembershipTreeError(w, r, "delete governance assignment", err) {
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.membership.delete", id.String(), "success",
		mustJSON(unitScopeAudit(membershipWriteAuditData(removed), r, s.auditUnitScope(ctx, removed.UnitID, false)))))
	w.WriteHeader(http.StatusNoContent)
}

// moveMembershipInScope is a unit admin's membership move: from a unit they govern to another, read
// under the locks the move takes. A move whose destination's walls are Leq the source's applies
// directly, except a legacy row whose move would let another legacy row take over (H3); any other is
// held, whatever the second-human switch says.
func (s *Server) moveMembershipInScope(w http.ResponseWriter, r *http.Request, id, to uuid.UUID) {
	ctx := r.Context()
	var from, moved types.GovernanceMembership
	err := s.writeGovernance(ctx, func(q store.Querier) error {
		row, err := lockedMembershipQ(ctx, q, id)
		if err != nil {
			return err
		}
		if err := s.requireUnitScopeQ(ctx, q, false, row.UnitID, to); err != nil {
			return err
		}
		narrows, err := s.pathNarrowsQ(ctx, q, row.UnitID, to)
		if err != nil {
			return err
		}
		if !narrows {
			return errHoldForApproval
		}
		activates, err := moveActivationsQ(ctx, q, row, to)
		if err != nil {
			return err
		}
		if len(activates) > 0 {
			return errHoldForApproval
		}
		from, moved, err = moveMembershipQ(ctx, q, id, to)
		return err
	})
	if errors.Is(err, errHoldForApproval) {
		s.holdMembershipMove(w, r, membershipTreePayload{ID: id, UnitID: to})
		return
	}
	if s.answerOutsideScope(w, r, "governance.membership", err) || writeMembershipTreeError(w, r, "move governance assignment", err) {
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.membership.write", moved.ID.String(), "success",
		mustJSON(unitScopeAudit(membershipMoveAuditData(from, moved), r, s.auditUnitScope(ctx, from.UnitID, false)))))
	writeJSON(w, http.StatusOK, moved)
}

// pathNarrowsQ reports whether unit to's walls are Leq unit from's, each its path composed on q, with
// no higher member size: the proof a move needs to apply without a second human. A path that does not
// compose proves nothing.
func (s *Server) pathNarrowsQ(ctx context.Context, q store.Querier, from, to uuid.UUID) (bool, error) {
	compose := func(id uuid.UUID) (*ResolvedUnit, error) {
		chain, err := unitChainQ(ctx, q, id, false)
		if err != nil || len(chain) == 0 {
			return nil, err
		}
		res, err := s.composeChain(chain)
		if err != nil {
			return nil, nil //nolint:nilerr // a path that does not compose is no proof, not a failure
		}
		return res, nil
	}
	src, err := compose(from)
	if err != nil || src == nil {
		return false, err
	}
	dst, err := compose(to)
	if err != nil || dst == nil {
		return false, err
	}
	return composer.Leq(dst.Ceiling, src.Ceiling, dst.Limits, src.Limits) &&
		len(memberCapRaises(s.resolvedMemberCap(dst), s.resolvedMemberCap(src))) == 0, nil
}

// unitScopeAudit is a unit admin's direct write row: data plus the actor, the scope they wrote in
// (auditUnitScope) and the approver, "" because no second human approved it.
func unitScopeAudit(data map[string]any, r *http.Request, scope string) map[string]any {
	data["actor"], data["unit_scope"], data["approver"] = principalFromRequest(r), scope, ""
	return data
}
