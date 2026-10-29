// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func serviceCommandRequest(s *types.RunService) createRunRequest {
	return createRunRequest{Experience: client.ExperienceBackground,
		Workload: &client.RunWorkload{Kind: client.WorkloadCommand, Command: "make serve"}, Service: s}
}

// TestServiceRefusal: a service rides a background run of a command on a container sandbox, and only
// there. The shape is 422 run_service_shape, a VM sandbox is request_field_unavailable, and a malformed
// spec is run_mode_invalid, all from the contract's own shape checks.
func TestServiceRefusal(t *testing.T) {
	asked := &types.RunService{Port: 8080, Health: &types.ServiceHealth{Path: "/healthz"}}
	vm := serviceCommandRequest(asked)
	vm.MachineKind = string(types.MachineLinuxVM)
	interactive := serviceCommandRequest(asked)
	interactive.Experience, interactive.Workload = client.ExperienceInteractive, nil
	agentTask := serviceCommandRequest(asked)
	agentTask.Workload = &client.RunWorkload{Kind: client.WorkloadAgentTask, Agent: "claude-code", Task: "t"}
	noWorkload := serviceCommandRequest(asked)
	noWorkload.Workload = nil
	legacyExec := createRunRequest{TaskMode: "exec", Task: "make serve", Service: asked}

	for _, tc := range []struct {
		name   string
		req    createRunRequest
		status int
		reason string
	}{
		{"no service", serviceCommandRequest(nil), 0, ""},
		{"a background command", serviceCommandRequest(asked), 0, ""},
		{"a bare spec takes every default", serviceCommandRequest(&types.RunService{}), 0, ""},
		{"an interactive run", interactive, http.StatusUnprocessableEntity, reasonRunServiceShape},
		{"a background agent task", agentTask, http.StatusUnprocessableEntity, reasonRunServiceShape},
		{"a background run with no workload", noWorkload, http.StatusUnprocessableEntity, reasonRunServiceShape},
		{"the legacy task_mode exec shape", legacyExec, http.StatusUnprocessableEntity, reasonRunServiceShape},
		{"a linux VM", vm, http.StatusUnprocessableEntity, reasonRequestFieldUnavailable},
		{"an unknown restart", serviceCommandRequest(&types.RunService{Restart: "always"}), http.StatusBadRequest, reasonRunModeInvalid},
		{"a health check with no port", serviceCommandRequest(&types.RunService{Health: &types.ServiceHealth{}}), http.StatusBadRequest, reasonRunModeInvalid},
		{"a port out of range", serviceCommandRequest(&types.RunService{Port: 70000}), http.StatusBadRequest, reasonRunModeInvalid},
		{"a health path that is not a request target", serviceCommandRequest(&types.RunService{Port: 80, Health: &types.ServiceHealth{Path: "healthz"}}),
			http.StatusBadRequest, reasonRunModeInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The contract's list, not the function alone: the create, Review, preview, preset and template doors all run it.
			refusal := runContractShapeRefusal(tc.req)
			if tc.reason == "" {
				if refusal != nil {
					t.Fatalf("refused: %+v", refusal.body)
				}
				return
			}
			if refusal == nil || refusal.status != tc.status || refusal.body.Reason != tc.reason {
				t.Fatalf("refusal = %+v, want %d %s", refusal, tc.status, tc.reason)
			}
		})
	}
	if refusal := serviceRefusal(interactive); refusal == nil || refusal.body.Error != "A service is a background run of a command." {
		t.Errorf("the shape refusal says %+v, want the canon sentence", refusal)
	}
}

// TestServiceRefusalNeverWritesTheRequest: validation fills defaults on a copy, so the request is never
// written through.
func TestServiceRefusalNeverWritesTheRequest(t *testing.T) {
	req := serviceCommandRequest(&types.RunService{Port: 80, Health: &types.ServiceHealth{}})
	if refusal := serviceRefusal(req); refusal != nil {
		t.Fatalf("refused: %+v", refusal.body)
	}
	if req.Service.Restart != "" || req.Service.Health.IntervalSec != 0 || req.Service.Health.FailureThreshold != 0 {
		t.Fatalf("the request was written through: %+v %+v", req.Service, req.Service.Health)
	}
}

func TestFrozenRunServiceFillsDefaults(t *testing.T) {
	if got, err := frozenRunService(nil); got != nil || err != nil {
		t.Fatalf("no service froze to %+v, %v", got, err)
	}
	got, err := frozenRunService(&types.RunService{Port: 80, Health: &types.ServiceHealth{Path: "/"}})
	want := types.RunService{Restart: types.ServiceRestartOnFailure, Port: 80,
		Health: &types.ServiceHealth{Path: "/", IntervalSec: types.DefaultServiceHealthIntervalSec, FailureThreshold: types.DefaultServiceHealthFailures}}
	if err != nil || got.Restart != want.Restart || got.Port != want.Port || *got.Health != *want.Health {
		t.Fatalf("froze to %+v %+v, %v; want %+v %+v", got, got.Health, err, want, want.Health)
	}
	if _, err := frozenRunService(&types.RunService{Restart: "always"}); err == nil {
		t.Fatal("an unknown restart froze")
	}
}

// TestServiceEnvFromTheFrozenSpec: the supervisor's variables, from run.Service, through the dispatch
// env writer. A run with no service carries none of them.
func TestServiceEnvFromTheFrozenSpec(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *types.RunService
		want map[string]string
	}{
		{"not a service", nil, map[string]string{}},
		{"restart only", &types.RunService{Restart: types.ServiceRestartNever},
			map[string]string{"WARDYN_SERVICE": "1", "WARDYN_SERVICE_RESTART": "never"}},
		{"a port, no health check", &types.RunService{Restart: types.ServiceRestartOnFailure, Port: 8080},
			map[string]string{"WARDYN_SERVICE": "1", "WARDYN_SERVICE_RESTART": "on_failure", "WARDYN_SERVICE_PORT": "8080"}},
		{"a TCP health check", &types.RunService{Restart: types.ServiceRestartOnFailure, Port: 8080, Health: &types.ServiceHealth{IntervalSec: 15, FailureThreshold: 3}},
			map[string]string{"WARDYN_SERVICE": "1", "WARDYN_SERVICE_RESTART": "on_failure", "WARDYN_SERVICE_PORT": "8080",
				"WARDYN_SERVICE_HEALTH_INTERVAL": "15", "WARDYN_SERVICE_HEALTH_THRESHOLD": "3"}},
		{"an HTTP health check", &types.RunService{Restart: types.ServiceRestartOnFailure, Port: 8080, Health: &types.ServiceHealth{Path: "/healthz", IntervalSec: 30, FailureThreshold: 5}},
			map[string]string{"WARDYN_SERVICE": "1", "WARDYN_SERVICE_RESTART": "on_failure", "WARDYN_SERVICE_PORT": "8080",
				"WARDYN_SERVICE_HEALTH_PATH": "/healthz", "WARDYN_SERVICE_HEALTH_INTERVAL": "30", "WARDYN_SERVICE_HEALTH_THRESHOLD": "5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			applyDispatchModeEnv(env, types.AgentRun{ID: uuid.New(), Service: tc.spec}, dispatchParams{})
			got := map[string]string{}
			for k, v := range env {
				if strings.HasPrefix(k, "WARDYN_SERVICE") {
					got[k] = v
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("env = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q (env %v)", k, got[k], v, got)
				}
			}
		})
	}
}

// TestServiceCreateWarnings: the pause sentence rides a service's 201 only under an idle pause.
func TestServiceCreateWarnings(t *testing.T) {
	svc := &types.RunService{Restart: types.ServiceRestartOnFailure}
	paused := types.RunLimits{PauseIdleAfterSec: 900}
	for _, tc := range []struct {
		name string
		run  types.AgentRun
		want []string
	}{
		{"a service under an idle pause", types.AgentRun{Service: svc, RunLimits: paused},
			[]string{"Your admin pauses runs nobody is using, so this service will be paused while it waits for requests."}},
		{"a service with no idle pause", types.AgentRun{Service: svc}, nil},
		{"a run that is not a service", types.AgentRun{RunLimits: paused}, nil},
	} {
		if got := serviceCreateWarnings(tc.run); !slices.Equal(got, tc.want) {
			t.Errorf("%s: warnings %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestQueuedServiceWarnings: a queued run captures no limits until it starts, so its 201 reads the
// limits the launch resolved, not the empty ones on the queued row.
func TestQueuedServiceWarnings(t *testing.T) {
	svc := &types.RunService{Restart: types.ServiceRestartOnFailure}
	queued := types.AgentRun{State: types.RunQueued, Service: svc}
	if got := queuedServiceWarnings(queued, types.RunLimits{PauseIdleAfterSec: 900}); !slices.Equal(got, []string{servicePauseWarning}) {
		t.Errorf("under an idle pause: %q, want the pause sentence", got)
	}
	if got := queuedServiceWarnings(queued, types.RunLimits{}); got != nil {
		t.Errorf("with no idle pause: %q, want none", got)
	}
	if got := queuedServiceWarnings(types.AgentRun{State: types.RunQueued}, types.RunLimits{PauseIdleAfterSec: 900}); got != nil {
		t.Errorf("not a service: %q, want none", got)
	}
}

// TestRunCreateAuditRecordsService: run.create carries the frozen spec, defaults filled, and nothing for
// a run that is not a service.
func TestRunCreateAuditRecordsService(t *testing.T) {
	audit := func(req createRunRequest) map[string]any {
		return createRunAuditData(req, nil, types.CC2, "", "jti", nil, types.AutonomyResolution{}, false, runProviderChoice{})
	}
	got, ok := audit(serviceCommandRequest(&types.RunService{Port: 80}))["service"].(*types.RunService)
	if !ok || got.Restart != types.ServiceRestartOnFailure || got.Port != 80 {
		t.Fatalf("run.create service = %+v, want the spec with its defaults filled", got)
	}
	if _, present := audit(serviceCommandRequest(nil))["service"]; present {
		t.Fatal("run.create carries a service the request did not name")
	}
}

// TestServiceShapeAtTheDoors: the shape refusal answers the same at create, Review and the policy
// preview, and a Run document is held to it by the import door.
func TestServiceShapeAtTheDoors(t *testing.T) {
	const (
		service = `"service":{"port":8080}`
		command = `"experience":"background","workload":{"kind":"command","command":"make serve"},` + noRepos
	)
	for _, door := range contractDoors {
		t.Run("interactive/"+door.name, func(t *testing.T) {
			h := newHarness(t)
			w := do(t, h.srv, http.MethodPost, door.path, adminToken, modeBody(interactiveOK+`,`+service))
			if w.Code != http.StatusUnprocessableEntity || errorReason(w) != reasonRunServiceShape {
				t.Fatalf("got %d %s, want 422 %s", w.Code, w.Body.String(), reasonRunServiceShape)
			}
		})
	}
	for fields, wantRefused := range map[string]bool{command + `,` + service: false, interactiveOK + `,` + service: true} {
		res := validateTemplateImport([]byte(docIntent(fields)), client.TemplateFormatJSON, "")
		if got := slices.Contains(importReasons(res), reasonRunServiceShape+"@intent"); got != wantRefused {
			t.Errorf("door diagnostics for %s = %v, service shape refused: %v, want %v", fields, importReasons(res), got, wantRefused)
		}
	}
}
