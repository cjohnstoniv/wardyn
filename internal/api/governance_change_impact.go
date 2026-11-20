// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// What a change's impact computed, and why it computed nothing.
const (
	impactTree     = "tree"     // a profile write or a membership move replayed through the tree dry run
	impactBoundary = "boundary" // a boundary change met with every layer, as the boundary dry run does
	impactNone     = "none"     // nothing: not_computed says why

	impactKindNotComputed      = "kind"      // the kind has no dry run
	impactOperationNotComputed = "operation" // the dry run does not model the operation (treeModels)
	impactStaleNotComputed     = "stale"     // the target or the deployment default moved since the proposal
	impactRefusedNotComputed   = "refused"   // the change, replayed now, is refused
)

// maxImpactLiveRuns bounds the live runs an impact lists; live_runs_total counts them all.
const maxImpactLiveRuns = 50

// changeImpact is GET /governance/changes/{id}/impact's answer: what approving a pending change would do
// now, computed by the dry run of its kind. People, LiveRuns and LiveRunsTotal are set, as arrays, only
// for a tree impact; Boundary only for a boundary one.
type changeImpact struct {
	Computed      string              `json:"computed"`
	NotComputed   string              `json:"not_computed,omitempty"`
	AsOf          string              `json:"as_of"`
	People        *[]treeDryRunPerson `json:"people,omitempty"`
	LiveRuns      *[]changeImpactRun  `json:"live_runs,omitempty"`
	LiveRunsTotal *int                `json:"live_runs_total,omitempty"`
	Activates     []unitActivation    `json:"activates,omitempty"`
	Vacates       []unitActivation    `json:"vacates,omitempty"`
	Boundary      *boundaryReport     `json:"boundary,omitempty"`
}

// changeImpactRun is one live run an impact reaches: its title only for its owner or a security admin.
type changeImpactRun struct {
	ID        uuid.UUID `json:"id"`
	Owner     string    `json:"owner"`
	StartedAt time.Time `json:"started_at"`
	Title     string    `json:"title,omitempty"`
}

// impactCounts is an impact reduced to its counts (impactFor).
type impactCounts struct {
	Computed      string `json:"computed"`
	NotComputed   string `json:"not_computed,omitempty"`
	People        int    `json:"people"`
	LiveRunsTotal int    `json:"live_runs_total"`
	Activates     int    `json:"activates"`
	Vacates       int    `json:"vacates"`
}

// counts is out reduced to its counts.
func (out changeImpact) counts() impactCounts {
	c := impactCounts{Computed: out.Computed, NotComputed: out.NotComputed, Activates: len(out.Activates), Vacates: len(out.Vacates)}
	if out.People != nil {
		c.People = len(*out.People)
	}
	if out.LiveRunsTotal != nil {
		c.LiveRunsTotal = *out.LiveRunsTotal
	}
	return c
}

// impactApplyError is a replayed apply's refusal, told apart from a failed read around it.
type impactApplyError struct{ err error }

func (e *impactApplyError) Error() string { return e.err.Error() }
func (e *impactApplyError) Unwrap() error { return e.err }

// handleGetGovernanceChangeImpact answers what approving a pending change would do now. It reads the
// change as handleGetGovernanceChange does (the same hidden 404), refuses a change no longer pending,
// and replays it (changeImpactOf). It is a read: no audit row.
func (s *Server) handleGetGovernanceChangeImpact(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "governance change")
	if !ok {
		return
	}
	ch, err := s.cfg.Store.GetGovernanceChange(r.Context(), id)
	if err == nil && !s.canSeeGovernanceChange(r, ch) {
		err = store.ErrNotFound
	}
	if notFoundIf(w, err, "governance change", reasonGovernanceChangeNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read governance change", err)
		return
	}
	if ch.State != types.GovernanceChangePending {
		writeErrorReason(w, http.StatusConflict, reasonGovernanceChangeNotPending, "This change is no longer pending ("+ch.State+").")
		return
	}
	out, err := s.changeImpactOf(r, ch)
	if err != nil {
		writeServerError(w, r, "governance change impact", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// impactFor is the impact of a write of kind to targetKey, its body after as its route takes it, before
// anyone proposes it: the replay behind GET /governance/changes/{id}/impact (changeImpactOf), run on a
// change that stands for the write as it would be held now, reduced to counts. It takes no request; who
// reads is ctx's caller. A profile write is replayed; every other kind answers none/kind.
func (s *Server) impactFor(ctx context.Context, kind, targetKey string, after json.RawMessage) (impactCounts, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	if err != nil {
		return impactCounts{}, err
	}
	if kind != govKindUnit {
		return impactCounts{Computed: impactNone, NotComputed: impactKindNotComputed}, nil
	}
	ch, err := s.unitWriteAsHeld(ctx, targetKey, after)
	if err != nil {
		return impactCounts{}, err
	}
	out, err := s.changeImpactOf(r, ch)
	return out.counts(), err
}

// unitWriteAsHeld is the pending change a profile write would be held as now: its body as the payload,
// the profile graph's hash as the base an approval compares, and a rename's legacy shift as its diff.
func (s *Server) unitWriteAsHeld(ctx context.Context, targetKey string, after json.RawMessage) (types.GovernanceChange, error) {
	id, err := uuid.Parse(targetKey)
	if err != nil {
		return types.GovernanceChange{}, errors.New("target key " + targetKey + " is not a profile id")
	}
	req, msg := unitRequestFromPayload(after)
	if msg != "" {
		return types.GovernanceChange{}, errors.New(msg)
	}
	all, err := s.cfg.Store.ListGovernanceUnits(ctx)
	if err != nil {
		return types.GovernanceChange{}, err
	}
	op := "create"
	var shift legacyShift
	if cur, exists := unitsByID(all)[id]; exists {
		op = "update"
		if cur.Name != req.Name {
			if shift, err = s.renameShift(ctx, id, req.Name); err != nil {
				return types.GovernanceChange{}, err
			}
		}
	}
	base, _, _ := composedFrom(all, id, req.comp)
	_, proposer := actorFromContext(ctx, "")
	return types.GovernanceChange{
		TargetKind: govKindUnit, Op: op, TargetKey: id.String(), Payload: after,
		Diff:           renderGovernanceDiff(nil, nil, nil, nil, heldReads{shift: shift}),
		BaseHash:       computeETag(unitBaseState(all, id, base)),
		DeploymentHash: computeETag(s.cfg.DefaultPolicy),
		ProposedBy:     proposer,
	}, nil
}

// changeImpactOf replays held change ch, as of now: a profile write or a membership move through its
// own apply between two reads of the tree (treeImpact), a boundary change met with every layer
// (boundaryImpact). A change whose deployment default moved is stale, as its approval would be.
func (s *Server) changeImpactOf(r *http.Request, ch types.GovernanceChange) (changeImpact, error) {
	out := changeImpact{Computed: impactNone, AsOf: time.Now().UTC().Format(time.RFC3339)}
	var err error
	switch {
	case ch.TargetKind != govKindUnit && ch.TargetKind != govKindMembership && ch.TargetKind != govKindBoundary:
		out.NotComputed = impactKindNotComputed
	case ch.DeploymentHash != computeETag(s.cfg.DefaultPolicy):
		out.NotComputed = impactStaleNotComputed
	case ch.TargetKind == govKindBoundary:
		err = s.boundaryImpact(r.Context(), ch, &out)
	case !treeModels(ch):
		out.NotComputed = impactOperationNotComputed
	default:
		err = s.treeImpact(r, ch, &out)
	}
	return out, err
}

// treeModels reports whether the tree dry run models held change ch: a unit's profile created or
// updated, or a membership moved. It compares the memberships both reads of the tree hold, so a
// membership added, removed or promoted, or a unit deleted, would answer an empty impact that is false.
func treeModels(ch types.GovernanceChange) bool {
	switch ch.TargetKind {
	case govKindUnit:
		return ch.Op == "create" || ch.Op == "update"
	case govKindMembership:
		return ch.Op == "move"
	}
	return false
}

// treeImpact replays a profile or membership change through its own apply between two reads of the
// tree, as the tree dry run does, on a transaction that is always rolled back, and reports the people
// whose walls change and the live runs reached. A refusal of the replay is answered, never written.
func (s *Server) treeImpact(r *http.Request, ch types.GovernanceChange, out *changeImpact) error {
	ctx := r.Context()
	kind := governanceChangeKinds[ch.TargetKind].forChange(ch)
	var before, after treeSnapshot
	err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) (err error) {
		if before, err = readTreeSnapshot(ctx, q); err != nil {
			return err
		}
		if _, err = kind.apply(s, r, q, ch); err != nil {
			return &impactApplyError{err}
		}
		after, err = readTreeSnapshot(ctx, q)
		return err
	})
	var refused *impactApplyError
	switch {
	case errors.Is(err, store.ErrGovernanceChangeStale):
		out.NotComputed = impactStaleNotComputed
		return nil
	case errors.As(err, &refused):
		out.NotComputed = impactRefusedNotComputed
		return nil
	case err != nil:
		return err
	}
	report, err := s.treeEffect(ctx, before, after)
	if err != nil {
		return err
	}
	listed := min(len(report.liveRunRows), maxImpactLiveRuns)
	runs := make([]changeImpactRun, 0, listed)
	for _, run := range report.liveRunRows[:listed] {
		row := changeImpactRun{ID: run.ID, Owner: run.CreatedBy, StartedAt: run.CreatedAt}
		if s.ownsRunOrAdmin(r, run) {
			row.Title = run.Title
		}
		runs = append(runs, row)
	}
	total := len(report.liveRunRows)
	out.Computed, out.People, out.LiveRuns, out.LiveRunsTotal = impactTree, &report.People, &runs, &total
	// The legacy shift the change was held with: the replay refuses as stale when it no longer holds.
	var held governanceDiffBody
	if json.Unmarshal(ch.Diff, &held) == nil {
		out.Activates, out.Vacates = held.Activates, held.Vacates
	}
	return nil
}

// boundaryImpact reads the boundary as applyBoundaryChange does, under its target lock on a transaction
// that is rolled back, and meets every layer with the boundary the approval would write.
func (s *Server) boundaryImpact(ctx context.Context, ch types.GovernanceChange, out *changeImpact) error {
	var held types.BoundaryWrite
	if decodeHeldPayload(ch.Payload, &held) != nil {
		out.NotComputed = impactRefusedNotComputed
		return nil
	}
	var next types.Boundary
	var refusal error
	err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) error {
		if err := store.LockGovernanceTarget(ctx, q, govKindBoundary); err != nil {
			return err
		}
		cur, err := store.GetBoundaryQ(ctx, q)
		if err != nil {
			return err
		}
		refusal = boundaryReplayRefusal(&held, cur, ch.BaseHash)
		next = types.Boundary{Mode: held.Mode, Spec: held.Spec, Limits: held.Limits, Contact: held.Contact, LockedKnobs: cur.LockedKnobs}
		if held.LockedKnobs != nil {
			next.LockedKnobs = *held.LockedKnobs
		}
		return nil
	})
	switch {
	case err != nil:
		return err
	case errors.Is(refusal, store.ErrGovernanceChangeStale):
		out.NotComputed = impactStaleNotComputed
		return nil
	case refusal != nil:
		out.NotComputed = impactRefusedNotComputed
		return nil
	}
	report, err := s.boundaryDryRun(ctx, next)
	if err != nil {
		return err
	}
	report.Target = dryRunTargetBoundary
	out.Computed, out.Boundary = impactBoundary, &report
	return nil
}
