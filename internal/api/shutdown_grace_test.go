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

// TestShutdownSinkMarginCoversSyslogClose: Fanout.Close closes the sinks
// concurrently, so the margin has to cover the slowest, and the syslog sink's
// Close is bounded by its own constant rather than by an HTTP client timeout.
func TestShutdownSinkMarginCoversSyslogClose(t *testing.T) {
	if sinks.SyslogCloseTimeout > shutdownSinkMargin {
		t.Errorf("sinks.SyslogCloseTimeout = %s exceeds the sink margin %s that the shutdown grace is sized on",
			sinks.SyslogCloseTimeout, shutdownSinkMargin)
	}
}

// TestShutdownGraceCoversTheBudget: wardynd's orderly stop is
// http.Server.Shutdown (HTTPShutdownTimeout), then WaitBackground
// (backgroundShutdownBudget), then the audit sinks. A platform grace period
// shorter than that SIGKILLs a detached teardown mid-KillSandbox — the run reads
// KILLED with its sandbox up and no run.kill row. Both shipped deployments must
// give the whole sequence room.
func TestShutdownGraceCoversTheBudget(t *testing.T) {
	// The longest tracked goroutine is the post-capture sign-in kill: its run read, the kill cascade, and
	// the revokes, run.kill row and reconciles that follow it on their own killRevokeTimeout window.
	if want := signInCaptureReadTimeout + killCascadeTimeout + killRevokeTimeout; backgroundShutdownBudget < want {
		t.Errorf("backgroundShutdownBudget = %s, want >= %s (read %s + kill cascade %s + revoke window %s): a shutdown would cut off in-flight revokes",
			backgroundShutdownBudget, want, signInCaptureReadTimeout, killCascadeTimeout, killRevokeTimeout)
	}
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

// TestShutdownBudgetCoversAnSSHDoorsLastRow: a daemon stop waits for the SSH
// gateway's connections and doors inside WaitBackground (sshGoTracked), so the
// last audit row each writes is over before the pool closes and the process
// exits, and a SIGKILL at the grace's end finds none in flight. Each bounds its
// own work at a stop below that wait's budget: a handshake by its pre-auth
// deadline plus one authentication store call, a refused connection by its
// refusal grace, and a door's start-failure row by heldStoreCallBound; a door's
// last row runs on the stopped daemon's context, so its store write fails at
// once and the audit spool takes it. Catches: one of those bounds raised past
// the budget.
func TestShutdownBudgetCoversAnSSHDoorsLastRow(t *testing.T) {
	for name, d := range map[string]time.Duration{
		"a handshake and its authentication call": sshHandshakeTimeout() + sshAuthTimeout,
		"a refused connection's grace":            sshRefusalGraceFor(),
		"a start-failure row":                     heldStoreCallBound,
	} {
		if d >= backgroundShutdownBudget {
			t.Errorf("%s is bounded at %s, want below backgroundShutdownBudget (%s)", name, d, backgroundShutdownBudget)
		}
	}
}
