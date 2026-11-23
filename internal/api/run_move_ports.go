// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//lint:file-ignore U1000 The move's executor, a later change, calls these ports; until the change that wires them
// to the pause's own functions, each one is unset and refuses.

package api

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The fail-closed ports from a move to the stopped pause and to the revoke at pause, whose functions arrive in
// their own changes. Each port carries types the move owns, with only the fields the move reads, so no port names
// a type from those changes. Every port has one shape and is called only through call, which answers
// errPauseInterfaceMissing for a port no one set: a port never answers success by default. The change that sets
// moveReachable replaces each variable with a direct call to an adapter. Only _test.go files may set a port
// (TestMoveGateSeamsSetOnlyInTests).

// errPauseInterfaceMissing is every unset port's answer.
var errPauseInterfaceMissing = errors.New("api: moving a run needs the stopped pause, which this build does not wire")

// movePort is one port: an argument the move owns in, an answer the move owns out, and always an error.
type movePort[A, T any] func(context.Context, A) (T, error)

// call is the only way to call a port: a nil port answers the zero T and errPauseInterfaceMissing.
func (p movePort[A, T]) call(ctx context.Context, a A) (T, error) {
	if p == nil {
		var zero T
		return zero, errPauseInterfaceMissing
	}
	return p(ctx, a)
}

// moveStopArgs is who stops the run for a move.
type moveStopArgs struct {
	Run                  types.AgentRun
	ActorType, Principal string
}

// movePauseReceipt is what the move reads of a stop: the stopped row's generation.
type movePauseReceipt struct {
	Gen int64
}

// moveGenArgs names one pause generation of a run.
type moveGenArgs struct {
	RunID uuid.UUID
	Gen   int64
}

// moveRevokeState is what the move reads of a pause's revoke: whether it completed, and the Azure DevOps revoke's
// outcome.
type moveRevokeState struct {
	Complete bool
	ADO      string
}

// moveMintArgs is a run and the destination's withholding, judged before anything is minted there.
type moveMintArgs struct {
	Run  types.AgentRun
	Dest withholding
}

// moveMintPlan is the destination's grant judgement: grants kept, dropped and re-approved.
type moveMintPlan struct {
	Keep, Drop, Reapprove []uuid.UUID
}

// moveGrantRefusal is a needed grant the destination withholds, with the layer that withholds it.
type moveGrantRefusal struct {
	Layer, Reason string
}

func (e *moveGrantRefusal) Error() string { return e.Reason + ": " + e.Layer }

// moveCarriedSet is what a move carries: the roots, and what under them is never carried.
type moveCarriedSet struct {
	Roots, Exclude []string
}

// moveResumeArgs is a stopped run resumed on a sandbox at a pause generation.
type moveResumeArgs struct {
	Run        types.AgentRun
	SandboxRef string
	Gen        int64
}

// The twelve ports, none set outside tests.
var (
	movePortStopRefusal       movePort[types.AgentRun, string]
	movePortPauseGen          movePort[types.AgentRun, int64]
	movePortPauseStop         movePort[moveStopArgs, movePauseReceipt]
	movePortRevokeFor         movePort[moveGenArgs, moveRevokeState]
	movePortLineageCut        movePort[uuid.UUID, bool]
	movePortStampCutLineage   movePort[uuid.UUID, bool]
	movePortLocallyContained  movePort[uuid.UUID, bool]
	movePortMintAtDestination movePort[moveMintArgs, moveMintPlan]
	movePortCarriedSet        movePort[types.AgentRun, moveCarriedSet]
	movePortRevokeAtPause     movePort[moveGenArgs, moveRevokeState]
	movePortWithPauseResume   movePort[struct{}, context.Context]
	movePortResumeOn          movePort[moveResumeArgs, struct{}]
)
