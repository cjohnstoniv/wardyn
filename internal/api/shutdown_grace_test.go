// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"gopkg.in/yaml.v3"
)

// shutdownSinkMargin is what the pin leaves for the audit sinks' final flush
// (Fanout.Close, which has no bound of its own) after the two budgets. The
// slowest sink to close is the webhook one, bounded by its own HTTP client
// timeout — derived, not a second hard-coded copy of that number.
const shutdownSinkMargin = sinks.WebhookTimeout

// TestShutdownGraceCoversTheBudget: wardynd's orderly stop is
// http.Server.Shutdown (HTTPShutdownTimeout), then WaitBackground
// (backgroundShutdownBudget), then the audit sinks. A platform grace period
// shorter than that SIGKILLs a detached teardown mid-KillSandbox — the run reads
// KILLED with its sandbox up and no run.kill row. Both shipped deployments must
// give the whole sequence room.
func TestShutdownGraceCoversTheBudget(t *testing.T) {
	need := HTTPShutdownTimeout + backgroundShutdownBudget + shutdownSinkMargin

	var values struct {
		Grace int `yaml:"terminationGracePeriodSeconds"`
	}
	readYAML(t, "../../deploy/helm/wardyn/values.yaml", &values)
	if got := time.Duration(values.Grace) * time.Second; got < need {
		t.Errorf("helm terminationGracePeriodSeconds = %s, want >= %s (HTTP %s + background %s + sinks %s)",
			got, need, HTTPShutdownTimeout, backgroundShutdownBudget, shutdownSinkMargin)
	}

	var compose struct {
		Services map[string]struct {
			StopGrace string `yaml:"stop_grace_period"`
		} `yaml:"services"`
	}
	readYAML(t, "../../deploy/compose/docker-compose.yaml", &compose)
	got, err := time.ParseDuration(compose.Services["wardynd"].StopGrace)
	if err != nil {
		t.Fatalf("compose wardynd stop_grace_period %q: %v", compose.Services["wardynd"].StopGrace, err)
	}
	if got < need {
		t.Errorf("compose wardynd stop_grace_period = %s, want >= %s", got, need)
	}
}

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}
