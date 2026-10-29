// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

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

// TestServiceCreateWarnings: the pause sentence rides a service's 201 only under an idle pause, and the
// no-cap sentence only when no lifetime cap applies.
func TestServiceCreateWarnings(t *testing.T) {
	svc := &types.RunService{Restart: types.ServiceRestartOnFailure}
	paused := types.RunLimits{PauseIdleAfterSec: 900}
	capAt := time.Now().Add(time.Hour)
	for _, tc := range []struct {
		name string
		run  types.AgentRun
		want []string
	}{
		{"a capped service under an idle pause", types.AgentRun{Service: svc, RunLimits: paused, LifetimeCapAt: &capAt}, []string{servicePauseWarning}},
		{"a capped service with no idle pause", types.AgentRun{Service: svc, LifetimeCapAt: &capAt}, nil},
		{"an uncapped service", types.AgentRun{Service: svc}, []string{serviceNoCapWarning}},
		{"an uncapped service under an idle pause", types.AgentRun{Service: svc, RunLimits: paused}, []string{servicePauseWarning, serviceNoCapWarning}},
		{"a run that is not a service", types.AgentRun{RunLimits: paused}, nil},
	} {
		if got := serviceCreateWarnings(tc.run); !slices.Equal(got, tc.want) {
			t.Errorf("%s: warnings %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestServiceNoCapWarning: the exact sentence, present with no lifetime cap and absent with one, at the
// two dry doors' shared helper and on a queued run's 201.
func TestServiceNoCapWarning(t *testing.T) {
	const want = "No lifetime limit applies to this run, so Wardyn will not keep renewing it: the service ends at its lease like any run. Ask your admin to set a lifetime limit on the pool to keep a service up."
	if serviceNoCapWarning != want {
		t.Fatalf("serviceNoCapWarning = %q, want %q", serviceNoCapWarning, want)
	}
	for _, c := range []struct {
		name            string
		service, capped bool
		want            []string
	}{
		{"service, no cap", true, false, []string{want}},
		{"service, capped", true, true, nil},
		{"no service, no cap", false, false, nil},
	} {
		if got := serviceWarnings(c.service, 0, c.capped); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	queued := types.AgentRun{State: types.RunQueued, Service: &types.RunService{Restart: types.ServiceRestartOnFailure}}
	if got := queuedServiceWarnings(queued, types.RunLimits{}, createEnd{}); !slices.Equal(got, []string{want}) {
		t.Errorf("queued, no cap: %q, want the no-cap sentence", got)
	}
	if got := queuedServiceWarnings(queued, types.RunLimits{}, createEnd{capSec: 3600}); got != nil {
		t.Errorf("queued, capped: %q, want none", got)
	}
}

// TestQueuedServiceWarnings: a queued run captures no limits until it starts, so its 201 reads the
// limits the launch resolved, not the empty ones on the queued row.
func TestQueuedServiceWarnings(t *testing.T) {
	svc := &types.RunService{Restart: types.ServiceRestartOnFailure}
	queued := types.AgentRun{State: types.RunQueued, Service: svc}
	capped := createEnd{capSec: 3600}
	if got := queuedServiceWarnings(queued, types.RunLimits{PauseIdleAfterSec: 900}, capped); !slices.Equal(got, []string{servicePauseWarning}) {
		t.Errorf("under an idle pause: %q, want the pause sentence", got)
	}
	if got := queuedServiceWarnings(queued, types.RunLimits{}, capped); got != nil {
		t.Errorf("with no idle pause: %q, want none", got)
	}
	if got := queuedServiceWarnings(types.AgentRun{State: types.RunQueued}, types.RunLimits{PauseIdleAfterSec: 900}, capped); got != nil {
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

// TestServicePauseWarnings: the sentence appears only for a service under an idle pause.
func TestServicePauseWarnings(t *testing.T) {
	for _, c := range []struct {
		name    string
		service bool
		pause   int
		want    []string
	}{
		{"service and pause", true, 1800, []string{servicePauseWarning}},
		{"no service", false, 1800, nil},
		{"no pause", true, 0, nil},
	} {
		if got := servicePauseWarnings(c.service, c.pause); !slices.Equal(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
