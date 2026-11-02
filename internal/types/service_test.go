// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
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
