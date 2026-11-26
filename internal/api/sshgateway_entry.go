// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sshRunEntryRefusal is the run checks both SSH doors make after their owner rule: a background task has no SSH
// access for anyone, a super admin included (the run's shape, not a permission, so it sits after the ownership checks
// and outside the exemption), and deny_interactive on the profile the run was created under closes SSH for all but
// an exempt caller, as the terminal attach does (governance_run_doors.go). exempt is a super admin on their own run
// or an operator-owned one, never through a support grant, so it cannot reach a person's run. It returns the
// ssh.authenticate reason, "" when entry may go on, and the profile that denies interactive sessions when one does
// (so a reason with no profile, past the background check, is a profile that could not be read).
func (s *Server) sshRunEntryRefusal(ctx context.Context, run types.AgentRun, exempt bool) (reason, profile string) {
	if backgroundOnlyRefusal(run, "SSH access") != nil {
		return reasonRunBackgroundOnly, ""
	}
	if exempt {
		return "", ""
	}
	name, err := s.interactiveDeniedUnit(ctx, run)
	if err != nil {
		return "governance profile unreadable", ""
	}
	if name != "" {
		return fmt.Sprintf("governance profile %q denies interactive sessions", name), name
	}
	return "", ""
}
