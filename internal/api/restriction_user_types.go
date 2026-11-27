// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Restrictions over every user type a person's sign-in matches (#4073). A session is stamped with one
// type, the highest-priority one its sign-in matched, and the restrictions read for it name that type
// only: a restriction keyed on another type the same sign-in matches is missed. Under
// WARDYN_IDENTITY_NARROWING=enforce those restrictions apply too, as group restrictions combine; under
// audit only the stamped type's apply, and the ones enforce would add are recorded in
// governance.restriction.alert. The matched types are derived again at resolve time, with the same
// derivation sign-in runs, so nothing is added to the session.

// applyIdentityRestrictions is applyRestrictions with the restrictions on the caller's other matched user
// types (userTypeRestrictions) added to the list.
func (s *Server) applyIdentityRestrictions(ctx context.Context, q contextQuery, c governanceCeiling) (governanceCeiling, error) {
	list, err := s.restrictionsFor(ctx, q)
	if err != nil {
		return c, err
	}
	list, err = s.userTypeRestrictions(ctx, q, list)
	if err != nil {
		return c, err
	}
	if len(list) == 0 {
		return c, nil
	}
	return s.withRestrictions(ctx, c, list)
}

// reviveIdentityRestrictions is reviveRestrictions with the same union when the revive is the owner's own,
// from a session whose groups are known: the owner's sign-in is then derived again. Anyone else's revive
// keeps the captured ids, which hold what the launch applied.
func (s *Server) reviveIdentityRestrictions(ctx context.Context, run types.AgentRun) ([]types.Restriction, error) {
	list, err := s.reviveRestrictions(ctx, run)
	if err != nil {
		return nil, err
	}
	sub := oidcHumanFromContext(ctx)
	if sub == "" || sub != run.CreatedBy || oidcGroupsTruncatedFromContext(ctx) {
		return list, nil
	}
	subj, err := s.callerSubjects(ctx)
	if err != nil || subj.stale {
		return list, nil
	}
	return s.userTypeRestrictions(ctx, contextQuery{users: subj.users, groups: subj.groups,
		userType: subj.userType, email: oidcEmailFromContext(ctx)}, list)
}

// userTypeRestrictions adds to have the restrictions keyed on a user type q's sign-in matches other than
// its stamped one, under enforce; under audit it returns have and records what enforce would add. A
// deployment without single sign-on, a store without restrictions, or a query that is not the caller's
// own (no email) returns have. With no restriction naming a type other than the stamped one, nothing is
// derived. Groups that cannot be evaluated hide the types they would match: enforce refuses, as
// restrictionsFor does for a group-keyed restriction, and audit records the alert as unresolvable.
func (s *Server) userTypeRestrictions(ctx context.Context, q contextQuery, have []types.Restriction) ([]types.Restriction, error) {
	rs, ok := s.restrictionStore()
	if !ok || s.cfg.OIDC == nil || q.email == "" {
		return have, nil
	}
	enforce := identityNarrowingEnforced()
	named, err := rs.RestrictionUserTypes(ctx)
	if err != nil {
		return s.userTypesUnreadable(ctx, q, have, enforce, err)
	}
	others := slices.DeleteFunc(named, func(t string) bool { return t == q.userType })
	if len(others) == 0 {
		return have, nil
	}
	if q.unusable {
		if enforce {
			// Decided and recorded here, as restrictionsFor does: a display read writes no row.
			if !isDisplayRead(ctx) {
				s.recordRefusal(ctx, nil, authz.Deny(authz.ReasonGroupsSnapshotStale, "governance.ceiling", ""))
			}
			return nil, errGroupsSnapshotStale
		}
		s.recordUserTypeRestrictionAlert(ctx, q.userType, nil, nil, true)
		return have, nil
	}
	d, err := s.cfg.OIDC.PreviewRole(ctx, nil, q.groups, q.email)
	if err != nil {
		return s.userTypesUnreadable(ctx, q, have, enforce, err)
	}
	var matched []string
	for _, m := range d.Matches {
		if m.UserType != "" && slices.Contains(others, m.UserType) && !slices.Contains(matched, m.UserType) {
			matched = append(matched, m.UserType)
		}
	}
	slices.Sort(matched)
	var extra []types.Restriction
	for _, t := range matched {
		list, err := rs.ListRestrictionsFor(ctx, nil, nil, t)
		if err != nil {
			return s.userTypesUnreadable(ctx, q, have, enforce, err)
		}
		for _, r := range list {
			if !slices.ContainsFunc(have, func(x types.Restriction) bool { return x.ID == r.ID }) {
				extra = mergeRestrictions(extra, []types.Restriction{r})
			}
		}
	}
	if len(extra) == 0 {
		return have, nil
	}
	if enforce {
		return mergeRestrictions(have, extra), nil
	}
	s.recordUserTypeRestrictionAlert(ctx, q.userType, matched, extra, false)
	return have, nil
}

// userTypesUnreadable is a failed read of the user types q's sign-in matches or of their restrictions:
// enforce returns err, so the ceiling fails as any store error does; audit logs it, records the alert as
// unresolvable and returns have.
func (s *Server) userTypesUnreadable(ctx context.Context, q contextQuery, have []types.Restriction, enforce bool, err error) ([]types.Restriction, error) {
	if enforce {
		return nil, err
	}
	slog.WarnContext(ctx, "api: could not read the user types a sign-in matches; only the stamped type's restrictions apply", slog.Any("err", err))
	s.recordUserTypeRestrictionAlert(ctx, q.userType, nil, nil, true)
	return have, nil
}

// userTypeAlertSeen is the alerts written today: one row per server, person and set of restrictions (or
// unresolvable) a UTC day, so an affected person's every request does not write one. It is in memory, so a
// restart or a second replica can write one more; the row is evidence, not a decision.
var userTypeAlertSeen struct {
	sync.Mutex
	day  string
	keys map[userTypeAlertKey]bool
}

type userTypeAlertKey struct {
	srv      *Server
	sub, set string
}

// recordUserTypeRestrictionAlert writes governance.restriction.alert under audit: the caller's sign-in
// matches user types (matched) whose restrictions (extra) enforce would add beside their stamped type's,
// or those types could not be known (unresolvable). A display read writes none.
func (s *Server) recordUserTypeRestrictionAlert(ctx context.Context, stamped string, matched []string, extra []types.Restriction, unresolvable bool) {
	if s.cfg.Audit == nil || isDisplayRead(ctx) {
		return
	}
	sub := oidcHumanFromContext(ctx)
	ids := make([]string, 0, len(extra))
	rows := make([]map[string]any, 0, len(extra))
	for _, r := range extra {
		ids = append(ids, r.ID.String())
		rows = append(rows, map[string]any{"id": r.ID, "name": r.Name})
	}
	slices.Sort(ids)
	set := strings.Join(ids, ",")
	if unresolvable {
		set = "unresolvable"
	}
	now := time.Now
	if s.cfg.Now != nil {
		now = s.cfg.Now
	}
	day := now().UTC().Format(time.DateOnly)
	key := userTypeAlertKey{srv: s, sub: sub, set: set}
	userTypeAlertSeen.Lock()
	if userTypeAlertSeen.day != day {
		userTypeAlertSeen.day, userTypeAlertSeen.keys = day, map[userTypeAlertKey]bool{}
	}
	seen := userTypeAlertSeen.keys[key]
	userTypeAlertSeen.keys[key] = true
	userTypeAlertSeen.Unlock()
	if seen {
		return
	}
	if matched == nil {
		matched = []string{}
	}
	data := map[string]any{"user_type": stamped, "user_types": matched, "restrictions": rows, "mode": identityNarrowingMode()}
	if unresolvable {
		data["unresolvable"] = true
	}
	actor := types.ActorHuman
	if _, _, isService := serviceIdentityFromContext(ctx); isService {
		actor = serviceActorType // as recordRefusal: a service's published human is its own principal
	}
	s.recordAudit(ctx, s.auditEvent(nil, actor, sub, "governance.restriction.alert", "governance.ceiling", "success", mustJSON(data)))
}
