// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// withRunHeld runs do on parent holding run id's lock, as an end does: the hold is taken apart from
// parent's cancel, which still ends do's ctx, and released when do returns.
func withRunHeld(t *testing.T, s *Server, parent context.Context, id uuid.UUID, do func(context.Context)) {
	t.Helper()
	held, unlock, err := s.lockRunOp(context.WithoutCancel(parent), id)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancelCause(held)
	defer cancel(nil)
	if dl, ok := parent.Deadline(); ok {
		var cancelDL context.CancelFunc
		ctx, cancelDL = context.WithDeadline(ctx, dl)
		defer cancelDL()
	}
	defer context.AfterFunc(parent, func() { cancel(context.Cause(parent)) })()
	do(ctx)
}

// finalizeHeld is finalizeRunEffects under run id's lock (withRunHeld).
func finalizeHeld(t *testing.T, s *Server, parent context.Context, id uuid.UUID, ref, action string, data map[string]any, finishAfterStop, gracefulStop bool) (err error) {
	t.Helper()
	withRunHeld(t, s, parent, id, func(ctx context.Context) {
		err = s.finalizeRunEffects(ctx, id, ref, action, data, finishAfterStop, gracefulStop)
	})
	return err
}

// bringBackHeld is bringComputerFolderBack under run id's lock (withRunHeld).
func bringBackHeld(t *testing.T, s *Server, id uuid.UUID, ref string) (kept bool) {
	t.Helper()
	withRunHeld(t, s, context.Background(), id, func(ctx context.Context) {
		kept = s.bringComputerFolderBack(ctx, id, ref)
	})
	return kept
}
