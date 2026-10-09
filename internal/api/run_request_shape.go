// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"net/http"
)

func runRequestEnums(req createRunRequest) (types.ConfinementClass, *runRefusal) {
	reqCC, ccOK := parseConfinementClass(req.ConfinementClass)
	if !ccOK {
		return "", runError(http.StatusBadRequest, reasonConfinementClassUnknown, fmt.Sprintf("unknown confinement_class %q", req.ConfinementClass))
	}

	// task_mode is a tiny closed enum; reject anything else up front (fail
	// closed, same shape as confinement_class above).
	if req.TaskMode != "" && req.TaskMode != "harness" && req.TaskMode != "exec" {
		return "", runError(http.StatusBadRequest, reasonTaskModeUnknown, fmt.Sprintf("unknown task_mode %q (want harness or exec)", req.TaskMode))
	}

	// interactive_start is task_mode's interactive counterpart and gets the same
	// closed-enum treatment. NOTE there is deliberately no title requirement
	// here: the console requires one, but the site-config probe, harness login
	// and workspace record/verify all create runs with no human to name them,
	// so a 400 would break every one of them. Title is a display field; the
	// console is where it is required.
	if req.InteractiveStart != "" && req.InteractiveStart != "shell" && req.InteractiveStart != "agent" {
		return "", runError(http.StatusBadRequest, reasonInteractiveStartUnknown, fmt.Sprintf("unknown interactive_start %q (want shell or agent)", req.InteractiveStart))
	}

	// tool_approvals gates whether an AUTONOMOUS (non-interactive) Claude Code
	// run's own tool calls route to a Wardyn approval instead of running
	// unsupervised — same closed-enum treatment as task_mode/interactive_start
	// above.
	if req.ToolApprovals != "" && req.ToolApprovals != "auto" && req.ToolApprovals != "hold" {
		return "", runError(http.StatusBadRequest, reasonToolApprovalsUnknown, fmt.Sprintf("unknown tool_approvals %q (want auto or hold)", req.ToolApprovals))
	}
	// codex-cli has no external tool-approval contract (verified only as far
	// as that harness's own docs go) — refuse the request outright
	// rather than silently falling back to today's unsupervised skip-permissions,
	// which would contradict the "hold" the caller explicitly asked for.
	if req.ToolApprovals == "hold" && req.Agent == "codex-cli" {
		return "", runError(http.StatusBadRequest, reasonToolApprovalsHoldUnsupportedAgent, "tool_approvals=hold is not supported for codex-cli (no external tool-approval contract)")
	}

	return reqCC, nil
}
