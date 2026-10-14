// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerLimitsCheck grades the runner_limits row: runners that take no run because no limits are set.
// ok is false when there are none. It never sets Blocking: the operator reads this row in the console
// that setting limits happens in. The count is of claimed self-hosted runners and configured
// remote-hosted ones; an unclaimed or revoked runner is offered no run either way.
func runnerLimitsCheck(unset int) (SetupCheck, bool) {
	if unset == 0 {
		return SetupCheck{}, false
	}
	noun := "runners have"
	if unset == 1 {
		noun = "runner has"
	}
	return SetupCheck{ID: "runner_limits", Label: "Runner limits", Status: "warn",
		Detail: fmt.Sprintf("%d %s no limits set, so every run placed on it is refused (runner_limits_unset).", unset, noun),
		Fix: "List them with `wardyn runner list --all --no-limits` and `wardyn runner remote list --no-limits`, " +
			"then print a proposal with `wardyn runner remote propose <id>` (remote-hosted) and apply it with `wardyn runner set <id> ...`."}, true
}

// runnerLimitsRow counts the runners with no limits. A store that cannot be read leaves the row out:
// nothing is guessed, and the runner pages show the same fact.
func (s *Server) runnerLimitsRow(ctx context.Context) (SetupCheck, bool) {
	unset := 0
	if rs, ok := s.cfg.Store.(store.RunnerStore); ok {
		runners, err := rs.ListRunners(ctx)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: setup status: list runners", slog.Any("err", err))
			return SetupCheck{}, false
		}
		for _, r := range runners {
			if r.State == types.RunnerClaimed && r.Limits == nil {
				unset++
			}
		}
	}
	if rs, ok := s.cfg.Store.(store.RemoteRunnerStore); ok {
		rows, err := rs.ListRemoteRunners(ctx)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: setup status: list remote runners", slog.Any("err", err))
			return SetupCheck{}, false
		}
		for _, row := range rows {
			if row.Runner.Limits == nil && s.remoteRunnerConfigured(row.Runner.ID) {
				unset++
			}
		}
	}
	return runnerLimitsCheck(unset)
}
