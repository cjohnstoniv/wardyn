// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// boundBackgroundIdleStop is the half of "a background task has no idle stop"
// that the request cannot say: the idle setting may arrive from a stored policy,
// the deployment default or the governance profile, none of which the person
// wrote. A Background run ends when its work exits, so whatever the resolved
// policy carries is not applied, the run row stores 0, and the idle reaper never
// reads it (the run-mode conflict refuses the one source the person did write,
// inline_policy). The note rides the 201 and the create audit, so the difference
// from the policy is never silent.
func boundBackgroundIdleStop(req createRunRequest, spec *types.RunPolicySpec) []string {
	if req.Experience != client.ExperienceBackground || spec.AutoStopAfterSec == 0 {
		return nil
	}
	was := spec.AutoStopAfterSec
	spec.AutoStopAfterSec = 0
	return []string{fmt.Sprintf("a background task ends when its work exits, so the policy's idle stop (auto_stop_after_sec %d) does not apply", was)}
}

// noRepositoriesResolvedRefusal is the half of "no repositories or drives" the
// request cannot see: a stored policy, the deployment default or a preset's
// policy can mount a directory or clone a repository the person never named, and
// the explicit choice of nothing must not be answered with a run that has
// something. It is refused, never dropped, with the sentence the request-level
// check uses. (A built-in Git component grants access and attaches nothing, and a
// devcontainer repository is an image source, so neither counts as attached.)
func noRepositoriesResolvedRefusal(req createRunRequest, spec types.RunPolicySpec) *runRefusal {
	if !req.NoRepositoriesOrDrives || (len(spec.WorkspaceMounts) == 0 && len(spec.WorkspaceRepos) == 0) {
		return nil
	}
	return runError(http.StatusUnprocessableEntity, reasonRunModeConflict, noRepositoriesConflictMsg())
}
