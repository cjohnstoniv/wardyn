// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateRunService_NilIsNoService(t *testing.T) {
	if err := ValidateRunService(nil); err != nil {
		t.Fatalf("nil service: %v", err)
	}
}

func TestValidateRunService_FillsDefaults(t *testing.T) {
	s := &RunService{Port: 8080, Health: &ServiceHealth{}}
	if err := ValidateRunService(s); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if s.Restart != ServiceRestartOnFailure || s.Health.IntervalSec != 15 || s.Health.FailureThreshold != 3 {
		t.Fatalf("defaults not filled: %+v health %+v", s, s.Health)
	}
	bare := &RunService{}
	if err := ValidateRunService(bare); err != nil || bare.Restart != ServiceRestartOnFailure || bare.Health != nil {
		t.Fatalf("bare service: %+v err %v", bare, err)
	}
}

func TestValidateRunService_Refusals(t *testing.T) {
	cases := []struct {
		name string
		s    RunService
		want string
	}{
		{"restart", RunService{Restart: "always"}, "service.restart"},
		{"port low", RunService{Port: -1}, "service.port"},
		{"port high", RunService{Port: 65536}, "service.port"},
		{"health needs port", RunService{Health: &ServiceHealth{}}, "needs service.port"},
		{"path no slash", RunService{Port: 80, Health: &ServiceHealth{Path: "healthz"}}, "service.health.path"},
		{"path space", RunService{Port: 80, Health: &ServiceHealth{Path: "/a b"}}, "service.health.path"},
		{"path crlf", RunService{Port: 80, Health: &ServiceHealth{Path: "/a\r\nHost: x"}}, "service.health.path"},
		{"path too long", RunService{Port: 80, Health: &ServiceHealth{Path: "/" + strings.Repeat("a", 256)}}, "service.health.path"},
		{"interval low", RunService{Port: 80, Health: &ServiceHealth{IntervalSec: 4}}, "interval_sec"},
		{"interval high", RunService{Port: 80, Health: &ServiceHealth{IntervalSec: 301}}, "interval_sec"},
		{"interval negative", RunService{Port: 80, Health: &ServiceHealth{IntervalSec: -5}}, "interval_sec"},
		{"threshold high", RunService{Port: 80, Health: &ServiceHealth{FailureThreshold: 11}}, "failure_threshold"},
		{"threshold negative", RunService{Port: 80, Health: &ServiceHealth{FailureThreshold: -1}}, "failure_threshold"},
		{"probe unknown", RunService{Port: 80, Health: &ServiceHealth{Probe: "tcp"}}, `service.health.probe: invalid "tcp"`},
		{"probe case", RunService{Port: 80, Health: &ServiceHealth{Probe: "Control_Plane"}}, "service.health.probe"},
		{"probe external", RunService{Port: 80, Health: &ServiceHealth{Probe: "external"}}, "service.health.probe"},
	}
	for _, c := range cases {
		err := ValidateRunService(&c.s)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want it to mention %q", c.name, err, c.want)
		}
	}
}

func TestValidateRunService_Accepts(t *testing.T) {
	for _, s := range []RunService{
		{Restart: ServiceRestartNever, Port: 65535},
		{Port: 1, Health: &ServiceHealth{Path: "/", IntervalSec: 5, FailureThreshold: 1}},
		{Port: 80, Health: &ServiceHealth{Path: "/a/b-c_d.e~f", IntervalSec: 300, FailureThreshold: 10}},
		{Port: 80, Health: &ServiceHealth{Path: "/" + strings.Repeat("a", 255)}},
		{Port: 80, Health: &ServiceHealth{Probe: ServiceProbeControlPlane}},
		{Port: 80, Health: &ServiceHealth{Path: "/healthz", Probe: ServiceProbeControlPlane}},
	} {
		s := s
		err := ValidateRunService(&s)
		if err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
}

func TestRunServiceStatus_Wire(t *testing.T) {
	b, err := json.Marshal(RunServiceStatus{Health: ServiceHealthStarting})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"health":"starting"`) || !strings.Contains(string(b), `"restarts":0`) || strings.Contains(string(b), "last_exit_code") {
		t.Fatalf("wire: %s", b)
	}
	for _, h := range []ServiceHealthState{ServiceHealthStarting, ServiceHealthHealthy, ServiceHealthUnhealthy, ServiceHealthUnknown} {
		if !h.Valid() {
			t.Errorf("%q not valid", h)
		}
	}
	if ServiceHealthState("down").Valid() {
		t.Error("unknown state valid")
	}
}

// A refused probe names its request field, so the API can say which field it is; the message is the server's
// sentence as the console shows it raw.
func TestValidateRunService_ProbeRefusalNamesItsField(t *testing.T) {
	err := ValidateRunService(&RunService{Port: 80, Health: &ServiceHealth{Probe: "tcp"}})
	var fe *ServiceFieldError
	if !errors.As(err, &fe) || fe.Field != "service.health.probe" || err.Error() != `service.health.probe: invalid "tcp"` {
		t.Fatalf("err = %#v", err)
	}
}

// The probe is off unless the spec names it, and it rides only a health check (which needs a port).
func TestRunService_ControlPlaneProbe(t *testing.T) {
	var none *RunService
	for name, c := range map[string]struct {
		s    *RunService
		want bool
	}{
		"no service":        {none, false},
		"no health":         {&RunService{Port: 80}, false},
		"health, probe off": {&RunService{Port: 80, Health: &ServiceHealth{}}, false},
		"probe on":          {&RunService{Port: 80, Health: &ServiceHealth{Probe: ServiceProbeControlPlane}}, true},
	} {
		if got := c.s.ControlPlaneProbe(); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// The spec's probe is omitted while off, so a spec frozen before it existed reads and writes the same bytes.
func TestServiceHealth_ProbeOffIsOmitted(t *testing.T) {
	b, err := json.Marshal(ServiceHealth{Path: "/h", IntervalSec: 15, FailureThreshold: 3})
	if err != nil || string(b) != `{"path":"/h","interval_sec":15,"failure_threshold":3}` {
		t.Fatalf("off: %s %v", b, err)
	}
	st, err := json.Marshal(RunServiceStatus{Health: ServiceHealthHealthy})
	if err != nil || strings.Contains(string(st), "probe") {
		t.Fatalf("status without a probe reading: %s %v", st, err)
	}
	b, err = json.Marshal(ServiceHealth{IntervalSec: 15, FailureThreshold: 3, Probe: ServiceProbeControlPlane})
	if err != nil || !strings.Contains(string(b), `"probe":"control_plane"`) {
		t.Fatalf("on: %s %v", b, err)
	}
}

// The worse of two readings, worst first: unhealthy, unknown, starting, healthy; every pair, both orders.
func TestWorseServiceHealth(t *testing.T) {
	order := []ServiceHealthState{ServiceHealthUnhealthy, ServiceHealthUnknown, ServiceHealthStarting, ServiceHealthHealthy}
	for i, a := range order {
		for j, b := range order {
			want := order[min(i, j)]
			if got := WorseServiceHealth(a, b); got != want {
				t.Errorf("worse(%s, %s) = %s, want %s", a, b, got, want)
			}
		}
	}
	if got := WorseServiceHealth("down", ServiceHealthHealthy); got != ServiceHealthUnknown {
		t.Errorf("an unknown value reads %s, want unknown", got)
	}
	if got := WorseServiceHealth(ServiceHealthStarting, "down"); got != ServiceHealthUnknown {
		t.Errorf("an unknown value reads %s, want unknown", got)
	}
}

// The shown health is the sandbox's alone with no probe reading, and the worse of the two with one.
func TestRunServiceStatus_CombinedHealth(t *testing.T) {
	st := RunServiceStatus{Health: ServiceHealthHealthy}
	if got := st.CombinedHealth(); got != ServiceHealthHealthy {
		t.Fatalf("no probe: %s", got)
	}
	st.Probe = &RunServiceProbe{Health: ServiceHealthUnhealthy}
	if got := st.CombinedHealth(); got != ServiceHealthUnhealthy {
		t.Fatalf("probe unhealthy: %s", got)
	}
	st.Health, st.Probe.Health = ServiceHealthUnhealthy, ServiceHealthHealthy
	if got := st.CombinedHealth(); got != ServiceHealthUnhealthy {
		t.Fatalf("sandbox unhealthy: %s", got)
	}
}
