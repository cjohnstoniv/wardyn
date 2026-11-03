// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// remoteMacWindowSub is a runner's remote substrate as the grant's macWindowSub: it carries the grant to the
// runner's mac_window call and words the runner's three refusals as this package's.
type remoteMacWindowSub struct{ sub *remote.Substrate }

func (r remoteMacWindowSub) MacWindowSupported() (supported, reported bool) {
	return r.sub.MacWindowSupported()
}

func (r remoteMacWindowSub) MacWindow(ctx context.Context, ref string, req macWindowRequest) (macWindowSession, error) {
	w, err := r.sub.MacWindow(ctx, ref, remote.MacWindowRequest{GrantID: req.GrantID.String(), Title: req.Title})
	switch {
	case errors.Is(err, remote.ErrMacWindowNotAtMac):
		return nil, errMacWindowNotAtMac
	case errors.Is(err, remote.ErrMacWindowAlreadyOpen):
		return nil, errMacWindowAlreadyOpen
	case errors.Is(err, remote.ErrMacWindowLockUnknown):
		return nil, errMacWindowLockUnknown
	case err != nil:
		return nil, err
	}
	return w, nil
}

// MacWindowUnexpectedFromRunner is the checked mac_window_unexpected adapter: a window the runner's helper
// opened for which the runner issued no grant. The event names the run, and the run must be this runner's.
func (s *Server) MacWindowUnexpectedFromRunner(ctx context.Context, runnerID uuid.UUID, ev runnerwire.Event) error {
	e, err := decodeRunnerEventData[runnerwire.MacWindowUnexpected](ev.Data)
	if err != nil || ev.RunID == nil {
		return errReportShape
	}
	run, err := s.cfg.Store.GetRun(ctx, *ev.RunID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: %w", errRunnerUnknownRun, err)
	}
	if err != nil {
		return err
	}
	if run.RunnerID == nil || *run.RunnerID != runnerID {
		return errRunnerEventRefused
	}
	s.auditMacWindowUnexpected(ctx, runnerID, ev.RunID, e.Grant)
	return nil
}
