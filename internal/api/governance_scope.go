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

// errOutsideScope is a scope refusal decided inside a store transaction, carried out of it.
var errOutsideScope = errors.New("api: the target is outside the caller's units")

// grantsCover is unitScope decided over a graph a write transaction read under the graph lock: whether
// one of grants is on unit or a unit above it (strict: above it only). The pre-transaction check reads
// the same rule through the store; this re-check closes the window in which a concurrent re-parent
// could move the target out of the caller's subtree.
func grantsCover(all []types.GovernanceUnit, grants []types.GovernanceUnitAdmin, unit uuid.UUID, strict bool) bool {
	byID := unitsByID(all)
	cur := &unit
	if strict {
		cur = byID[unit].ParentID
	}
	for n := 0; cur != nil && n < store.MaxUnitDepthCap; n++ {
		if slices.ContainsFunc(grants, func(g types.GovernanceUnitAdmin) bool { return g.UnitID == *cur }) {
			return true
		}
		row, ok := byID[*cur]
		if !ok {
			return false
		}
		cur = row.ParentID
	}
	return false
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
	}
}
