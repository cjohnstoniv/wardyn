// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// What an envelope changes in the approval paths of unit writes: a unit admin's edit strictly inside an
// envelope's root, within the root's walls, applies directly (envelopeExempt), and every held change of
// the root itself, and every unit admin grant or revoke on it, is decided by the security tier alone
// (envelopeRootApprover). An envelope that cannot be read proves nothing and guards everything: the
// write is held, and its root check answers "security only".
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// envelopeUnits is the set of units that carry an envelope, read on the decision's transaction when ctx
// carries one (withUnitScopeTx), otherwise on the store. A store that keeps no envelopes has none.
func (s *Server) envelopeUnits(ctx context.Context) (map[uuid.UUID]bool, error) {
	st, ok := s.cfg.Store.(store.EnvelopeStore)
	if !ok {
		return nil, nil
	}
	var rows []types.GovernanceEnvelope
	var err error
	if q, inTx := ctx.Value(unitScopeTxKey{}).(store.Querier); inTx {
		rows, err = store.ListEnvelopesQ(ctx, q)
	} else {
		rows, err = st.ListEnvelopes(ctx)
	}
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(rows))
	for _, e := range rows {
		out[e.UnitID] = true
	}
	return out, nil
}

// envelopeRoot reports whether unit carries an envelope, read as envelopeUnits reads.
func (s *Server) envelopeRoot(ctx context.Context, unit uuid.UUID) (bool, error) {
	st, ok := s.cfg.Store.(store.EnvelopeStore)
	if !ok {
		return false, nil
	}
	var err error
	if q, inTx := ctx.Value(unitScopeTxKey{}).(store.Querier); inTx {
		_, err = store.GetEnvelopeQ(ctx, q, unit)
	} else {
		_, err = st.GetEnvelope(ctx, unit)
	}
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// securityOnlyRoot reports whether a change of unit is the security tier's alone to decide: unit is an
// envelope's root, or the envelope cannot be read. The change read, the dry run and the decision all
// ask this one predicate, so the approvers listed are the approvers accepted.
func (s *Server) securityOnlyRoot(ctx context.Context, unit uuid.UUID) bool {
	root, err := s.envelopeRoot(ctx, unit)
	if err != nil {
		slog.ErrorContext(ctx, "api: read the envelope of a governance unit", "unit", unit, "err", err)
		return true
	}
	return root
}

// envelopeGuardedUnit is the unit whose envelope decides who approves ch: a unit change's own unit, or
// the unit of a unit admin grant or revoke. ok is false for every other change.
func envelopeGuardedUnit(ch types.GovernanceChange) (uuid.UUID, bool) {
	switch ch.TargetKind {
	case govKindUnit:
		id, err := uuid.Parse(ch.TargetKey)
		return id, err == nil
	case govKindUnitAdmin:
		unit, _, _, ok := splitMembershipKey(ch.TargetKey)
		return unit, ok
	}
	return uuid.Nil, false
}

// envelopeRootApprover is inner, except that a change of an envelope's root (envelopeGuardedUnit) is
// decided by a security caller only: the root's walls, locks, contact, parent and admins are the
// security tier's once it sets the envelope. A unit admin inner admits is refused with
// security_admin_surface, the reason of a security-only surface; a caller inner refuses keeps inner's.
func envelopeRootApprover(ch types.GovernanceChange, inner govApprover) govApprover {
	unit, guarded := envelopeGuardedUnit(ch)
	if !guarded {
		return inner
	}
	return govApprover{
		allowed: func(s *Server, r *http.Request) bool {
			if !inner.allowed(s, r) {
				return false
			}
			return s.isSecurityOperator(r.Context()) || !s.securityOnlyRoot(r.Context(), unit)
		},
		reason: inner.reason,
		refusal: func(s *Server, r *http.Request) authz.Reason {
			if inner.allowed(s, r) {
				return securityApprover.reason
			}
			return inner.reason
		},
		scoped: inner.scoped,
	}
}

// envelopeSecurityOnly reports whether held change ch lists the security admins alone because of an
// envelope (securityOnlyRoot on its guarded unit).
func (s *Server) envelopeSecurityOnly(ctx context.Context, ch types.GovernanceChange) bool {
	unit, guarded := envelopeGuardedUnit(ch)
	return guarded && s.securityOnlyRoot(ctx, unit)
}

// envelopeExempt is unitWriteExempt's third proof, for a unit admin's update of a unit strictly inside
// an envelope: the nearest envelope root strictly above the unit is the same before and after the
// write (so neither the root itself nor a re-parent out of it qualifies), and the new effective walls
// are Leq the root's with no higher member cap. Composition keeps a unit below the root within its walls,
// on a port too: the meet reads a bare entry as 80 and 443 only, as Leq does, so a unit naming another port
// of an entry the root names bare keeps neither. A security caller's write follows the
// second-human switch instead, and an envelope read error proves nothing.
func (s *Server) envelopeExempt(ctx context.Context, all []types.GovernanceUnit, p types.GovernanceUnit, res *ResolvedUnit) bool {
	if res == nil || !s.isUnitAdminCaller(ctx) {
		return false
	}
	envs, err := s.envelopeUnits(ctx)
	if err != nil {
		slog.WarnContext(ctx, "api: read the envelopes for a unit write; it is held", "unit", p.ID, "err", err)
		return false
	}
	if len(envs) == 0 || envs[p.ID] {
		return false
	}
	byID := unitsByID(all)
	cur, exists := byID[p.ID]
	if !exists {
		return false
	}
	root, ok := nearestEnvelopeAbove(byID, envs, cur.ParentID, p.ID)
	if !ok {
		return false
	}
	if after, ok := nearestEnvelopeAbove(byID, envs, p.ParentID, p.ID); !ok || after != root {
		return false
	}
	chain, err := chainFromRows(byID, root, store.MaxUnitDepthCap)
	if err != nil {
		return false
	}
	rootRes, err := s.composeChain(chain)
	if err != nil {
		return false
	}
	return composer.Leq(res.Ceiling, rootRes.Ceiling, res.Limits, rootRes.Limits) &&
		len(memberCapRaises(s.resolvedMemberCap(res), s.resolvedMemberCap(rootRes))) == 0
}

// nearestEnvelopeAbove walks parent links up from parent, the parent of unit self: the first unit that
// carries an envelope. A walk that meets self again (a cycle), a unit the rows lack, or the depth cap
// finds none.
func nearestEnvelopeAbove(byID map[uuid.UUID]types.GovernanceUnit, envs map[uuid.UUID]bool, parent *uuid.UUID, self uuid.UUID) (uuid.UUID, bool) {
	for n := 0; parent != nil && n < store.MaxUnitDepthCap; n++ {
		if *parent == self {
			return uuid.Nil, false
		}
		if envs[*parent] {
			return *parent, true
		}
		row, ok := byID[*parent]
		if !ok {
			return uuid.Nil, false
		}
		parent = row.ParentID
	}
	return uuid.Nil, false
}
