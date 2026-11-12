// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The elevation request kind: a person asks to be a member of a unit for a while, a decider the unit's
// elevation rule names approves, and the approval writes a user membership with an end. The kind's
// create, apply and view; the routes are governance_requests.go's, the sweep governance_elevation_sweep.go's.
package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// elevationPayload is an elevation change's stored payload.
type elevationPayload struct {
	UnitID   uuid.UUID `json:"unit_id"`
	Subject  string    `json:"subject"`
	Duration string    `json:"duration"` // types.CanonicalDuration
	// Groups are the rule's eligible_requesters entries the requester matched when asking, in the rule's order.
	Groups []string `json:"groups,omitempty"`
}

// elevationAfter is the diff's after side.
type elevationAfter struct {
	UnitID   uuid.UUID `json:"unit_id"`
	UnitName string    `json:"unit_name"`
	Subject  string    `json:"subject"`
	Duration string    `json:"duration"`
}

// elevationRefusal is a create's answer short of the propose.
type elevationRefusal struct {
	status      int
	reason, msg string
	retryAfter  *time.Time
}

func (e *elevationRefusal) write(w http.ResponseWriter) {
	if e.retryAfter != nil {
		writeJSON(w, e.status, struct {
			errorBody
			RetryAfter time.Time `json:"retry_after"`
		}{errorBody{Error: e.msg, Reason: e.reason}, *e.retryAfter})
		return
	}
	writeErrorReason(w, e.status, e.reason, e.msg)
}

// notOffered is the one answer for a unit that offers the caller no elevation, whatever the cause, so the
// answer tells nothing about the rule.
func notOffered(name string) *elevationRefusal {
	return &elevationRefusal{status: http.StatusBadRequest, reason: reasonElevationNotOffered, msg: name + " offers no elevation"}
}

// proposeElevation is the elevation kind's create (C4 steps 4.1-4.7).
func proposeElevation(s *Server, w http.ResponseWriter, r *http.Request, body governanceRequestBody, who requestCaller) {
	ctx := r.Context()
	if body.Image != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "image is not a field of an elevation request")
		return
	}
	unit, rule, groups, ref, err := s.elevationUnitAndRule(ctx, body.UnitID)
	if err == nil && ref == nil {
		ref = elevationDuration(unit, *rule, body.Duration)
	}
	var note types.ChangeNote
	if err == nil && ref == nil {
		note, ref = elevationNote(body.Reason, body.Ticket)
	}
	if err == nil && ref == nil {
		ref, err = s.elevationTicketRefusal(ctx, nil, unit, note.Ticket)
	}
	key := membershipKey(unit.ID, types.CapabilitySubjectUser, types.CanonicalUserSubject(who.principal))
	if err == nil && ref == nil {
		ref, err = s.elevationMemberOrCooldown(ctx, unit, *rule, key)
	}
	if err != nil {
		if errors.Is(err, errDecidersRead) {
			writeErrorReason(w, http.StatusServiceUnavailable, reasonDecidersUnreadable, "the deciders rules could not be read; try again")
			return
		}
		writeServerError(w, r, "request an elevation", err)
		return
	}
	if ref != nil {
		ref.write(w)
		return
	}
	dur, _ := time.ParseDuration(body.Duration)
	payload := elevationPayload{UnitID: unit.ID, Subject: types.CanonicalUserSubject(who.principal), Duration: types.CanonicalDuration(dur), Groups: groups}
	s.proposeGovernanceChange(w, r, govProposal{
		kind: types.GovernanceTargetElevation, op: "create", key: key, payload: payload,
		after:   elevationAfter{UnitID: unit.ID, UnitName: unit.Name, Subject: payload.Subject, Duration: payload.Duration},
		changed: []string{"duration", "subject", "unit_id"},
		note:    &note,
		answer: func(w http.ResponseWriter, saved types.GovernanceChange) {
			data := map[string]any{"unit_id": unit.ID, "units": []string{unit.ID.String()}, "duration": payload.Duration,
				"reason": note.Reason, "change_id": saved.ID, "expires_at": saved.ExpiresAt}
			if note.Ticket != "" {
				data["ticket"] = note.Ticket
			}
			actorType, actor := actorFromRequest(r)
			s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "governance.elevation.request", saved.ID.String(), "success", mustJSON(data)))
			view := s.requestViewOf(r, saved, "elevation", types.DeciderKindElevation, elevationView(s, ctx, saved), map[uuid.UUID]string{unit.ID: unit.Name})
			writeJSON(w, http.StatusAccepted, map[string]any{"pending_change": view.GovernanceChange, "request": view})
		},
	})
}

// elevationUnitAndRule is create steps 4.1-4.2a: the unit (not Default, known), its met elevation rule,
// the caller's eligibility under it and the eligible_requesters entries they matched. Every miss is notOffered.
func (s *Server) elevationUnitAndRule(ctx context.Context, rawUnit string) (types.GovernanceUnit, *types.DeciderRule, []string, *elevationRefusal, error) {
	id, err := uuid.Parse(rawUnit)
	if err != nil {
		return types.GovernanceUnit{}, nil, nil, notOffered("this unit"), nil
	}
	units, err := s.unitOutlines(ctx)
	if err != nil {
		return types.GovernanceUnit{}, nil, nil, nil, err
	}
	i := slices.IndexFunc(units, func(u types.GovernanceUnit) bool { return u.ID == id })
	if i < 0 || isDefaultUnit(units[i]) {
		return types.GovernanceUnit{}, nil, nil, notOffered("this unit"), nil
	}
	unit := units[i]
	rule, _, err := s.requestRuleFor(ctx, nil, unit.ID, types.DeciderKindElevation)
	if err != nil {
		return unit, nil, nil, nil, fmt.Errorf("%w: %w", errDecidersRead, err)
	}
	groups, err := s.elevationMatchedGroups(ctx, rule)
	if err != nil {
		return unit, nil, nil, nil, err
	}
	if len(groups) == 0 {
		return unit, nil, nil, notOffered(unit.Name), nil
	}
	return unit, rule, groups, nil, nil
}

// isDefaultUnit is whether u is the Default unit, which offers no elevation.
func isDefaultUnit(u types.GovernanceUnit) bool {
	return u.ID == store.DefaultUnitID || u.Kind == types.UnitKindDefault
}

// elevationEligible is create step 2a: a rule, a usable group snapshot, and a group of the rule's
// eligible_requesters the caller is in.
func (s *Server) elevationEligible(ctx context.Context, rule *types.DeciderRule) (bool, error) {
	groups, err := s.elevationMatchedGroups(ctx, rule)
	return len(groups) > 0, err
}

// elevationMatchedGroups is the rule's eligible_requesters entries the caller is in, in the rule's order
// (callerInGroup: a bare API token only with WARDYN_ROLE_STAMP_TTL set). None for no rule or a stale or
// truncated group snapshot.
func (s *Server) elevationMatchedGroups(ctx context.Context, rule *types.DeciderRule) ([]string, error) {
	if rule == nil {
		return nil, nil
	}
	subj, err := s.callerSubjects(ctx)
	if err != nil {
		return nil, err
	}
	if subj.stale || oidcGroupsTruncatedFromContext(ctx) {
		return nil, nil
	}
	var matched []string
	for _, entry := range rule.EligibleRequesters {
		if g, ok := types.DeciderGroupOf(entry); ok && s.callerInGroup(ctx, g) {
			matched = append(matched, entry)
		}
	}
	return matched, nil
}

// elevationDuration is create step 4.3: at least 15m and at most the met rule's max_elevation.
func elevationDuration(unit types.GovernanceUnit, rule types.DeciderRule, raw string) *elevationRefusal {
	d, err := time.ParseDuration(raw)
	if err != nil || d < types.MinMaxElevation {
		return &elevationRefusal{status: http.StatusBadRequest, reason: reasonElevationDurationInvalid, msg: "duration must be at least 15m, for example 4h"}
	}
	if limit := rule.MaxElevationDuration(); d > limit {
		return &elevationRefusal{status: http.StatusBadRequest, reason: reasonElevationTooLong,
			msg: fmt.Sprintf("%s allows at most %s.", unit.Name, types.CanonicalDuration(limit))}
	}
	return nil
}

// elevationNote is create step 4.4: the reason is required.
func elevationNote(reason, ticket string) (types.ChangeNote, *elevationRefusal) {
	note, err := types.ValidateChangeNote(reason, ticket, true)
	switch {
	case err == nil:
		return note, nil
	case errors.Is(err, types.ErrChangeNoteReasonRequired):
		return note, &elevationRefusal{status: http.StatusBadRequest, reason: reasonRequestReasonRequired, msg: "a request needs a reason"}
	case errors.Is(err, types.ErrChangeNoteReasonTooLong):
		return note, &elevationRefusal{status: http.StatusBadRequest, reason: reasonRequestReasonTooLong, msg: "a request's reason is at most 500 characters"}
	}
	return note, &elevationRefusal{status: http.StatusBadRequest, reason: reasonGovernanceChangeNoteInvalid, msg: err.Error()}
}

// elevationTicketPatterns is every elevation rule with a ticket_pattern on unitID's chain, nearest first:
// read on q inside a decision, else from the request's memoized tree.
func (s *Server) elevationTicketPatterns(ctx context.Context, q store.Querier, unitID uuid.UUID) ([]types.DeciderRule, error) {
	t, err := s.requestTree(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDecidersRead, err)
	}
	if t == nil {
		return nil, nil
	}
	rules, _ := t.chain(unitID, types.DeciderKindElevation)
	return slices.DeleteFunc(rules, func(r types.DeciderRule) bool { return r.TicketPattern == "" }), nil
}

// elevationTicketRefusal is create step 4a: with a ticket_pattern on the chain, a ticket is required and
// must match every pattern whole.
func (s *Server) elevationTicketRefusal(ctx context.Context, q store.Querier, unit types.GovernanceUnit, ticket string) (*elevationRefusal, error) {
	rules, err := s.elevationTicketPatterns(ctx, q, unit.ID)
	if err != nil || len(rules) == 0 {
		return nil, err
	}
	if ticket == "" {
		return &elevationRefusal{status: http.StatusBadRequest, reason: reasonElevationTicketRequired, msg: unit.Name + " needs a ticket"}, nil
	}
	for _, r := range rules {
		if re := r.TicketRegexp(); re == nil || !re.MatchString(ticket) {
			return &elevationRefusal{status: http.StatusBadRequest, reason: reasonElevationTicketInvalid,
				msg: unit.Name + " needs a ticket like " + r.TicketPattern}, nil
		}
	}
	return nil, nil
}

// elevationNoteTicketOK is the note route's step 4a for an elevation change: the new ticket, at the
// payload's unit, meets every pattern on the chain. It writes the refusal.
func (s *Server) elevationNoteTicketOK(w http.ResponseWriter, r *http.Request, ch types.GovernanceChange, ticket string) bool {
	ctx := r.Context()
	unitID, ok := requestUnitOf(ch)
	if !ok {
		writeServerError(w, r, "read the request's unit", errors.New("an elevation change names no unit"))
		return false
	}
	name := "this unit"
	if names, err := s.unitNames(ctx); err == nil && names[unitID] != "" {
		name = names[unitID]
	}
	ref, err := s.elevationTicketRefusal(ctx, nil, types.GovernanceUnit{ID: unitID, Name: name}, ticket)
	if errors.Is(err, errDecidersRead) {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDecidersUnreadable, "the deciders rules could not be read; try again")
		return false
	}
	if err != nil {
		writeServerError(w, r, "check the request's ticket", err)
		return false
	}
	if ref != nil {
		ref.write(w)
		return false
	}
	return true
}

// elevationMemberOrCooldown is create steps 4.5 and 4.5a: no live membership of the caller's places them
// in the unit already, and their last elevation there ended at least renew_after ago.
func (s *Server) elevationMemberOrCooldown(ctx context.Context, unit types.GovernanceUnit, rule types.DeciderRule, key string) (*elevationRefusal, error) {
	member, err := s.callerMemberUnits(ctx)
	if err != nil {
		return nil, err
	}
	if member[unit.ID] {
		return &elevationRefusal{status: http.StatusConflict, reason: reasonElevationAlreadyMember, msg: "you are already in " + unit.Name}, nil
	}
	var until *time.Time
	err = s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) error {
		var err error
		until, err = elevationCooldownUntil(ctx, q, key, rule, s.cfg.Now())
		return err
	})
	if err != nil || until == nil {
		return nil, err
	}
	return &elevationRefusal{status: http.StatusConflict, reason: reasonElevationCooldown, retryAfter: until,
		msg: "your last elevation in " + unit.Name + " ended too recently; ask again after " + until.UTC().Format(time.RFC3339)}, nil
}

// callerMemberUnits is the units a live membership row matching the caller (user, group or user_type)
// places them in.
func (s *Server) callerMemberUnits(ctx context.Context) (map[uuid.UUID]bool, error) {
	subj, err := s.callerSubjects(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.cfg.Store.ListMembershipsFor(ctx, subj.users, subj.groups, subj.userType)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(rows))
	for _, m := range rows {
		out[m.Membership.UnitID] = true
	}
	return out, nil
}

// elevationCooldownUntil is when the person at key may next ask: their last applied elevation's decision
// plus its duration plus the rule's renew_after, when that is after now; nil otherwise.
func elevationCooldownUntil(ctx context.Context, q store.Querier, key string, rule types.DeciderRule, now time.Time) (*time.Time, error) {
	last, err := store.LastAppliedGovernanceChangeQ(ctx, q, types.GovernanceTargetElevation, key)
	if err != nil || last == nil || last.DecidedAt == nil {
		return nil, err
	}
	var p elevationPayload
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		return nil, fmt.Errorf("api: read a past elevation's payload: %w", err)
	}
	d, err := time.ParseDuration(p.Duration)
	if err != nil {
		return nil, fmt.Errorf("api: read a past elevation's duration: %w", err)
	}
	until := last.DecidedAt.Add(d + rule.RenewAfterDuration()).UTC()
	if !until.After(now) {
		return nil, nil
	}
	return &until, nil
}

// elevationOffers is every non-Default unit the caller may ask an elevation in, sorted by name.
func (s *Server) elevationOffers(ctx context.Context) ([]ElevationOffer, error) {
	out := []ElevationOffer{}
	t, err := s.deciderTree(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errDecidersRead, err)
	}
	if t == nil {
		return out, nil
	}
	units, err := s.unitOutlines(ctx)
	if err != nil {
		return nil, err
	}
	var member map[uuid.UUID]bool
	for _, u := range units {
		if isDefaultUnit(u) {
			continue
		}
		rule, _, ok := t.ruleFor(u.ID, types.DeciderKindElevation)
		if !ok {
			continue
		}
		if eligible, err := s.elevationEligible(ctx, &rule); err != nil || !eligible {
			if err != nil {
				return nil, err
			}
			continue
		}
		if member == nil {
			if member, err = s.callerMemberUnits(ctx); err != nil {
				return nil, err
			}
		}
		if member[u.ID] {
			continue
		}
		rules, _ := t.chain(u.ID, types.DeciderKindElevation)
		out = append(out, ElevationOffer{UnitID: u.ID, UnitName: u.Name, MaxElevation: rule.MaxElevation,
			TicketRequired: slices.ContainsFunc(rules, func(r types.DeciderRule) bool { return r.TicketPattern != "" })})
	}
	slices.SortFunc(out, func(a, b ElevationOffer) int { return strings.Compare(a.UnitName, b.UnitName) })
	return out, nil
}

// applyElevationChange is an approved elevation's write (C5): under the graph and key locks, the unit
// and the rule as they stand now still allow it, an ended row at the key is cleared, and a user
// membership ending duration after the decision is inserted. Any change that would have refused the
// request is stale.
func applyElevationChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var p elevationPayload
	if err := decodeHeldPayload(ch.Payload, &p); err != nil {
		return govApplied{}, fmt.Errorf("api: decode an elevation change: %w", err)
	}
	dur, err := time.ParseDuration(p.Duration)
	if err != nil {
		return govApplied{}, fmt.Errorf("api: an elevation change's duration: %w", err)
	}
	if err := store.LockGovernanceGraph(ctx, q); err != nil {
		return govApplied{}, err
	}
	if err := store.LockGovernanceMembershipKey(ctx, q, string(types.CapabilitySubjectUser), p.Subject); err != nil {
		return govApplied{}, err
	}
	unit, err := store.GetGovernanceUnitQ(ctx, q, p.UnitID, false)
	if errors.Is(err, store.ErrNotFound) || (err == nil && isDefaultUnit(unit)) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	rule, _, err := s.requestRuleFor(ctx, q, unit.ID, types.DeciderKindElevation)
	if err != nil {
		return govApplied{}, err
	}
	if rule == nil || dur > rule.MaxElevationDuration() {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	// The rule must still name a group the person matched when asking; a request saved without groups fails closed.
	if !slices.ContainsFunc(p.Groups, func(g string) bool { return slices.Contains(rule.EligibleRequesters, g) }) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	now := s.cfg.Now().UTC()
	if until, err := elevationCooldownUntil(ctx, q, ch.TargetKey, *rule, now); err != nil || until != nil {
		return govApplied{}, cmp.Or(err, store.ErrGovernanceChangeStale)
	}
	ticket := ""
	if ch.Note != nil {
		ticket = ch.Note.Ticket
	}
	if ref, err := s.elevationTicketRefusal(ctx, q, unit, ticket); err != nil || ref != nil {
		return govApplied{}, cmp.Or(err, store.ErrGovernanceChangeStale)
	}
	ended, err := store.DeleteExpiredMembershipAtKeyQ(ctx, q, unit.ID, string(types.CapabilitySubjectUser), p.Subject)
	if err != nil {
		return govApplied{}, err
	}
	var endedBy *types.GovernanceChange
	if ended != nil {
		if endedBy, err = store.LastAppliedGovernanceChangeQ(ctx, q, types.GovernanceTargetElevation, ch.TargetKey); err != nil {
			return govApplied{}, err
		}
	}
	_, err = store.GetGovernanceMembershipByKeyQ(ctx, q, unit.ID, string(types.CapabilitySubjectUser), p.Subject, true)
	if err == nil {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if !errors.Is(err, store.ErrNotFound) {
		return govApplied{}, err
	}
	_, approver := actorFromRequest(r)
	end := now.Add(dur)
	m := types.GovernanceMembership{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: p.Subject, UnitID: unit.ID,
		CreatedAt: now, CreatedBy: approver, ExpiresAt: &end}
	if err := store.InsertGovernanceMembershipQ(ctx, q, m); err != nil {
		return govApplied{}, err
	}
	data := map[string]any{"unit_id": unit.ID, "units": []string{unit.ID.String()}, "subject_type": types.CapabilitySubjectUser,
		"subject": p.Subject, "duration": p.Duration, "expires_at": end}
	if ticket != "" {
		data["ticket"] = ticket
	}
	return govApplied{action: "governance.elevation.approve", target: m.ID.String(), data: data,
		// The pool reads (the decider's scope) and the ended row's own audit row wait for the commit.
		afterCommit: func() map[string]any {
			more := map[string]any{}
			if scope := s.decisionUnitScope(ctx, unit.ID, rule); scope != "" {
				more["unit_scope"] = scope
			}
			if ended != nil {
				s.auditElevationEnd(ctx, *ended, endedBy)
				more["replaced_expired"] = ended.ID
			}
			return more
		}}, nil
}

// auditElevationEnd writes governance.elevation.expire for an elevation row that has ended, with the
// ticket and change of the approval that wrote it when that is known.
func (s *Server) auditElevationEnd(ctx context.Context, m types.GovernanceMembership, by *types.GovernanceChange) {
	data := map[string]any{"unit_id": m.UnitID, "units": []string{m.UnitID.String()}, "subject_type": m.SubjectType,
		"subject": m.Subject, "expired_at": m.ExpiresAt}
	if by != nil {
		data["change_id"] = by.ID
		if by.Note != nil && by.Note.Ticket != "" {
			data["ticket"] = by.Note.Ticket
		}
	}
	// No run id: the row is a membership's, named by its target.
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "governance.elevation.expire", m.ID.String(), "success", mustJSON(data)))
}

// elevationView is an elevation's own fields of a request view: the unit, the duration and, once
// applied, the membership's end.
func elevationView(s *Server, ctx context.Context, ch types.GovernanceChange) requestFields {
	var p elevationPayload
	if json.Unmarshal(ch.Payload, &p) != nil {
		return requestFields{}
	}
	f := requestFields{unitID: &p.UnitID, duration: p.Duration}
	if ch.State != types.GovernanceChangeApplied {
		return f
	}
	_ = s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) error {
		m, err := store.GetGovernanceMembershipByKeyQ(ctx, q, p.UnitID, string(types.CapabilitySubjectUser), p.Subject, false)
		if err == nil && m.ExpiresAt != nil {
			f.endsAt = m.ExpiresAt
		}
		return nil
	})
	return f
}
