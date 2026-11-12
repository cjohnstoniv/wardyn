// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"regexp"
	"time"
)

// ServiceProbeControlPlane is ServiceHealth.Probe's one value besides off: wardynd checks the service's port itself,
// through the sandbox's exec path, beside the supervisor's own check.
const ServiceProbeControlPlane = "control_plane"

// ServiceRestart is what the supervisor does when a service's command exits with an error.
type ServiceRestart string

const (
	ServiceRestartOnFailure ServiceRestart = "on_failure" // default
	ServiceRestartNever     ServiceRestart = "never"
)

// Defaults and bounds for a service's health check, filled and enforced by ValidateRunService.
const (
	DefaultServiceHealthIntervalSec = 15
	MinServiceHealthIntervalSec     = 5
	MaxServiceHealthIntervalSec     = 300
	DefaultServiceHealthFailures    = 3
	MinServiceHealthFailures        = 1
	MaxServiceHealthFailures        = 10
)

// serviceHealthPathRE bounds a health path to a request-target the supervisor can write into an HTTP request line.
var serviceHealthPathRE = regexp.MustCompile(`^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]{0,255}$`)

// RunService is the spec of a background run that is kept running: frozen on the run at create
// (AgentRun.Service), defaults filled. Its presence is what makes a run a service.
type RunService struct {
	Restart ServiceRestart `json:"restart"`
	// Port is the port the service listens on, 127.0.0.1 inside the sandbox; 0 = none declared. A port
	// alone is not checked: the supervisor checks the service only when Health is set too.
	Port   int            `json:"port,omitempty"`
	Health *ServiceHealth `json:"health,omitempty"` // needs Port
}

// ServiceHealth is how a service is checked: an HTTP GET of Path (2xx/3xx passes), or a TCP connect when Path is empty.
type ServiceHealth struct {
	Path             string `json:"path,omitempty"`
	IntervalSec      int    `json:"interval_sec"`
	FailureThreshold int    `json:"failure_threshold"` // consecutive failures before the service reads unhealthy
	// Probe is "" (off, the default) or ServiceProbeControlPlane. It is frozen with the spec at create, like the rest.
	Probe string `json:"probe,omitempty"`
}

// ControlPlaneProbe reports whether wardynd checks the service's port itself (ServiceHealth.Probe). Nil is no service.
func (s *RunService) ControlPlaneProbe() bool {
	return s != nil && s.Health != nil && s.Health.Probe == ServiceProbeControlPlane
}

// ServiceHealthState is the health wardynd last observed for a service.
type ServiceHealthState string

const (
	ServiceHealthStarting  ServiceHealthState = "starting"
	ServiceHealthHealthy   ServiceHealthState = "healthy"
	ServiceHealthUnhealthy ServiceHealthState = "unhealthy"
	ServiceHealthUnknown   ServiceHealthState = "unknown"
)

// Valid reports whether s is one of the four health states.
func (s ServiceHealthState) Valid() bool {
	switch s {
	case ServiceHealthStarting, ServiceHealthHealthy, ServiceHealthUnhealthy, ServiceHealthUnknown:
		return true
	}
	return false
}

// RunServiceStatus is the last observation of a service (AgentRun.ServiceStatus); absent until the first one.
type RunServiceStatus struct {
	Health ServiceHealthState `json:"health"`
	// Reason is closed: restarting | probe_failing | supervisor_missing | unreachable, and on the wire paused
	// (the API serves a paused run's health as unknown/paused, and a stale one as unknown/unreachable).
	Reason       string    `json:"reason,omitempty"`
	Restarts     int       `json:"restarts"`
	LastExitCode *int      `json:"last_exit_code,omitempty"`
	ObservedAt   time.Time `json:"observed_at"` // wardynd's clock, never the sandbox's
	// Probe is wardynd's own check of the port, present only when the spec opted in (ServiceHealth.Probe) and the
	// check has run.
	Probe *RunServiceProbe `json:"probe,omitempty"`
}

// RunServiceProbe is the last result of wardynd's own check of a service's port.
type RunServiceProbe struct {
	Health ServiceHealthState `json:"health"`
	// Reason is closed: refused | timeout | bad_status | bad_answer | not_run, and on the wire paused and unreachable
	// (as RunServiceStatus.Reason). A failure below the threshold keeps the last health and names its finding here.
	Reason     string    `json:"reason,omitempty"`
	HTTPStatus int       `json:"http_status,omitempty"` // the status line's code, when the check read one
	Failures   int       `json:"failures"`              // consecutive failed checks
	ObservedAt time.Time `json:"observed_at"`           // wardynd's clock
}

// serviceHealthRank orders the states worst first; a value outside the four ranks as unknown.
func serviceHealthRank(s ServiceHealthState) int {
	switch s {
	case ServiceHealthUnhealthy:
		return 0
	case ServiceHealthStarting:
		return 2
	case ServiceHealthHealthy:
		return 3
	}
	return 1
}

// WorseServiceHealth is the worse of two readings, worst first: unhealthy, unknown, starting, healthy. A value
// outside the four reads unknown.
func WorseServiceHealth(a, b ServiceHealthState) ServiceHealthState {
	if serviceHealthRank(b) < serviceHealthRank(a) {
		a = b
	}
	if !a.Valid() {
		return ServiceHealthUnknown
	}
	return a
}

// CombinedHealth is the health a service is shown with: the sandbox's reading, or the worse of it and wardynd's
// own check when that has a reading.
func (st RunServiceStatus) CombinedHealth() ServiceHealthState {
	if st.Probe == nil {
		return st.Health
	}
	return WorseServiceHealth(st.Health, st.Probe.Health)
}

// ServiceFieldError is a ValidateRunService refusal that names the request field it is about.
type ServiceFieldError struct {
	Field string
	Msg   string
}

func (e *ServiceFieldError) Error() string { return e.Msg }

// ValidateRunService validates s in place and fills its defaults: restart on_failure, and a health check's
// interval and threshold. A nil s is no service. Whether the run's shape may carry a service is the caller's call.
func ValidateRunService(s *RunService) error {
	if s == nil {
		return nil
	}
	switch s.Restart {
	case "":
		s.Restart = ServiceRestartOnFailure
	case ServiceRestartOnFailure, ServiceRestartNever:
	default:
		return fmt.Errorf("service.restart: invalid %q", s.Restart)
	}
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("service.port: %d is outside 1..65535", s.Port)
	}
	h := s.Health
	if h == nil {
		return nil
	}
	if s.Port == 0 {
		return fmt.Errorf("service.health: needs service.port")
	}
	if h.Path != "" && !serviceHealthPathRE.MatchString(h.Path) {
		return fmt.Errorf("service.health.path: must start with / and be at most 256 URL characters")
	}
	switch h.Probe {
	case "", ServiceProbeControlPlane:
	default:
		return &ServiceFieldError{Field: "service.health.probe", Msg: fmt.Sprintf("service.health.probe: invalid %q", h.Probe)}
	}
	if h.IntervalSec == 0 {
		h.IntervalSec = DefaultServiceHealthIntervalSec
	}
	if h.IntervalSec < MinServiceHealthIntervalSec || h.IntervalSec > MaxServiceHealthIntervalSec {
		return fmt.Errorf("service.health.interval_sec: %d is outside %d..%d",
			h.IntervalSec, MinServiceHealthIntervalSec, MaxServiceHealthIntervalSec)
	}
	if h.FailureThreshold == 0 {
		h.FailureThreshold = DefaultServiceHealthFailures
	}
	if h.FailureThreshold < MinServiceHealthFailures || h.FailureThreshold > MaxServiceHealthFailures {
		return fmt.Errorf("service.health.failure_threshold: %d is outside %d..%d",
			h.FailureThreshold, MinServiceHealthFailures, MaxServiceHealthFailures)
	}
	return nil
}
