// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"regexp"
	"time"
)

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
}

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
