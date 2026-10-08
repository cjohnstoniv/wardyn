// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"

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

var _ RestrictedComponentCreator = PG{}

// CreateRestrictedComponent — see RestrictedComponentCreator.
func (s PG) CreateRestrictedComponent(ctx context.Context, c types.Component, capability, by string) (types.Component, error) {
	if c.Owner != "" {
		return types.Component{}, errors.New("store: create restricted component: only an organisation component is restricted this way")
	}
	var out types.Component
	err := s.inTx(ctx, func(q Querier) error {
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
