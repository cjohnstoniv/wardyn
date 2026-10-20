// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// runShapeConflictMsg says why a shape does not fit the run it rides on. Its
// TypeScript twin is RUN_SHAPE_CONFLICT in ui/src/app/lib/run-mode-refusals.ts.
func runShapeConflictMsg(shape, detail string) string {
	return fmt.Sprintf("shape %s does not fit this run: %s.", shape, detail)
}

// runShapeRefusal checks the New Run shape against the carriers it names. Each
// shape has one rule, and a shape needs the experience it is read against. It
// reads only carriers, so it holds after projection and in a partial document.
func runShapeRefusal(req createRunRequest) *runRefusal {
	if req.Shape == "" {
		return nil
	}
	conflict := func(detail string) *runRefusal {
		return runError(http.StatusUnprocessableEntity, reasonRunShapeConflict, runShapeConflictMsg(string(req.Shape), detail))
	}
	background := req.Experience == client.ExperienceBackground
	kind := client.WorkloadKind("")
	if req.Workload != nil {
		kind = req.Workload.Kind
	}
	switch {
	case !req.Shape.Valid():
		return runModeInvalid("shape: %q is not one of agent_task, environment, application", string(req.Shape))
	case req.Experience == "":
		return conflict("it needs experience, which says whether the run is background or interactive")
	case req.Shape == client.RunShapeApplication && !(background && kind == client.WorkloadCommand) && !applicationServer(req):
		return conflict("an application is a background run of a command, or an interactive run that starts a command and declares its app: " +
			"set experience background and workload kind command, or experience interactive with startup kind command and ui_apps")
	case req.Shape == client.RunShapeEnvironment && background:
		return conflict("an environment is interactive: set experience interactive")
	case req.Shape == client.RunShapeAgentTask && background && kind != client.WorkloadAgentTask:
		return conflict("a background agent task needs workload kind agent_task")
	case req.Shape == client.RunShapeAgentTask && !background &&
		!slices.ContainsFunc(req.Tools, func(t client.IncludedTool) bool { return t.Kind == client.IncludedToolHarness }):
		return conflict("an interactive agent task includes a harness in tools")
	}
	return nil
}

// applicationServer is the interactive application (#1994): an Environment run
// that starts a command and declares the app it serves, opened through the web
// app gateway. It is the same run the environment shape accepts; the shape only
// says which choice it was set up from.
func applicationServer(req createRunRequest) bool {
	pol, _ := requestPolicyRules(req)
	return req.Experience == client.ExperienceInteractive && req.Startup != nil &&
		req.Startup.Kind == client.StartupCommand && pol != nil && len(pol.UIApps) > 0
}
