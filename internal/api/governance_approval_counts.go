// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// How many approvals each kind of held governance change needs, and whether one must come from a security
// admin: GET and PUT /governance/approval-counts. A count never goes below the deploy-time minimum, a count
// above the people who can approve a kind is refused for every caller, and an edit by a person is always held
// for the highest count in force, whatever WARDYN_GOVERNANCE_SECOND_HUMAN says. Who can approve is counted by
// the one pool engine of governance_change_pool.go; nothing here counts people on its own.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// approvalCountsKey is the target key of a held edit of the counts: there is one set of counts.
	approvalCountsKey = "approval_counts"
	// approvalCountsWriteAction is the audit action of a write of the counts.
	approvalCountsWriteAction = "governance.approval_counts.write"
	// approvalCountsMax is the highest count a kind can need.
	approvalCountsMax = 9
)

// Who approves a kind, as the page names it.
const (
	approversSuperAdmin  = "super_admin"
	approversSecurity    = "security"
	approversSecurityOrU = "security_or_unit_admin"
)

// Whether enough people can approve a kind now.
const (
	poolStatusOK          = "ok"
	poolStatusExact       = "exact"
	poolStatusShort       = "short"
	poolStatusCantConfirm = "cant_confirm"
)

// approvalCountsRule is a count and whether one approver must be a security admin.
type approvalCountsRule struct {
	Required        int  `json:"required"`
	RequireSecurity bool `json:"require_security"`
}

// approvalCountsWriteKind is one kind's own count in a write.
type approvalCountsWriteKind struct {
	Kind            string `json:"kind"`
	Required        int    `json:"required"`
	RequireSecurity bool   `json:"require_security"`
}

// approvalCountsWrite is the PUT body, a whole replace: a kind it does not list follows the default.
type approvalCountsWrite struct {
	Default approvalCountsRule        `json:"default"`
	Kinds   []approvalCountsWriteKind `json:"kinds"`
}

// approvalCountsPool is who can approve a kind now: count people, compared with the kind's count. UnitName
// names the unit a unit kind is counted in; Why says what cannot be counted when the status is cant_confirm.
type approvalCountsPool struct {
	Status   string `json:"status"`
	Count    int    `json:"count"`
	UnitName string `json:"unit_name,omitempty"`
	Why      string `json:"why,omitempty"`
}

// approvalCountsKind is one kind's row on the page. Own is false when the kind follows the default.
type approvalCountsKind struct {
	Kind            string             `json:"kind"`
	Own             bool               `json:"own"`
	Required        int                `json:"required"`
	RequireSecurity bool               `json:"require_security"`
	Approvers       string             `json:"approvers"`
	Pool            approvalCountsPool `json:"pool"`
}

// approvalCountsView is GET's answer, and the admin token's PUT's.
type approvalCountsView struct {
	Minimum int                  `json:"minimum"`
	Highest int                  `json:"highest"`
	Default approvalCountsRule   `json:"default"`
	Kinds   []approvalCountsKind `json:"kinds"`
}

// approvalCountKinds is every kind the page lists and a write may set, sorted: each kind of
// governanceChangeKinds except the ones that always have a single decider and the counts' own kind. It is
// derived, so a new held kind is listed with no edit here.
func approvalCountKinds() []string {
	var out []string
	for kind := range governanceChangeKinds {
		if singleDecider(kind) || kind == govKindApprovalCounts {
			continue
		}
		out = append(out, kind)
	}
	slices.Sort(out)
	return out
}

// isUnitCountKind reports the kinds a unit admin may also decide (unitChangeApprover), matched by name.
func isUnitCountKind(kind string) bool {
	switch kind {
	case govKindUnit, govKindMembership, govKindUnitAdmin, govKindUnitDeciders:
		return true
	}
	return false
}

// approversOf names who approves a kind by its table: a unit kind's security or unit admins, a super admin for a
// kind on the super-admin tier, else a security admin. It compares the approver's reason only.
func approversOf(kind string) string {
	if isUnitCountKind(kind) {
		return approversSecurityOrU
	}
	if governanceChangeKinds[kind].approver.reason == authz.ReasonAdminSurface {
		return approversSuperAdmin
	}
	return approversSecurity
}

// raisingProbe is, for a kind whose held changes forChange can move to the super-admin tier, one change it
// moves: a use policy naming a remote-hosted runner, or a unit change that sets a mount's host path. The probe
// names no proposer, so its pool leaves nobody out.
func raisingProbe(kind string) (types.GovernanceChange, bool) {
	switch kind {
	case govKindRunnerPoolUsePolicy:
		return types.GovernanceChange{TargetKind: kind, Payload: json.RawMessage(`{"remote_runner_id":"probe"}`)}, true
	case govKindUnit:
		payload, err := json.Marshal(map[string]any{"ceiling": map[string]any{
			"workspace_mounts": []types.WorkspaceMount{{Source: "/probe", Target: "/probe"}},
		}})
		if err != nil {
			return types.GovernanceChange{}, false
		}
		return types.GovernanceChange{TargetKind: kind, Payload: payload}, true
	}
	return types.GovernanceChange{}, false
}

// kindPool is one pool a kind is judged on: a unit's for a unit kind (named by unitName), the one pool of any
// other kind, or, raised, the pool of its changes that only a super admin decides.
type kindPool struct {
	res      poolResult
	unitName string
	raised   bool
}

// whoCanApprove counts who can approve kind on the loaded inputs: the raising probe's pool through
// poolForChange first, then every unit for a unit kind, else the kind's one pool (uuid.Nil).
func (s *Server) whoCanApprove(in *poolInputs, units []types.GovernanceUnit, kind string) ([]kindPool, error) {
	var out []kindPool
	if probe, ok := raisingProbe(kind); ok {
		res, err := s.poolForChange(in, probe, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, kindPool{res: res, raised: true})
	}
	if !isUnitCountKind(kind) || len(units) == 0 {
		res, err := s.poolForKind(in, kind, uuid.Nil)
		if err != nil {
			return nil, err
		}
		return append(out, kindPool{res: res}), nil
	}
	for _, u := range units {
		res, err := s.poolForKind(in, kind, u.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, kindPool{res: res, unitName: u.Name})
	}
	return out, nil
}

// poolStatus compares the people who can decide a change of the kind with its count. The proposer of a change
// is in their own kind's pool and never decides it, so Count is the pool less one: ok with someone to spare,
// exact, else cant_confirm when someone cannot be counted and short when everyone was.
func poolStatus(res poolResult, required int) approvalCountsPool {
	p := approvalCountsPool{Count: max(len(res.Eligible)-1, 0)}
	switch {
	case p.Count > required:
		p.Status = poolStatusOK
	case p.Count == required:
		p.Status = poolStatusExact
	case len(res.Uncountable) > 0:
		p.Status, p.Why = poolStatusCantConfirm, res.Uncountable[0].Why
	default:
		p.Status = poolStatusShort
	}
	return p
}

// poolRank orders the statuses from the best to the worst.
var poolRank = map[string]int{poolStatusOK: 0, poolStatusExact: 1, poolStatusCantConfirm: 2, poolStatusShort: 3}

// worstPool is the pool a kind is shown and judged on, and whether it is the raised one: the worst status
// (short, then cant_confirm, then exact, then ok), the fewer people on a tie, then the first, so the raised
// pool wins a full tie.
func worstPool(pools []kindPool, required int) (approvalCountsPool, bool) {
	var worst approvalCountsPool
	var raised bool
	for i, kp := range pools {
		p := poolStatus(kp.res, required)
		p.UnitName = kp.unitName
		if r, w := poolRank[p.Status], poolRank[worst.Status]; i == 0 || r > w || r == w && p.Count < worst.Count {
			worst, raised = p, kp.raised
		}
	}
	return worst, raised
}

// approvalCountsDefault is the default row, or the minimum (at least 1) with no security rule.
func approvalCountsDefault(rows []store.GovernanceApprovalCount, minimum int) approvalCountsRule {
	for _, r := range rows {
		if r.Kind == store.GovernanceApprovalDefaultKind {
			return approvalCountsRule{Required: r.Required, RequireSecurity: r.RequireSecurity}
		}
	}
	return approvalCountsRule{Required: max(1, minimum)}
}

// approvalCountsState is the stored counts as a write states them: the default and every kind's own row.
func approvalCountsState(rows []store.GovernanceApprovalCount, minimum int) approvalCountsWrite {
	out := approvalCountsWrite{Default: approvalCountsDefault(rows, minimum), Kinds: []approvalCountsWriteKind{}}
	for _, r := range rows {
		if r.Kind != store.GovernanceApprovalDefaultKind {
			out.Kinds = append(out.Kinds, approvalCountsWriteKind{Kind: r.Kind, Required: r.Required, RequireSecurity: r.RequireSecurity})
		}
	}
	return out
}

// storedApprovalCounts is the rows as stored, the default row included and nothing derived from the minimum:
// its hash is what an approval of a held edit compares, so a change of the minimum alone never makes one stale.
func storedApprovalCounts(rows []store.GovernanceApprovalCount) []approvalCountsWriteKind {
	out := make([]approvalCountsWriteKind, len(rows))
	for i, r := range rows {
		out[i] = approvalCountsWriteKind{Kind: r.Kind, Required: r.Required, RequireSecurity: r.RequireSecurity}
	}
	return out
}

// highestOf is the highest count in force: the minimum, every kind's own count and the default.
func highestOf(c approvalCountsWrite, minimum int) int {
	h := max(minimum, c.Default.Required)
	for _, k := range c.Kinds {
		h = max(h, k.Required)
	}
	return h
}

// kindCountOf is the count c gives kind: its own row, else the default, and whether it has its own row.
func kindCountOf(c approvalCountsWrite, kind string) (approvalCountsRule, bool) {
	for _, k := range c.Kinds {
		if k.Kind == kind {
			return approvalCountsRule{Required: k.Required, RequireSecurity: k.RequireSecurity}, true
		}
	}
	return c.Default, false
}

// approvalCountsEditRequired is how many approvals a held edit of the counts needs: the highest count in
// force when it is proposed. Every approver of the edit is already a security admin, so it needs no security
// rule of its own.
func (s *Server) approvalCountsEditRequired(ctx context.Context) (required int, requireSecurity bool, err error) {
	minimum := s.GovernanceMinApprovals()
	var rows []store.GovernanceApprovalCount
	if _, ok := s.cfg.Store.(store.GovernanceApprovalStore); ok {
		err = s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) (rerr error) {
			rows, rerr = store.ListGovernanceApprovalCountsQ(ctx, q)
			return rerr
		})
		if err != nil {
			return 0, false, err
		}
	}
	return highestOf(approvalCountsState(rows, minimum), minimum), false, nil
}

// approvalCountsReads is what GET and PUT read on one rolled-back transaction: the stored counts, the pool
// inputs and the units.
type approvalCountsReads struct {
	rows  []store.GovernanceApprovalCount
	in    *poolInputs
	units []types.GovernanceUnit
}

func (s *Server) readApprovalCounts(ctx context.Context, fn func(rd approvalCountsReads) error) error {
	return s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) error {
		var rd approvalCountsReads
		var err error
		if rd.rows, err = store.ListGovernanceApprovalCountsQ(ctx, q); err != nil {
			return err
		}
		if rd.in, err = s.loadPoolInputs(ctx, q); err != nil {
			return err
		}
		if rd.units, err = store.ListGovernanceUnitsQ(ctx, q); err != nil {
			return err
		}
		return fn(rd)
	})
}

// approvalCountsViewOf is the page: the counts as stored, and for each kind who can approve it now, judged on
// the count in force, the minimum when that is higher than the row.
func (s *Server) approvalCountsViewOf(rd approvalCountsReads) (approvalCountsView, error) {
	minimum := s.GovernanceMinApprovals()
	state := approvalCountsState(rd.rows, minimum)
	view := approvalCountsView{Minimum: minimum, Highest: highestOf(state, minimum), Default: state.Default, Kinds: []approvalCountsKind{}}
	for _, kind := range approvalCountKinds() {
		rule, own := kindCountOf(state, kind)
		pools, err := s.whoCanApprove(rd.in, rd.units, kind)
		if err != nil {
			return approvalCountsView{}, err
		}
		pool, raised := worstPool(pools, max(minimum, rule.Required))
		row := approvalCountsKind{Kind: kind, Own: own, Required: rule.Required, RequireSecurity: rule.RequireSecurity,
			Approvers: approversOf(kind), Pool: pool}
		if raised {
			row.Approvers = approversSuperAdmin
		}
		view.Kinds = append(view.Kinds, row)
	}
	return view, nil
}

func (s *Server) approvalCountsRead(ctx context.Context) (approvalCountsView, error) {
	view := approvalCountsView{Kinds: []approvalCountsKind{}}
	err := s.readApprovalCounts(ctx, func(rd approvalCountsReads) (err error) {
		view, err = s.approvalCountsViewOf(rd)
		return err
	})
	return view, err
}

// handleGetApprovalCounts answers how many approvals each kind needs and who can approve it now.
func (s *Server) handleGetApprovalCounts(w http.ResponseWriter, r *http.Request) {
	view, err := s.approvalCountsRead(r.Context())
	if err != nil {
		writeServerError(w, r, "read the approval counts", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// kindLabel is a kind's name as a sentence starts it: role_mapping is "Role mapping".
func kindLabel(kind string) string {
	label := strings.ReplaceAll(kind, "_", " ")
	if label == "" {
		return label
	}
	return strings.ToUpper(label[:1]) + label[1:]
}

// approvalCountsInvalid is the 400's sentence for a write, "" when every kind is one the page lists, once,
// and every count is in range.
func (s *Server) approvalCountsInvalid(c approvalCountsWrite) string {
	listed := approvalCountKinds()
	seen := map[string]bool{}
	for _, k := range c.Kinds {
		switch {
		case k.Kind == govKindApprovalCounts:
			return "The approval counts take no count of their own: an edit to them needs the highest count in force."
		case singleDecider(k.Kind):
			return kindLabel(k.Kind) + " always has one decider and takes no count."
		case !slices.Contains(listed, k.Kind):
			return fmt.Sprintf("No kind %q has a count.", k.Kind)
		case seen[k.Kind]:
			return kindLabel(k.Kind) + " is listed twice."
		}
		seen[k.Kind] = true
	}
	return approvalCountsOutOfRange(c, s.GovernanceMinApprovals())
}

// approvalCountsOutOfRange is the 400's sentence for a count below the minimum (or 1) or above the most a kind
// can need, "" when there is none. An approval re-checks a held edit with it against the minimum then in force;
// the kinds were checked at proposal.
func approvalCountsOutOfRange(c approvalCountsWrite, minimum int) string {
	lo := max(1, minimum)
	if n := c.Default.Required; n < lo || n > approvalCountsMax {
		return fmt.Sprintf("The default count must be from %d to %d.", lo, approvalCountsMax)
	}
	for _, k := range c.Kinds {
		if k.Required < lo || k.Required > approvalCountsMax {
			return fmt.Sprintf("%s needs a count from %d to %d.", kindLabel(k.Kind), lo, approvalCountsMax)
		}
	}
	return ""
}

// countRefusals is the 422's sentences for a write c over the counts before it, in kind order. For a PUT
// (recheck false) it judges each kind c lists and each kind that follows a default above the minimum: refused
// when any of its pools is short, and, when c raises the kind, when one cannot be counted in full. For an
// approval of a held edit (recheck true) it judges only the kinds c raises, and only for short.
func (s *Server) countRefusals(in *poolInputs, units []types.GovernanceUnit, before, c approvalCountsWrite, minimum int, recheck bool) ([]string, error) {
	var out []string
	for _, kind := range approvalCountKinds() {
		rule, own := kindCountOf(c, kind)
		was, _ := kindCountOf(before, kind)
		raises := rule.Required > max(minimum, was.Required)
		if recheck && !raises || !own && rule.Required <= minimum {
			continue
		}
		pools, err := s.whoCanApprove(in, units, kind)
		if err != nil {
			return nil, err
		}
		p, raised := worstPool(pools, rule.Required)
		what := "it"
		switch {
		case raised:
			what = "its changes that need a super admin"
		case p.UnitName != "":
			what = fmt.Sprintf("it in the unit %q", p.UnitName)
		}
		switch {
		case p.Status == poolStatusShort:
			out = append(out, fmt.Sprintf("%s needs %d approvals and only %s besides the proposer can approve %s.",
				kindLabel(kind), rule.Required, peopleCount(p.Count), what))
		case p.Status == poolStatusCantConfirm && raises && !recheck:
			out = append(out, fmt.Sprintf("%s can't be raised to %d: only %s besides the proposer can be counted to approve %s, and someone who might can't be counted.",
				kindLabel(kind), rule.Required, peopleCount(p.Count), what))
		}
	}
	return out, nil
}

// peopleCount is n people in words: "1 person", "3 people".
func peopleCount(n int) string {
	if n == 1 {
		return "1 person"
	}
	return fmt.Sprintf("%d people", n)
}

// approvalCountsUnreachable is the 422's sentence for a write, "" when it can be saved: countRefusals over the
// counts stored now, and a write after which an edit to the counts could not pass: the security admins and
// super admins, less the proposer, below the new highest count, everyone counted.
func (s *Server) approvalCountsUnreachable(rd approvalCountsReads, c approvalCountsWrite) (string, error) {
	minimum := s.GovernanceMinApprovals()
	short, err := s.countRefusals(rd.in, rd.units, approvalCountsState(rd.rows, minimum), c, minimum, false)
	if err != nil {
		return "", err
	}
	edits, err := s.poolForKind(rd.in, govKindApprovalCounts, uuid.Nil)
	if err != nil {
		return "", err
	}
	if highest, others := highestOf(c, minimum), len(edits.Eligible)-1; others < highest && len(edits.Uncountable) == 0 {
		short = append(short, fmt.Sprintf("Edits to these counts would need %d approvals and only %d security admins besides the proposer could give them.",
			highest, max(others, 0)))
	}
	return strings.Join(short, " "), nil
}

// approvalCountsRows is a write as the rows it stores: the default row, then each kind's own.
func approvalCountsRows(c approvalCountsWrite) []store.GovernanceApprovalCount {
	rows := []store.GovernanceApprovalCount{{Kind: store.GovernanceApprovalDefaultKind, Required: c.Default.Required, RequireSecurity: c.Default.RequireSecurity}}
	for _, k := range c.Kinds {
		rows = append(rows, store.GovernanceApprovalCount{Kind: k.Kind, Required: k.Required, RequireSecurity: k.RequireSecurity})
	}
	return rows
}

func approvalCountsAuditData(minimum int, c approvalCountsWrite) map[string]any {
	return map[string]any{"minimum": minimum, "default": c.Default, "kinds": c.Kinds}
}

// handlePutApprovalCounts replaces the counts. In order: the minimum must be set (409), local mode is refused
// (503), the body must be valid (400) and reachable (422), every caller included; then the admin token writes
// directly, recorded as a bypass, and every other caller's edit is held for the highest count in force. The
// second-human switch plays no part.
func (s *Server) handlePutApprovalCounts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	minimum := s.GovernanceMinApprovals()
	if minimum <= 0 {
		writeErrorReason(w, http.StatusConflict, reasonGovernanceApprovalCountsMinimumUnset,
			"Approval counts need WARDYN_GOVERNANCE_MIN_APPROVALS: set it from 1 to 9 before setting a count.")
		return
	}
	if s.cfg.LocalMode {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonGovernanceApprovalCountsLocalMode,
			"Approval counts can't be enforced in local mode: local mode authenticates nobody, so no edit can be held for distinct people. Configure SSO, or unset WARDYN_GOVERNANCE_MIN_APPROVALS.")
		return
	}
	var body approvalCountsWrite
	if msg := decodeStrictMsg(w, r, &body); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernanceApprovalCountsInvalid, "Approval counts: "+msg)
		return
	}
	if msg := s.approvalCountsInvalid(body); msg != "" {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernanceApprovalCountsInvalid, msg)
		return
	}
	if body.Kinds == nil {
		body.Kinds = []approvalCountsWriteKind{}
	}
	slices.SortFunc(body.Kinds, func(a, b approvalCountsWriteKind) int { return strings.Compare(a.Kind, b.Kind) })
	var before approvalCountsWrite
	var base []approvalCountsWriteKind
	var refusal string
	err := s.readApprovalCounts(ctx, func(rd approvalCountsReads) (err error) {
		before, base = approvalCountsState(rd.rows, minimum), storedApprovalCounts(rd.rows)
		refusal, err = s.approvalCountsUnreachable(rd, body)
		return err
	})
	if err != nil {
		writeServerError(w, r, "read the approval counts", err)
		return
	}
	if refusal != "" {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonGovernanceApprovalCountsUnreachable, refusal)
		return
	}
	if !isAdminTokenCaller(r) {
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindApprovalCounts, op: "set", key: approvalCountsKey, payload: body,
			before: before, after: body, changed: changedPaths(before, body), baseState: base,
		})
		return
	}
	st, ok := s.cfg.Store.(store.GovernanceApprovalStore)
	if !ok {
		writeServerError(w, r, "write the approval counts", fmt.Errorf("this store keeps no approval counts"))
		return
	}
	if err := st.ReplaceGovernanceApprovalCounts(ctx, approvalCountsRows(body)); err != nil {
		writeServerError(w, r, "write the approval counts", err)
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		approvalCountsWriteAction, approvalCountsKey, "success", mustJSON(approvalCountsAuditData(minimum, body))))
	s.recordGovernanceBypass(r, govKindApprovalCounts, approvalCountsKey, "success", nil)
	view, err := s.approvalCountsRead(ctx)
	if err != nil {
		writeServerError(w, r, "read the approval counts", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// applyApprovalCountsChange applies a held edit inside the decision transaction: the target lock, the counts as
// they now stand compared with what the proposal reviewed, the counts re-checked against the minimum now, each
// kind the edit raises re-checked against the people now, then the write on q. A refusal leaves the edit held.
func applyApprovalCountsChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var held approvalCountsWrite
	if err := decodeHeldPayload(ch.Payload, &held); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindApprovalCounts); err != nil {
		return govApplied{}, err
	}
	rows, err := store.ListGovernanceApprovalCountsQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(storedApprovalCounts(rows)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	minimum := s.GovernanceMinApprovals()
	if msg := approvalCountsOutOfRange(held, minimum); msg != "" {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonGovernanceApprovalCountsInvalid, "%s", msg)
	}
	msg, err := recheckRaisedCounts(s, ctx, q, approvalCountsState(rows, minimum), held, minimum)
	if err != nil {
		return govApplied{}, err
	}
	if msg != "" {
		return govApplied{}, writeRefusal(http.StatusUnprocessableEntity, reasonGovernanceApprovalCountsUnreachable, "%s", msg)
	}
	if err := store.ReplaceGovernanceApprovalCountsQ(ctx, q, approvalCountsRows(held)); err != nil {
		return govApplied{}, err
	}
	return govApplied{action: approvalCountsWriteAction, target: approvalCountsKey, data: approvalCountsAuditData(minimum, held)}, nil
}

// recheckRaisedCounts is an approval's re-check of a held edit: each kind it raises over the counts now stored,
// judged on the people now. It is set in init because it reads the kinds table, whose row for the counts names
// applyApprovalCountsChange.
var recheckRaisedCounts func(s *Server, ctx context.Context, q store.Querier, before, held approvalCountsWrite, minimum int) (string, error)

func init() { recheckRaisedCounts = (*Server).recheckRaised }

func (s *Server) recheckRaised(ctx context.Context, q store.Querier, before, held approvalCountsWrite, minimum int) (string, error) {
	in, err := s.loadPoolInputs(ctx, q)
	if err != nil {
		return "", err
	}
	in.base = withUnitScopeTx(in.base, q) // the unit scope is read on the decision's transaction too
	units, err := store.ListGovernanceUnitsQ(ctx, q)
	if err != nil {
		return "", err
	}
	short, err := s.countRefusals(in, units, before, held, minimum, true)
	return strings.Join(short, " "), err
}
