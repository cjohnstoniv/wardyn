// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strconv"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// runServiceShapeMsg says why a service does not fit the run it rides on.
const runServiceShapeMsg = "A service is a background run of a command."

// serviceRefusal checks the request's service: it rides only a background run of a command on a
// container sandbox, and its fields are well formed. It reads only carriers, like runShapeRefusal, so
// it holds after projection and a template is held to it too. A request with no service passes.
func serviceRefusal(req createRunRequest) *runRefusal {
	if req.Service == nil {
		return nil
	}
	switch {
	case types.MachineKind(req.MachineKind).IsVM():
		return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
			"service: a service runs in a container sandbox, not a virtual machine.")
	case req.Experience != client.ExperienceBackground || req.Workload == nil || req.Workload.Kind != client.WorkloadCommand:
		return runError(http.StatusUnprocessableEntity, reasonRunServiceShape, runServiceShapeMsg)
	}
	if _, err := frozenRunService(req.Service); err != nil {
		return runModeInvalid("%s", err.Error())
	}
	return nil
}

// frozenRunService is the service spec a run keeps: a copy of s with its defaults filled, so the
// request is never written through. Nil in, nil out.
func frozenRunService(s *types.RunService) (*types.RunService, error) {
	if s == nil {
		return nil, nil
	}
	out := *s
	if s.Health != nil {
		h := *s.Health
		out.Health = &h
	}
	if err := types.ValidateRunService(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// serviceEnv sets the supervisor's variables from the run's frozen spec, never from the request. User
// env writers refuse the WARDYN_ prefix, so nothing else can set them.
func serviceEnv(env map[string]string, s *types.RunService) {
	if s == nil {
		return
	}
	env["WARDYN_SERVICE"] = "1"
	env["WARDYN_SERVICE_RESTART"] = string(s.Restart)
	if s.Port > 0 {
		env["WARDYN_SERVICE_PORT"] = strconv.Itoa(s.Port)
	}
	if h := s.Health; h != nil {
		if h.Path != "" {
			env["WARDYN_SERVICE_HEALTH_PATH"] = h.Path
		}
		env["WARDYN_SERVICE_HEALTH_INTERVAL"] = strconv.Itoa(h.IntervalSec)
		env["WARDYN_SERVICE_HEALTH_THRESHOLD"] = strconv.Itoa(h.FailureThreshold)
	}
}

// servicePauseWarning is the 201 sentence for a service created under an idle pause: the pause has no
// Background exclusion, so a service that waits for requests is frozen. No profile number is disclosed.
const servicePauseWarning = "Your admin pauses runs nobody is using, so this service will be paused while it waits for requests."

// serviceCreateWarnings are the sentences a service's 201 carries.
func serviceCreateWarnings(run types.AgentRun) []string {
	if run.Service != nil && run.RunLimits.PauseIdleAfterSec > 0 {
		return []string{servicePauseWarning}
	}
	return nil
}
