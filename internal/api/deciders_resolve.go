// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Who decides a run hold under a unit's deciders (types/deciders.go). One resolver answers every caller: the
// decision itself (authorizeUserDecision's member arm, decide, requireSecondHuman) and its read-only mirror
// (mayDecide, projectDecidable), so the two cannot drift. The rule is read at decide time, from the run's
// frozen unit up its ancestors, and the nearest unit with a rule for the kind wins. With no rule anywhere it
// answers "no rule", and every caller keeps the verdict it gives without deciders.
package api

import (
	"context"
	"fmt"
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

// The bases an approval.decide row names when a rule governs the request. owner and group:<name> are the
// decider entries themselves.
const (
	deciderBasisSecurity    = "security"
	deciderBasisAdminToken  = "admin_token"
	deciderBasisPolicy      = "policy"
	deciderBasisUnitAdminOf = "unit_admin:"
)

// deciderVerdict is the resolver's answer for one approval and one caller.
type deciderVerdict struct {
	// rule governs the approval's kind for the run's unit; nil when none does.
	rule *types.DeciderRule
	// unit holds rule.
	unit uuid.UUID
	// kind is the approval's decider kind, "" for a request no rule names.
	kind string
	// basis is how the caller decides under rule, "" when they do not (or there is no rule).
	basis string
	// ownerCapDenied: rule lists owner and the caller owns the run, but the egress_host grant refuses them the
	// host. The member path then answers its own capability refusal.
	ownerCapDenied bool
}

func (v deciderVerdict) decides() bool { return v.basis != "" }

func (v deciderVerdict) secondHuman() bool { return v.rule != nil && v.rule.SecondHuman }

// ruleRef is the rule an approval.decide row names, nil with none.
func (v deciderVerdict) ruleRef() *types.DeciderRuleRef {
	if v.rule == nil {
		return nil
	}
	return &types.DeciderRuleRef{Unit: v.unit, Kind: v.kind}
}

// deciderList is who decides under the rule, security last; nil with no rule.
func (v deciderVerdict) deciderList() []string {
	if v.rule == nil {
		return nil
	}
	return append(slices.Clone(v.rule.Who), deciderBasisSecurity)
}

// deciderSwitchName is the second-human switch a rule's second_human reads as in a refusal and in the
// break-glass audit row.
func deciderSwitchName(unit uuid.UUID) string { return "unit deciders (" + unit.String() + ")" }

// deciderKindOf is the kind a rule names for ap. Seven of the nine kinds are run holds: egress_domain,
// credential, push_content, a reviewed download (file_download), and the three tool_calls a person may decide, an
// Azure DevOps escalation, one MCP call and one kubectl write (kube_call). image and elevation name requests that
// are not run holds, so no approval maps to them and a rule for one of them governs no run hold. Any other row (a
// plain tool_call, credential_reauth, human_takeover) maps to none and is decided exactly as without deciders.
func deciderKindOf(ap types.ApprovalRequest) string {
	switch ap.Kind {
	case types.ApprovalEgressDomain:
		return types.DeciderKindEgressDomain
	case types.ApprovalCredential:
		return types.DeciderKindCredential
	case types.ApprovalPushContent:
		return types.DeciderKindPushContent
	case types.ApprovalFileDownload:
		return types.DeciderKindFileDownload
	case types.ApprovalToolCall:
		if _, isADO := adoEscalationScope(ap); isADO {
			return types.DeciderKindADOEscalation
		}
		if isKubeCallRow(ap) {
			return types.DeciderKindKubeCall
		}
		if isMCPCallRow(ap) {
			return types.DeciderKindMCPCall
		}
	}
	return ""
}

// deciderTree is every unit's rules and the unit tree's parent links, read once per request.
type deciderTree struct {
	rules   map[uuid.UUID][]types.DeciderRule
	parents map[uuid.UUID]*uuid.UUID
}

// ruleFor walks unit and its ancestors; the nearest with a rule for kind wins. A unit the tree no longer holds
// ends the walk with no rule, as a deleted unit leaves a run nothing to bind.
func (t *deciderTree) ruleFor(unit uuid.UUID, kind string) (types.DeciderRule, uuid.UUID, bool) {
	cur := &unit
	for n := 0; cur != nil && n < store.MaxUnitDepthCap; n++ {
		parent, exists := t.parents[*cur]
		if !exists {
			break
		}
		if r, ok := types.DeciderRuleFor(t.rules[*cur], kind); ok {
			return r, *cur, true
		}
		cur = parent
	}
	return types.DeciderRule{}, uuid.Nil, false
}

type deciderMemoKey struct{}

// deciderMemo is one request's read of the rules and the tree.
type deciderMemo struct {
	mu   sync.Mutex
	read bool
	tree *deciderTree
	err  error
}

// withDeciderMemo is r whose deciders reads happen once.
func withDeciderMemo(r *http.Request) *http.Request {
	if _, ok := r.Context().Value(deciderMemoKey{}).(*deciderMemo); ok {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), deciderMemoKey{}, &deciderMemo{}))
}

// deciderTree is the rules and the tree, nil when no unit has a rule (or the store keeps none). It reads the
// tree only when a rule exists, so a deployment without deciders pays one read of an empty table.
func (s *Server) deciderTree(ctx context.Context) (*deciderTree, error) {
	m, _ := ctx.Value(deciderMemoKey{}).(*deciderMemo)
	if m == nil {
		return s.readDeciderTree(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.read {
		m.tree, m.err = s.readDeciderTree(ctx)
		m.read = true
	}
	return m.tree, m.err
}

func (s *Server) readDeciderTree(ctx context.Context) (*deciderTree, error) {
	st := s.unitDeciderStore()
	if st == nil {
		return nil, nil
	}
	rows, err := st.ListUnitDeciders(ctx)
	if err != nil {
		return nil, fmt.Errorf("read unit deciders: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	units, err := s.unitOutlines(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the unit tree: %w", err)
	}
	t := &deciderTree{rules: map[uuid.UUID][]types.DeciderRule{}, parents: map[uuid.UUID]*uuid.UUID{}}
	for _, d := range rows {
		t.rules[d.UnitID] = d.Rules
	}
	for _, u := range units {
		t.parents[u.ID] = u.ParentID
	}
	return t, nil
}

// governingRule is the rule that governs ap's kind for run's unit, with no caller in it. No rule: a request no
// rule names, a run with no unit or whose unit is gone, or no rule on the unit or above it. A read that fails is
// an error, never "no rule".
func (s *Server) governingRule(ctx context.Context, ap types.ApprovalRequest, run types.AgentRun) (deciderVerdict, error) {
	v := deciderVerdict{kind: deciderKindOf(ap)}
	if v.kind == "" || run.UnitID == nil {
		return v, nil
	}
	t, err := s.deciderTree(ctx)
	if err != nil || t == nil {
		return v, err
	}
	if r, unit, ok := t.ruleFor(*run.UnitID, v.kind); ok {
		v.rule, v.unit = &r, unit
	}
	return v, nil
}

// resolveDecider is governingRule plus how the caller decides under the rule. An approval rule decides as the
// security tier's automation and matches no who entry; a security caller always decides, and the rule's
// second_human and max_scope still bind them. Anyone else decides through the first who entry that matches:
// owner (the run's creator, with the egress_host grant for an egress host), unit_admins (a grant on the run's
// unit or above) or group:<name> (a complete group snapshot). unit_admins and group never match the run's own
// people, who decide only through owner. A device, a service identity and a portal's delegated token match none.
func (s *Server) resolveDecider(r *http.Request, ap types.ApprovalRequest, run types.AgentRun) (deciderVerdict, error) {
	ctx := r.Context()
	v, err := s.governingRule(ctx, ap, run)
	if err != nil || v.rule == nil {
		return v, err
	}
	_, principal := actorFromRequest(r)
	if _, isPolicy := policyDeciderFrom(ctx); isPolicy {
		v.basis = deciderBasisPolicy
		return v, nil
	}
	if s.isSecurityOperator(ctx) {
		v.basis = deciderBasisSecurity
		if isAdminTokenCaller(r) {
			v.basis = deciderBasisAdminToken
		}
		return v, nil
	}
	if neverOperator(ctx) || principal == "" {
		return v, nil
	}
	for _, who := range v.rule.Who {
		switch {
		case who == types.DeciderOwner:
			if run.CreatedBy != principal {
				continue
			}
			if ap.Kind == types.ApprovalEgressDomain {
				allowed, err := s.capSeamAllowed(ctx, capEgressHost, approvalHost(ap))
				if err != nil {
					return v, err
				}
				if !allowed {
					v.ownerCapDenied = true
					continue
				}
			}
			v.basis = types.DeciderOwner
		case who == types.DeciderUnitAdmins:
			in, err := s.unitScope(ctx, *run.UnitID, false)
			if err != nil {
				return v, err
			}
			if !in || s.isRunsOwnPerson(ctx, run, principal) {
				continue
			}
			v.basis = deciderBasisUnitAdminOf + run.UnitID.String()
		default:
			g, ok := types.DeciderGroupOf(who)
			if !ok || !s.callerInGroup(ctx, g) || s.isRunsOwnPerson(ctx, run, principal) {
				continue // a type this build does not know matches nobody
			}
			v.basis = who
		}
		return v, nil
	}
	return v, nil
}

// callerInGroup reports whether the caller's group snapshot, complete and from a person, holds g.
func (s *Server) callerInGroup(ctx context.Context, g string) bool {
	_, groups := s.unitAdminSubjects(ctx)
	return slices.ContainsFunc(groups, func(have string) bool {
		c, ok := types.CanonicalGroupSubject(have)
		return ok && c == g
	})
}

// isRunsOwnPerson reports whether principal is one of run's own people: its creator, its recorded human author,
// or the sponsor of the service identity that created it. A read that cannot answer counts as yes.
func (s *Server) isRunsOwnPerson(ctx context.Context, run types.AgentRun, principal string) bool {
	if run.HumanAuthor != "" && run.HumanAuthor == principal {
		return true
	}
	authored, err := s.runAuthoredBy(ctx, run, principal)
	return authored || err != nil
}

// memberDecidesWithoutRules reports the kinds a run's owner decides with no deciders rule.
func memberDecidesWithoutRules(ap types.ApprovalRequest) bool {
	_, isADO := adoEscalationScope(ap)
	return ap.Kind == types.ApprovalEgressDomain || isADO || isMCPCallRow(ap)
}

// deciderMemberGate is authorizeUserDecision's arm for a member when a rule may govern ap. handled false leaves
// the member path as it is without deciders: no rule anywhere, a request no rule names, a run that cannot be
// read, and a caller the rule does not name who is not the run's owner (the same 404). It answers itself for a
// decider (pass, both rows loaded), for the run's owner the rule leaves out (403 not_a_decider, naming who
// decides), and for a rule read that failed, which the run's owner is told (503) and nobody else is.
func (s *Server) deciderMemberGate(w http.ResponseWriter, r *http.Request, id uuid.UUID, ap types.ApprovalRequest) (run types.AgentRun, handled, ok bool) {
	ctx := r.Context()
	if deciderKindOf(ap) == "" {
		return run, false, false
	}
	if t, err := s.deciderTree(ctx); err == nil && t == nil {
		return run, false, false
	}
	run, err := s.cfg.Store.GetRun(ctx, ap.RunID)
	if err != nil {
		return run, false, false
	}
	owner := run.CreatedBy != "" && run.CreatedBy == principalFromRequest(r)
	v, err := s.resolveDecider(r, ap, run)
	switch {
	case err != nil && owner:
		s.writeDecidersUnreadable(w, r, ap, err)
		return run, true, false
	case err != nil || v.rule == nil:
		return run, false, false
	case v.decides():
		return run, true, true
	case owner && memberDecidesWithoutRules(ap) && !v.ownerCapDenied:
		s.refuse(w, r, authz.Deny(authz.ReasonNotADecider, id.String(), "the deciders of unit "+v.unit.String()+
			" decide this "+v.kind+" request: "+strings.Join(v.deciderList(), ", ")).OnRun(ap.RunID))
		return run, true, false
	}
	return run, false, false
}

// writeDecidersUnreadable answers a decision whose rules could not be read: 503, never read as "no rule".
func (s *Server) writeDecidersUnreadable(w http.ResponseWriter, r *http.Request, ap types.ApprovalRequest, err error) {
	slog.WarnContext(r.Context(), "approvals: unit deciders unreadable; the decision is refused", "approval_id", ap.ID, "err", err)
	writeErrorReason(w, http.StatusServiceUnavailable, reasonDecidersUnreadable,
		"who decides this request could not be read, so it is not decided now; try again")
}

// deciderForDecide is decide's read of the rule and the caller's basis, for every caller past the member gate,
// and the rule's max_scope over scope (capScopeToRule). A security caller whose run or rules cannot be read
// still decides, as without deciders (the deployment switches still bind); anyone else is refused (503). It
// loads the run into run when it reads one.
func (s *Server) deciderForDecide(w http.ResponseWriter, r *http.Request, id uuid.UUID, ap types.ApprovalRequest, run *types.AgentRun, haveRun *bool, scope types.ApprovalScope) (deciderVerdict, types.ApprovalScope, bool) {
	ctx := r.Context()
	if deciderKindOf(ap) == "" {
		return deciderVerdict{}, scope, true
	}
	t, err := s.deciderTree(ctx)
	if err == nil && t == nil {
		return deciderVerdict{}, scope, true
	}
	security := s.isSecurityOperator(ctx)
	if err == nil && !*haveRun {
		loaded, rerr := s.cfg.Store.GetRun(ctx, ap.RunID)
		if rerr != nil {
			err = fmt.Errorf("read the run: %w", rerr)
		} else {
			*run, *haveRun = loaded, true
		}
	}
	var v deciderVerdict
	if err == nil {
		v, err = s.resolveDecider(r, ap, *run)
	}
	switch {
	case err != nil && security:
		return deciderVerdict{}, scope, true
	case err != nil:
		s.writeDecidersUnreadable(w, r, ap, err)
		return deciderVerdict{}, scope, false
	}
	scope, ok := s.capScopeToRule(w, r, id, ap, v, scope)
	return v, scope, ok
}

// scopeRank orders how far a decision reaches; 0 for a scope this build does not know.
func scopeRank(sc types.ApprovalScope) int {
	switch sc {
	case types.ScopeOnce:
		return 1
	case types.ScopeUntil:
		return 2
	case types.ScopeRun:
		return 3
	case types.ScopeAlways:
		return 4
	}
	return 0
}

// capScopeToRule is the rule's max_scope over a decision's scope, for every decider. Under a rule the cap is
// at most run, so always (which persists the host onto the workspace beyond the run) is always refused. A
// decision sent with no scope keeps the kind's default unless an egress default (the run) reaches past the
// cap, when it is decided once. A wider explicit scope is refused (403 decision_scope_above_rule).
func (s *Server) capScopeToRule(w http.ResponseWriter, r *http.Request, id uuid.UUID, ap types.ApprovalRequest, v deciderVerdict, scope types.ApprovalScope) (types.ApprovalScope, bool) {
	if v.rule == nil {
		return scope, true
	}
	limit := types.ScopeRun
	if v.rule.MaxScope != "" {
		limit = v.rule.MaxScope
	}
	if scope == "" {
		if ap.Kind == types.ApprovalEgressDomain && scopeRank(limit) < scopeRank(types.ScopeRun) {
			return types.ScopeOnce, true
		}
		return scope, true
	}
	if scopeRank(limit) > 0 && scopeRank(scope) <= scopeRank(limit) {
		return scope, true
	}
	s.refuse(w, r, authz.Deny(authz.ReasonDecisionScopeAboveRule, id.String(), fmt.Sprintf(
		"the deciders of unit %s allow this %s request to be decided up to %q; %q reaches further", v.unit, v.kind, limit, scope)).
		OnRun(ap.RunID))
	return scope, false
}
