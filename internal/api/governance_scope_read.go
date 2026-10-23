// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unitParentWalls is what a unit admin reads of the walls above one of their subtrees: the effective
// ceiling and limits of the unit the subtree hangs from, and per knob the layer that set it (locked,
// and named for the locker, when a lock above fixes it).
type unitParentWalls struct {
	UnitID     uuid.UUID                       `json:"unit_id"`
	ParentID   uuid.UUID                       `json:"parent_id"`
	ParentName string                          `json:"parent_name"`
	Effective  governanceEffective             `json:"effective"`
	Provenance map[string]policyref.Provenance `json:"provenance"`
}

// scopeGovernanceRead narrows GET /governance to a unit admin's subtrees (DESIGN-TREE §5.1): their units,
// the memberships and grants on them, and the walls each subtree root hangs from. The boundary is left
// out; a caller whose grants have since gone sees nothing.
func (s *Server) scopeGovernanceRead(ctx context.Context, resp governanceResponse, resolved []unitResolution, b types.Boundary, limit int) (governanceResponse, error) {
	grants, err := s.callerUnitGrants(ctx)
	if err != nil {
		return governanceResponse{}, err
	}
	rows := make([]types.GovernanceUnit, len(resolved))
	for i, u := range resolved {
		rows[i] = u.Row
	}
	in, roots := subtreeUnits(rows, grants, limit)
	out := governanceResponse{MaxDepth: resp.MaxDepth, Units: []governanceUnitView{},
		Memberships: []types.GovernanceMembership{}, UnitAdmins: []types.GovernanceUnitAdmin{}, ParentWalls: []unitParentWalls{}}
	for _, u := range resp.Units {
		if in[u.ID] {
			out.Units = append(out.Units, u)
		}
	}
	for _, m := range resp.Memberships {
		if in[m.UnitID] {
			out.Memberships = append(out.Memberships, m)
		}
	}
	for _, a := range resp.UnitAdmins {
		if in[a.UnitID] {
			out.UnitAdmins = append(out.UnitAdmins, a)
		}
	}
	byID := map[uuid.UUID]unitResolution{}
	for _, u := range resolved {
		byID[u.Row.ID] = u
	}
	for _, root := range roots {
		parent := byID[root].Row.ParentID
		if parent == nil {
			continue
		}
		p, ok := byID[*parent]
		if !ok {
			continue
		}
		walls := unitParentWalls{UnitID: root, ParentID: p.Row.ID, ParentName: p.Row.Name}
		if p.Resolved == nil {
			walls.Effective.Error = "the walls above this unit cannot be composed"
		} else {
			walls.Effective = governanceEffective{Ceiling: redactSpecForRead(p.Resolved.Ceiling, s.specReadTierFor(ctx)), Limits: p.Resolved.Limits}
			walls.Provenance = s.wallsProvenance(rows, p.Resolved, s.locksOver(rows, parent, b, limit))
		}
		out.ParentWalls = append(out.ParentWalls, walls)
	}
	return out, nil
}

// subtreeUnits is the units in the subtree of one of grants' units (the unit and every unit below it),
// and the subtree roots: each granted unit with no granted unit above it, in rows' order. The walk up
// is bounded by limit and a visited set, so a seeded cycle ends it.
func subtreeUnits(rows []types.GovernanceUnit, grants []types.GovernanceUnitAdmin, limit int) (map[uuid.UUID]bool, []uuid.UUID) {
	granted := map[uuid.UUID]bool{}
	for _, g := range grants {
		granted[g.UnitID] = true
	}
	byID := unitsByID(rows)
	in := map[uuid.UUID]bool{}
	var roots []uuid.UUID
	for _, r := range rows {
		root := granted[r.ID]
		seen := map[uuid.UUID]bool{r.ID: true}
		for cur := r.ParentID; cur != nil && !seen[*cur] && len(seen) <= limit; {
			row, ok := byID[*cur]
			if !ok {
				break
			}
			if granted[*cur] {
				in[r.ID], root = true, false
				break
			}
			seen[*cur] = true
			cur = row.ParentID
		}
		if granted[r.ID] {
			in[r.ID] = true
		}
		if root {
			roots = append(roots, r.ID)
		}
	}
	return in, roots
}

// wallsProvenance is, knob by knob, the layer that set res's effective value: the deepest unit of its
// chain whose value differs from the one it composes on (for the top of the chain, a knob it states),
// else the deployment (the Default unit's walls are WARDYN_DEFAULT_POLICY). A knob locks fixes is
// locked, named for its nearest locker.
func (s *Server) wallsProvenance(rows []types.GovernanceUnit, res *ResolvedUnit, locks map[string]knobLock) map[string]policyref.Provenance {
	out := map[string]policyref.Provenance{}
	for k := range lockableKnobs {
		out[k] = deploymentProvenance
	}
	byID := unitsByID(rows)
	var above map[string]json.RawMessage
	for i := len(res.Chain) - 1; i >= 0; i-- {
		link := res.Chain[i]
		eff := resolvedKnobs(s.resolveForDiff(rows, link.ID))
		for k := range lockableKnobs {
			v, has := eff[k]
			set := false
			if i == len(res.Chain)-1 {
				_, set = statedKnobs(byID[link.ID])[k]
			} else if old, had := above[k]; has != had || (has && !sameRaw(v, old)) {
				set = true
			}
			if set && link.ID != store.DefaultUnitID {
				out[k] = policyref.Provenance{Layer: policyref.LayerUnit, Name: link.Name, Contact: nonZeroContact(byID[link.ID].Contact)}
			}
		}
		above = eff
	}
	for k, l := range locks {
		out[k] = lockedProvenance(l)
	}
	return out
}

// lockedProvenance is the record of a knob lock l fixes: its locker's layer, name and contact.
func lockedProvenance(l knobLock) policyref.Provenance {
	layer := policyref.LayerUnit
	if l.locker == boundaryLocker {
		layer = policyref.LayerBoundary
	}
	return policyref.Provenance{Layer: layer, Name: l.locker, Contact: l.contact, Locked: true}
}

// meUnitAdmin is one unit of GET /me's unit_admin_of.
type meUnitAdmin struct {
	UnitID   uuid.UUID `json:"unit_id"`
	UnitName string    `json:"unit_name"`
}

// meUnitAdminOf is GET /me's unit_admin_of: each unit the caller administers by a grant (theirs or a
// group's), once, by name. Empty for none, and for a caller with no person; advisory, so a store error
// is logged and reads as none rather than failing the most-polled route.
func (s *Server) meUnitAdminOf(r *http.Request) []meUnitAdmin {
	out := []meUnitAdmin{}
	grants, err := s.callerUnitGrants(r.Context())
	if err == nil && len(grants) > 0 {
		var units []types.GovernanceUnit
		if units, err = s.unitTreeRows(r.Context()); err == nil {
			byID := unitsByID(units)
			for _, g := range grants {
				u, ok := byID[g.UnitID]
				if ok && !slices.ContainsFunc(out, func(m meUnitAdmin) bool { return m.UnitID == u.ID }) {
					out = append(out, meUnitAdmin{UnitID: u.ID, UnitName: u.Name})
				}
			}
		}
	}
	if err != nil {
		slog.WarnContext(r.Context(), "me: read unit admin grants", "error", err)
	}
	return out
}
