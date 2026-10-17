// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RestrictedComponentCreator creates an organisation component together with the
// capability restriction that keeps it attachable by nobody until an admin names
// who may. Optional beside ComponentStore, for the same reason: a store without it
// must make the caller refuse, never create the row unrestricted.
type RestrictedComponentCreator interface {
	// CreateRestrictedComponent inserts the org row c (Owner "") at version 1 under its caller-chosen
	// id and restricts (capability, c.ID) in the SAME transaction, so no reader can find the row
	// while it is open to every member, and a failed insert leaves no restriction behind. Errors as
	// CreateComponent; ErrConflict also rolls the restriction back.
	CreateRestrictedComponent(ctx context.Context, c types.Component, capability, by string) (types.Component, error)
}

// RestrictedComponentDeleter deletes an organisation component without ever leaving its id open.
// Optional for the reason RestrictedComponentCreator is: a store without it must make the caller
// refuse, never delete the row and leave the rest undone.
type RestrictedComponentDeleter interface {
	// DeleteRestrictedComponent deletes the org row id, restricts (capability, id) if it is not
	// restricted already, and deletes every grant of capability whose value is id, in ONE
	// transaction: the id ends restricted with no grant naming it, or nothing changed. It returns
	// the deleted row, how many grants went, and whether the restriction insert wrote a row (false
	// when the id was still restricted, true when a lift had opened it and this put it back).
	// ErrNotFound when no org row has the id, and then nothing is written. A wildcard grant names no
	// id and is kept.
	DeleteRestrictedComponent(ctx context.Context, id uuid.UUID, capability, by string) (types.Component, int, bool, error)
}

// ComponentRestrictionLifter lifts an organisation component's restriction only while the component
// exists. Optional beside ComponentStore: a store without it lifts like any other kind.
type ComponentRestrictionLifter interface {
	// LiftComponentRestriction deletes the restriction row of the component id, in one transaction
	// under the availability lock, after checking that an org row (owner "") has that id. ErrNotFound
	// when none does, and then nothing is written: lifting a deleted id's restriction would open an id
	// that must stay closed.
	LiftComponentRestriction(ctx context.Context, id uuid.UUID, by string) error
}

var (
	_ RestrictedComponentCreator = PG{}
	_ RestrictedComponentDeleter = PG{}
	_ ComponentRestrictionLifter = PG{}
)

// CreateRestrictedComponent — see RestrictedComponentCreator.
func (s PG) CreateRestrictedComponent(ctx context.Context, c types.Component, capability, by string) (types.Component, error) {
	if c.Owner != "" {
		return types.Component{}, errors.New("store: create restricted component: only an organisation component is restricted this way")
	}
	var out types.Component
	err := s.inTx(ctx, func(q Querier) error {
		// A grant written for this id before its row existed (the security tier may name any uuid)
		// must not make the new component attachable: it starts with no grant, whoever wrote one.
		if _, err := q.Exec(ctx, `DELETE FROM capability_grants WHERE capability = $1 AND value = $2`, capability, c.ID.String()); err != nil {
			return fmt.Errorf("store: clear grants naming the new component: %w", err)
		}
		if err := SetCapabilityRestrictionQ(ctx, q, capability, c.ID.String(), true, by); err != nil {
			return err
		}
		var err error
		out, err = createComponentQ(ctx, q, c)
		return err
	})
	if err != nil {
		return types.Component{}, err
	}
	return out, nil
}

// DeleteRestrictedComponent — see RestrictedComponentDeleter.
func (s PG) DeleteRestrictedComponent(ctx context.Context, id uuid.UUID, capability, by string) (types.Component, int, bool, error) {
	var out types.Component
	var grants int
	var restored bool
	err := s.inTx(ctx, func(q Querier) error {
		// The lock a write of this value's availability takes, so a lift cannot land between the
		// restriction below and the commit: the id is restricted at the moment this commits.
		if err := LockGovernanceTarget(ctx, q, "capability_availability", capability, id.String()); err != nil {
			return err
		}
		var err error
		if out, err = scanComponent(q.QueryRow(ctx, `DELETE FROM components WHERE id = $1 AND owner = '' RETURNING `+componentCols, id)); err != nil {
			return err
		}
		ins, err := q.Exec(ctx, `INSERT INTO capability_restrictions (capability, value, created_by) VALUES ($1, $2, $3)
			ON CONFLICT (capability, value) DO NOTHING`, capability, id.String(), by)
		if err != nil {
			return fmt.Errorf("store: set capability restriction: %w", err)
		}
		restored = ins.RowsAffected() > 0
		tag, err := q.Exec(ctx, `DELETE FROM capability_grants WHERE capability = $1 AND value = $2`, capability, id.String())
		if err != nil {
			return fmt.Errorf("store: delete the component's grants: %w", err)
		}
		grants = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return types.Component{}, 0, false, err
	}
	return out, grants, restored, nil
}

// LiftComponentRestriction — see ComponentRestrictionLifter.
func (s PG) LiftComponentRestriction(ctx context.Context, id uuid.UUID, by string) error {
	return s.inTx(ctx, func(q Querier) error {
		return LiftComponentRestrictionQ(ctx, q, id, by)
	})
}

// LiftComponentRestrictionQ is LiftComponentRestriction's statements on q, for a caller already in a
// transaction (a held change's apply). The availability lock is taken again, which is free inside one
// transaction, so the existence check and the delete are ordered against a component delete either way.
func LiftComponentRestrictionQ(ctx context.Context, q Querier, id uuid.UUID, _ string) error {
	const capability = "component"
	if err := LockGovernanceTarget(ctx, q, "capability_availability", capability, id.String()); err != nil {
		return err
	}
	var one int
	switch err := q.QueryRow(ctx, `SELECT 1 FROM components WHERE id = $1 AND owner = ''`, id).Scan(&one); {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("store: check the component exists: %w", err)
	}
	return SetCapabilityRestrictionQ(ctx, q, capability, id.String(), false, "")
}
