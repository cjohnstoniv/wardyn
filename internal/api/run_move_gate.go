// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// The build gate on moving a run. Moving a run needs the stopped pause and the revoke at pause, which arrive in
// their own changes; until the change that wires the move to them sets moveReachable, no configuration reaches a
// move. moveReachable is a const, which no linker flag can set, and the file has no build constraint.
// TestMoveGateSeamsSetOnlyInTests keeps every write of moveGateOpen, and of each movePort variable, in a test file.
const moveReachable = false

// moveGateOpen is read by every move entry point and by the features hook. Only _test.go files may assign it.
var moveGateOpen = func() bool { return moveReachable }

// The store answers whether a move holds a run, in the shape a Resume reads it through: store.PG keeps the
// method a Resume's refusal relies on.
var _ interface {
	RunMoveInFlight(context.Context, uuid.UUID) (bool, error)
} = store.PG{}
