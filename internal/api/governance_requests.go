// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Held requests: GET and POST /governance/requests, and approve and reject at /governance/requests/{id}.
// A signed-in person asks for something only the deciders of a unit may give (an elevation today), and the
// request is held as a governance change: the same queue, the same in-transaction recheck, the same
// second-human rule and TTL. The routes sit on the member router, so a group decider who administers no
// unit reaches them; who may see or decide a request is the approver predicate's, inside the handler.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// requestKeep is how long a decided request stays in the list.
const requestKeep = 24 * time.Hour

// governanceRequestKind is one kind a person may request: the held-change target it is stored as, the
// decider kind whose rules decide it, how a create proposes it, and its fields read back from the stored
// payload. A new kind is a row of governanceRequestKinds; no handler changes.
type governanceRequestKind struct {
	target  string // types.GovernanceTarget*
	decider string // types.DeciderKind*
	// propose writes the 202 or the refusal.
	propose func(s *Server, w http.ResponseWriter, r *http.Request, body governanceRequestBody, who requestCaller)
	// view is the kind's fields from the stored payload.
	view func(s *Server, ctx context.Context, ch types.GovernanceChange) requestFields
}

var governanceRequestKinds = map[string]governanceRequestKind{
	"elevation": {target: types.GovernanceTargetElevation, decider: types.DeciderKindElevation, propose: proposeElevation, view: elevationView},
	"image":     {target: types.GovernanceTargetImageRequest, decider: types.DeciderKindImage, propose: proposeImageRequest, view: imageRequestView},
}

// requestKindOf is the request kind stored as targetKind; false for a change that is not a request.
func requestKindOf(targetKind string) (string, bool) {
	for name, k := range governanceRequestKinds {
		if k.target == targetKind {
			return name, true
		}
	}
	return "", false
}

// requestCaller is the person asking: what requester returns.
type requestCaller struct{ principal, email string }

// requestFields is one kind's own fields of a request view.
type requestFields struct {
	unitID   *uuid.UUID
	duration string
	endsAt   *time.Time
	image    string
}

// governanceRequestBody is POST /governance/requests' body.
type governanceRequestBody struct {
	Kind     string `json:"kind"`
	UnitID   string `json:"unit_id,omitempty"`
	Duration string `json:"duration,omitempty"`
	Image    string `json:"image,omitempty"` // an image request's; refused for kind elevation
	Reason   string `json:"reason"`
	Ticket   string `json:"ticket,omitempty"`
}

// GovernanceRequestView is one request as the list and the create answer it: the held change, redacted as
// /governance/changes redacts it, with the kind's fields and who decides it.
type GovernanceRequestView struct {
	types.GovernanceChange
	Kind      string           `json:"kind"` // elevation | image
	UnitID    *uuid.UUID       `json:"unit_id,omitempty"`
	UnitName  string           `json:"unit_name,omitempty"`
	Duration  string           `json:"duration,omitempty"` // types.CanonicalDuration, as max_elevation is stored: "4h", "1h30m"
	EndsAt    *time.Time       `json:"ends_at,omitempty"`  // applied elevations only: the membership's expires_at
	Image     string           `json:"image,omitempty"`    // an image request's
	Mine      bool             `json:"mine"`
	Decidable bool             `json:"decidable"`
	Deciders  []changeApprover `json:"deciders"`
}

// ElevationOffer is one unit the caller may ask an elevation in.
type ElevationOffer struct {
	UnitID         uuid.UUID `json:"unit_id"`
	UnitName       string    `json:"unit_name"`
	MaxElevation   string    `json:"max_elevation"`             // the met rule's
	TicketRequired bool      `json:"ticket_required,omitempty"` // a rule on the chain has a ticket_pattern
}

// governanceRequestList is GET /governance/requests' body.
type governanceRequestList struct {
	Requests []GovernanceRequestView `json:"requests"`
	Offered  []ElevationOffer        `json:"offered"`
	// ImageOffered is whether the caller may ask for an image: a person whose default unit has an image
	// rule on it or above it (imageRequestOffered).
	ImageOffered bool `json:"image_offered"`
}

func (s *Server) mountGovernanceRequestRoutes(member chi.Router) {
	member.Get("/governance/requests", s.handleListGovernanceRequests)
	member.Post("/governance/requests", s.handleCreateGovernanceRequest)
	member.Post("/governance/requests/{id}/approve", func(w http.ResponseWriter, r *http.Request) { s.handleDecideGovernanceRequest(w, r, true) })
	member.Post("/governance/requests/{id}/reject", func(w http.ResponseWriter, r *http.Request) { s.handleDecideGovernanceRequest(w, r, false) })
}

// requester is the caller as a person who may request: signed in, not a device, a service, a portal or
// an approval rule, and not local mode. The admin token holds no person and is refused here.
func (s *Server) requester(r *http.Request) (requestCaller, bool) {
	ctx := r.Context()
	if oidcHumanFromContext(ctx) == "" || neverOperator(ctx) || s.cfg.LocalMode {
		return requestCaller{}, false
	}
	_, principal := actorFromRequest(r)
	return requestCaller{principal: principal, email: oidcEmailFromContext(ctx)}, true
}

// handleCreateGovernanceRequest is POST /governance/requests: 202 with the held change and the request.
func (s *Server) handleCreateGovernanceRequest(w http.ResponseWriter, r *http.Request) {
	who, ok := s.requester(r)
	if !ok {
		writeErrorReason(w, http.StatusForbidden, reasonGovernanceRequestNeedsPerson, "only a signed-in person can request this")
		return
	}
	var body governanceRequestBody
	if !decodeStrict(w, r, &body) {
		return
	}
	kind, ok := governanceRequestKinds[body.Kind]
	if !ok {
		names := make([]string, 0, len(governanceRequestKinds))
		for name := range governanceRequestKinds {
			names = append(names, name)
		}
		slices.Sort(names)
		writeErrorReason(w, http.StatusBadRequest, reasonGovernanceRequestKindInvalid, "kind must be one of: "+strings.Join(names, ", "))
		return
	}
	kind.propose(s, w, withDeciderMemo(r), body, who)
}

// handleListGovernanceRequests is GET /governance/requests: the requests the caller may see (pending, and
// decided in the last day) and the units they may ask an elevation in.
func (s *Server) handleListGovernanceRequests(w http.ResponseWriter, r *http.Request) {
	r = withDeciderMemo(r)
	ctx := r.Context()
	out := governanceRequestList{Requests: []GovernanceRequestView{}, Offered: []ElevationOffer{}}
	if rr, ok := s.cfg.Store.(store.GovernanceRequestReader); ok {
		targets := make([]string, 0, len(governanceRequestKinds))
		for _, k := range governanceRequestKinds {
			targets = append(targets, k.target)
		}
		rows, err := rr.ListGovernanceRequests(ctx, targets, s.cfg.Now().UTC().Add(-requestKeep))
		if err != nil {
			writeServerError(w, r, "list governance requests", err)
			return
		}
		var names map[uuid.UUID]string
		for _, ch := range rows {
			if !s.canSeeGovernanceChange(r, ch) {
				continue
			}
			if names == nil {
				if names, err = s.unitNames(ctx); err != nil {
					writeServerError(w, r, "read the unit names", err)
					return
				}
			}
			out.Requests = append(out.Requests, s.requestView(r, ch, names))
		}
	}
	if _, ok := s.requester(r); ok {
		offers, err := s.elevationOffers(ctx)
		if errors.Is(err, errDecidersRead) {
			writeErrorReason(w, http.StatusServiceUnavailable, reasonDecidersUnreadable, "the deciders rules could not be read; try again")
			return
		}
		if err != nil {
			writeServerError(w, r, "list elevation offers", err)
			return
		}
		out.Offered = offers
	}
	out.ImageOffered = s.imageRequestOfferedQuiet(r)
	writeJSON(w, http.StatusOK, out)
}

// errDecidersRead marks a failed read of the deciders rules or the unit tree they are walked on.
var errDecidersRead = errors.New("api: read the deciders rules")

// unitNames is every unit's name by id.
func (s *Server) unitNames(ctx context.Context) (map[uuid.UUID]string, error) {
	units, err := s.unitOutlines(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string, len(units))
	for _, u := range units {
		out[u.ID] = u.Name
	}
	return out, nil
}

// requestView is ch as the caller reads it.
func (s *Server) requestView(r *http.Request, ch types.GovernanceChange, names map[uuid.UUID]string) GovernanceRequestView {
	name, _ := requestKindOf(ch.TargetKind)
	kind := governanceRequestKinds[name]
	return s.requestViewOf(r, ch, name, kind.decider, kind.view(s, r.Context(), ch), names)
}

// requestViewOf is requestView with the kind given: a kind's own create answers through it, so the kinds
// table never refers to itself.
func (s *Server) requestViewOf(r *http.Request, ch types.GovernanceChange, name, decider string, f requestFields, names map[uuid.UUID]string) GovernanceRequestView {
	ctx := r.Context()
	_, principal := actorFromRequest(r)
	v := GovernanceRequestView{GovernanceChange: s.changeForRead(r, ch), Kind: name, UnitID: f.unitID, Duration: f.duration,
		EndsAt: f.endsAt, Image: f.image, Mine: sameGovernanceHuman(ch, principal, oidcEmailFromContext(ctx)),
		Deciders: s.requestApprovers(ctx, ch, decider)}
	if f.unitID != nil {
		v.UnitName = names[*f.unitID]
	}
	v.Decidable = ch.State == types.GovernanceChangePending && !v.Mine && governanceChangeKinds[ch.TargetKind].forChange(ch).approver.allowed(s, r)
	return v
}

// handleDecideGovernanceRequest is approve and reject at /governance/requests/{id}: the decision of
// /governance/changes/{id}, reached only for a request the caller may see. An unknown id, a change that is
// not a request and one the caller may not see answer the same 404.
func (s *Server) handleDecideGovernanceRequest(w http.ResponseWriter, r *http.Request, approve bool) {
	id, ok := parseIDParam(w, r, "id", "governance change")
	if !ok {
		return
	}
	r = withDeciderMemo(r)
	ch, err := s.cfg.Store.GetGovernanceChange(r.Context(), id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeServerError(w, r, "read governance change", err)
		return
	}
	if _, isRequest := requestKindOf(ch.TargetKind); err != nil || !isRequest || !s.canSeeGovernanceChange(r, ch) {
		writeErrorReason(w, http.StatusNotFound, reasonGovernanceChangeNotFound, "governance change not found")
		return
	}
	s.decideGovernanceChange(w, r, approve)
}

// requestUnitOf is the unit a request's payload names.
func requestUnitOf(ch types.GovernanceChange) (uuid.UUID, bool) {
	var p struct {
		UnitID uuid.UUID `json:"unit_id"`
	}
	if json.Unmarshal(ch.Payload, &p) != nil || p.UnitID == uuid.Nil {
		return uuid.Nil, false
	}
	return p.UnitID, true
}

// requestTree is the deciders rules and the unit tree: read on q inside a decision's transaction, else the
// request's memoized read (nil when no unit has a rule).
func (s *Server) requestTree(ctx context.Context, q store.Querier) (*deciderTree, error) {
	if q == nil {
		return s.deciderTree(ctx)
	}
	rows, err := store.ListUnitDecidersQ(ctx, q)
	if err != nil {
		return nil, err
	}
	units, err := store.ListGovernanceUnitsQ(ctx, q)
	if err != nil {
		return nil, err
	}
	return newDeciderTree(rows, units), nil
}

// requestRuleFor is the rule for kind at unitID: the meet of every rule for it from the unit up, and the
// nearest unit holding one. nil when there is none.
func (s *Server) requestRuleFor(ctx context.Context, q store.Querier, unitID uuid.UUID, kind string) (*types.DeciderRule, uuid.UUID, error) {
	t, err := s.requestTree(ctx, q)
	if err != nil || t == nil {
		return nil, uuid.Nil, err
	}
	rule, from, ok := t.ruleFor(unitID, kind)
	if !ok {
		return nil, uuid.Nil, nil
	}
	return &rule, from, nil
}

// requestApprover is who may decide a request of kind: a security caller, or a caller the met rule at the
// request's unit names (unit_admins by a grant on or above the unit, group:<g> by the session's groups).
// Inside the decision's transaction the rule and the scope are read there, so a decider the rule dropped
// meanwhile is refused. A read that fails, or no rule, refuses. The requester is excluded by the
// decision's second-human rule, not here.
func requestApprover(ch types.GovernanceChange, kind string) govApprover {
	return govApprover{
		allowed: func(s *Server, r *http.Request) bool {
			if securityApprover.allowed(s, r) { // the security tier always decides, rule or none
				return true
			}
			ctx := r.Context()
			unitID, ok := requestUnitOf(ch)
			if !ok {
				return false
			}
			q, _ := ctx.Value(unitScopeTxKey{}).(store.Querier)
			rule, _, err := s.requestRuleFor(ctx, q, unitID, kind)
			if err != nil {
				slog.ErrorContext(ctx, "api: read the deciders rule of a request", "err", err)
				return false
			}
			return rule != nil && s.requestDeciderEntry(ctx, *rule, unitID) != ""
		},
		reason: authz.ReasonUnitScopeDenied,
		scoped: true,
	}
}

// requestDeciderEntry is the first entry of rule's who the caller qualifies under at unit, "" for none.
func (s *Server) requestDeciderEntry(ctx context.Context, rule types.DeciderRule, unit uuid.UUID) string {
	for _, who := range rule.Who {
		if who == types.DeciderUnitAdmins {
			if in, err := s.unitScope(ctx, unit, false); err == nil && in {
				return who
			}
			continue
		}
		if g, ok := types.DeciderGroupOf(who); ok && s.callerInGroup(ctx, g) {
			return who
		}
	}
	return ""
}

// decisionUnitScope is the scope a request's decider decided in, for the audit row: auditUnitScope's
// ("security" or the granting unit), else "group:<g>" for the first group entry of the rule the caller is
// in; "" when neither applies. It reads the pool, so it runs after the decision has committed.
func (s *Server) decisionUnitScope(ctx context.Context, unit uuid.UUID, rule *types.DeciderRule) string {
	if scope := s.auditUnitScope(ctx, unit, false); scope != "" {
		return scope
	}
	if rule == nil {
		return ""
	}
	for _, who := range rule.Who {
		if g, ok := types.DeciderGroupOf(who); ok && s.callerInGroup(ctx, g) {
			return who
		}
	}
	return ""
}

// requestApprovers lists who may decide a request of kind: any security admin, then for each entry of the
// met rule's who the unit admins covering the unit (the requester's own grants left out) or the group.
func (s *Server) requestApprovers(ctx context.Context, ch types.GovernanceChange, kind string) []changeApprover {
	out := []changeApprover{{Kind: "security"}}
	unitID, ok := requestUnitOf(ch)
	if !ok {
		return out
	}
	rule, _, err := s.requestRuleFor(ctx, nil, unitID, kind)
	if err != nil || rule == nil {
		return out
	}
	for _, who := range rule.Who {
		if who == types.DeciderUnitAdmins {
			out = append(out, s.approversFor(ctx, []uuid.UUID{unitID}, false, ch.ProposedBy, ch.ProposedByEmail)[1:]...)
		} else if g, ok := types.DeciderGroupOf(who); ok {
			out = append(out, changeApprover{Kind: "group", Name: g})
		}
	}
	return out
}

// requestRejectData is a request kind's reject row: the payload's fields, who it was for, the change, the
// ticket, and the decider's scope (none on a withdraw).
func (s *Server) requestRejectData(ctx context.Context, ch types.GovernanceChange, withdrawn bool) map[string]any {
	data := map[string]any{}
	_ = json.Unmarshal(ch.Payload, &data)
	data["subject_type"], data["change_id"] = types.CapabilitySubjectUser, ch.ID
	if ch.Note != nil && ch.Note.Ticket != "" {
		data["ticket"] = ch.Note.Ticket
	}
	unitID, ok := requestUnitOf(ch)
	if ok {
		data["units"] = []string{unitID.String()}
	}
	if withdrawn {
		data["withdrawn"] = true
		return data
	}
	if ok {
		name, _ := requestKindOf(ch.TargetKind)
		rule, _, _ := s.requestRuleFor(ctx, nil, unitID, governanceRequestKinds[name].decider)
		if scope := s.decisionUnitScope(ctx, unitID, rule); scope != "" {
			data["unit_scope"] = scope
		}
	}
	return data
}
