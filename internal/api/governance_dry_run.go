// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// POST /governance/dry-run for the policy tree: what a unit edit or a membership move would change,
// person by person, computed by applying the write in a transaction that is rolled back. The boundary
// target is boundary.go's.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The dry-run targets. A body with no target is a boundary dry run, as every body was before.
const (
	dryRunTargetBoundary       = "boundary"
	dryRunTargetUnit           = "unit"
	dryRunTargetMembershipMove = "membership_move"
)

// dryRunTarget is the one field every dry-run body may carry.
type dryRunTarget struct {
	Target string `json:"target"`
}

// unitDryRunRequest is a unit target: the unit write body (POST and PUT /governance/units) and the
// unit's id (none: a new unit).
type unitDryRunRequest struct {
	dryRunTarget
	ID uuid.UUID `json:"id"`
	governanceUnitRequest
}

// membershipMoveDryRunRequest is a membership_move target: PUT /governance/memberships/{id}'s body
// and the membership's id.
type membershipMoveDryRunRequest struct {
	dryRunTarget
	ID     uuid.UUID `json:"id"`
	UnitID uuid.UUID `json:"unit_id"`
}

// treeDryRunReport is a unit or membership_move dry run's answer: each membership whose context's
// walls would change or that would move, and the live runs in a unit whose walls change or in the
// unit a moved membership leaves.
type treeDryRunReport struct {
	Target   string             `json:"target"`
	People   []treeDryRunPerson `json:"people"`
	LiveRuns []uuid.UUID        `json:"live_runs"`
	// Approvers is who could decide the write were it held, the caller left out (approversFor).
	Approvers []changeApprover `json:"approvers"`
}

// treeDryRunPerson is one membership, the context it is (from_unit) and would be (unit), and the knobs
// that differ between the two contexts' walls. Error names a context that would not compose.
type treeDryRunPerson struct {
	MembershipID uuid.UUID                   `json:"membership_id"`
	SubjectType  types.CapabilitySubjectType `json:"subject_type"`
	Subject      string                      `json:"subject"`
	FromUnitID   uuid.UUID                   `json:"from_unit_id"`
	FromUnitName string                      `json:"from_unit_name"`
	UnitID       uuid.UUID                   `json:"unit_id"`
	UnitName     string                      `json:"unit_name"`
	Changes      []boundaryChange            `json:"changes"`
	Error        string                      `json:"error,omitempty"`
}

// treeDryRun answers a unit or membership_move body; boundary.go's handler hands it every target but
// the boundary.
func (s *Server) treeDryRun(w http.ResponseWriter, r *http.Request, target string, raw []byte) {
	var apply func(q store.Querier) error
	// touched is the units an approver of the write would have to govern (unitChangeScope's rule), from
	// the states either side of it; none means only a security admin could decide it.
	var touched func(before, after treeSnapshot) []uuid.UUID
	switch target {
	case dryRunTargetUnit:
		var req unitDryRunRequest
		if msg := strictJSON(raw, &req); msg != "" {
			writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, msg)
			return
		}
		unit, msg := finishGovernanceUnitRequest(req.governanceUnitRequest)
		if msg != "" {
			writeErrorReason(w, http.StatusBadRequest, repoURLMsgReason(msg, reasonGovernanceUnitRequestInvalid), msg)
			return
		}
		if msg := unit.parseComposition(); msg != "" {
			writeErrorReason(w, http.StatusBadRequest, reasonGovernanceOverlayInvalid, msg)
			return
		}
		limit, err := s.unitDepthLimit(r.Context())
		if err != nil {
			writeServerError(w, r, "governance dry run", err)
			return
		}
		id, status := req.ID, http.StatusOK
		if id == uuid.Nil {
			id, status = uuid.New(), http.StatusCreated
		}
		if s.isUnitAdminCaller(r.Context()) && !s.unitWritePrecheck(w, r, id, status, unit) {
			return
		}
		apply = func(q store.Querier) error { return s.applyUnitWriteQ(r, q, id, status, unit, limit) }
		touched = func(before, after treeSnapshot) []uuid.UUID {
			was, existed := unitsByID(before.units)[id]
			now := unitsByID(after.units)[id]
			units := []uuid.UUID{}
			if existed {
				units = append(units, id)
				if sameJSON(was.ParentID, now.ParentID) {
					return units
				}
			}
			if now.ParentID == nil {
				return nil
			}
			return append(units, *now.ParentID)
		}
	case dryRunTargetMembershipMove:
		var req membershipMoveDryRunRequest
		if msg := strictJSON(raw, &req); msg != "" {
			writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, msg)
			return
		}
		apply = func(q store.Querier) error {
			if row, err := store.GetGovernanceMembershipQ(r.Context(), q, req.ID, false); err == nil {
				if err := s.requireUnitScopeQ(r.Context(), q, false, row.UnitID, req.UnitID); err != nil {
					return err
				}
			}
			_, _, err := moveMembershipQ(r.Context(), q, req.ID, req.UnitID)
			return err
		}
		touched = func(before, _ treeSnapshot) []uuid.UUID {
			i := slices.IndexFunc(before.memberships, func(m types.GovernanceMembership) bool { return m.ID == req.ID })
			if i < 0 {
				return nil
			}
			return []uuid.UUID{before.memberships[i].UnitID, req.UnitID}
		}
	default:
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody,
			fmt.Sprintf("target: %q is not a dry-run target — use boundary, unit or membership_move", target))
		return
	}
	var before, after treeSnapshot
	err := s.cfg.Store.DryRunGovernance(r.Context(), func(q store.Querier) (err error) {
		if before, err = readTreeSnapshot(r.Context(), q); err != nil {
			return err
		}
		if err = apply(q); err != nil {
			return err
		}
		after, err = readTreeSnapshot(r.Context(), q)
		return err
	})
	if s.answerOutsideScope(w, r, "governance.dry_run", err) || writeMembershipTreeError(w, r, "governance dry run", err) {
		return
	}
	report, err := s.treeEffect(r.Context(), before, after)
	if err != nil {
		writeServerError(w, r, "governance dry run", err)
		return
	}
	report.Target = target
	report.Approvers = []changeApprover{{Kind: "security"}}
	if units := touched(before, after); len(units) > 0 {
		report.Approvers = s.approversFor(r.Context(), units, false, principalFromRequest(r), oidcEmailFromContext(r.Context()))
	}
	writeJSON(w, http.StatusOK, report)
}

// strictJSON decodes raw into dst as decodeStrict decodes a body, returning the message it would.
func strictJSON(raw []byte, dst any) string {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return "invalid JSON body: " + err.Error()
	}
	return ""
}

// applyUnitWriteQ applies a unit write on q with the refusals and the build of the write itself
// (writeGovernanceUnit) and of its approval (applyUnitChange): the Default unit's 409, the PUT-create
// rule, buildGovernanceUnit and WriteGovernanceUnitQ.
func (s *Server) applyUnitWriteQ(r *http.Request, q store.Querier, id uuid.UUID, status int, req governanceUnitRequest, limit int) error {
	if refusal := defaultUnitWriteRefusal(id); refusal != nil {
		return refusal
	}
	b, err := s.boundaryQ(r.Context(), q)
	if err != nil {
		return err
	}
	_, err = store.WriteGovernanceUnitQ(r.Context(), q, id, s.scopedUnitBuild(r.Context(), q, id, func(all []types.GovernanceUnit, _ []types.GovernanceMembership) (types.GovernanceUnit, error) {
		if refusal := s.unitPutCreateRefusal(r, id, status, all); refusal != nil {
			return types.GovernanceUnit{}, refusal
		}
		p, _, err := s.buildGovernanceUnit(principalFromRequest(r), id, req, all, b, limit)
		return p, err
	}))
	if errors.Is(err, store.ErrConflict) {
		return writeRefusal(http.StatusConflict, reasonGovernanceUnitNameConflict, "a governance profile named %q already exists", req.Name)
	}
	return err
}

// treeSnapshot is the unit tree and its memberships as one transaction reads them.
type treeSnapshot struct {
	units       []types.GovernanceUnit
	memberships []types.GovernanceMembership
}

func readTreeSnapshot(ctx context.Context, q store.Querier) (treeSnapshot, error) {
	units, err := store.ListGovernanceUnitsQ(ctx, q)
	if err != nil {
		return treeSnapshot{}, err
	}
	ms, err := store.ListGovernanceMembershipsQ(ctx, q)
	return treeSnapshot{units: units, memberships: ms}, err
}

// unitWalls is one context's walls as a run in it resolves them, or the error it resolves to.
type unitWalls struct {
	spec   types.RunPolicySpec
	limits types.GovernanceLimits
	err    string
}

// snapshotWalls resolves every unit of snap as the context resolve does once it has picked the unit:
// the Default unit is the deployment ceiling, any other unit its path composed from the top
// (composeAll, the composition GET /governance shows), then ceilingFromUnit and the boundary.
func (s *Server) snapshotWalls(ctx context.Context, snap treeSnapshot) (map[uuid.UUID]unitWalls, error) {
	limit, err := s.unitDepthLimit(ctx)
	if err != nil {
		return nil, err
	}
	ctx = withDisplayRead(ctx)
	deployment := governanceCeiling{Spec: s.cfg.DefaultPolicy.Clone(), MemberDefault: s.memberDefaultFor(s.cfg.DefaultPolicy.Resources)}
	out := map[uuid.UUID]unitWalls{}
	for _, res := range s.composeAll(snap.units, limit) {
		c := deployment
		var err error
		if res.Row.ID != store.DefaultUnitID {
			c, err = s.ceilingFromUnit(ctx, res.Resolved, res.Err, deployment)
		}
		if err == nil {
			c, err = s.boundaryCeiling(ctx, c)
		}
		if err != nil {
			out[res.Row.ID] = unitWalls{err: adminResolveError(err)}
			continue
		}
		out[res.Row.ID] = unitWalls{spec: redactSpecForRead(c.Spec, true), limits: c.Limits}
	}
	return out, nil
}

// wallChanges is the knobs that differ between two contexts' walls: the policy's, then every limit.
func wallChanges(b, a unitWalls) []boundaryChange {
	out := boundaryChanges(b.spec, a.spec, types.GovernanceLimits{}, types.GovernanceLimits{})
	var bl, al any
	roundTripJSON(b.limits, &bl)
	roundTripJSON(a.limits, &al)
	for _, p := range changedPaths(b.limits, a.limits) {
		out = append(out, boundaryChange{Knob: "limits." + p, Before: jsonAt(bl, p), After: jsonAt(al, p)})
	}
	if out == nil {
		out = []boundaryChange{}
	}
	return out
}

// jsonAt is the value at a dotted path of a decoded JSON value, nil where there is none.
func jsonAt(v any, path string) any {
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// treeEffect is what a write that turned before into after changes: the per-membership report and the
// live runs it touches. The dry run computes it over the rolled-back write; it is the same function
// over the states either side of an applied write.
func (s *Server) treeEffect(ctx context.Context, before, after treeSnapshot) (treeDryRunReport, error) {
	bw, err := s.snapshotWalls(ctx, before)
	if err != nil {
		return treeDryRunReport{}, err
	}
	aw, err := s.snapshotWalls(ctx, after)
	if err != nil {
		return treeDryRunReport{}, err
	}
	names := map[uuid.UUID]string{}
	for _, u := range append(slices.Clone(before.units), after.units...) {
		names[u.ID] = u.Name
	}
	wallsChanged := map[uuid.UUID]bool{}
	for id, b := range bw {
		if a, ok := aw[id]; ok && (b.err != a.err || len(wallChanges(b, a)) > 0) {
			wallsChanged[id] = true
		}
	}
	was := map[uuid.UUID]types.GovernanceMembership{}
	for _, m := range before.memberships {
		was[m.ID] = m
	}
	report := treeDryRunReport{People: []treeDryRunPerson{}, LiveRuns: []uuid.UUID{}}
	left := map[uuid.UUID][]types.GovernanceMembership{} // the unit a moved membership leaves → the membership
	for _, m := range after.memberships {
		prev, ok := was[m.ID]
		if !ok {
			continue
		}
		b, a := bw[prev.UnitID], aw[m.UnitID]
		changes := wallChanges(b, a)
		if prev.UnitID == m.UnitID && b.err == a.err && len(changes) == 0 {
			continue
		}
		if prev.UnitID != m.UnitID {
			left[prev.UnitID] = append(left[prev.UnitID], m)
		}
		report.People = append(report.People, treeDryRunPerson{
			MembershipID: m.ID, SubjectType: m.SubjectType, Subject: m.Subject,
			FromUnitID: prev.UnitID, FromUnitName: names[prev.UnitID], UnitID: m.UnitID, UnitName: names[m.UnitID],
			Changes: changes, Error: a.err,
		})
	}
	if len(wallsChanged) == 0 && len(left) == 0 {
		return report, nil
	}
	runs, err := s.liveRuns(ctx)
	if err != nil {
		return treeDryRunReport{}, err
	}
	for _, run := range runs {
		if run.UnitID != nil && (wallsChanged[*run.UnitID] || leftBy(left[*run.UnitID], run)) {
			report.LiveRuns = append(report.LiveRuns, run.ID)
		}
	}
	return report, nil
}

// leftBy reports whether run may be its owner's under one of the memberships that leave its unit: a
// user membership names its owner, and any other kind may match them.
func leftBy(moved []types.GovernanceMembership, run types.AgentRun) bool {
	return slices.ContainsFunc(moved, func(m types.GovernanceMembership) bool {
		return m.SubjectType != types.CapabilitySubjectUser || m.Subject == run.CreatedBy
	})
}
