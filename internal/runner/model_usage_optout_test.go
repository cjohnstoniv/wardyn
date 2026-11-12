// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// WARDYN_MODEL_USAGE=off on wardynd reaches every proxy it starts as an environment variable, and the
// proxy config it is handed carries no new key, so a proxy older than the meter starts on it and does not
// count. The proxy itself turns the variable into its meter-off setting (cmd/wardyn-proxy).
func TestModelUsageOptOutRidesTheProxyEnvironmentNotTheConfig(t *testing.T) {
	t.Setenv("WARDYN_MODEL_USAGE", "off")
	var got string
	for _, kv := range runner.ProxySidecarEnvKnobs() {
		if kv[0] == "WARDYN_MODEL_USAGE" {
			got = kv[1]
		}
	}
	if got != "off" {
		t.Fatalf("the sidecar environment carries WARDYN_MODEL_USAGE=%q, want off", got)
	}
	raw, err := runner.BuildProxyConfig(uuid.New(), runner.ProxyConfig{ControlPlaneURL: "http://wardynd:8080"}, runner.ProxyListenPort)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["disable_model_usage"]; ok {
		t.Fatalf("the opt-out is in the rendered proxy config, which a proxy older than the meter refuses: %s", raw)
	}
	var cfg proxy.Config
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.DisableModelUsage {
		t.Fatalf("the rendered config must leave the meter to the environment: %v", err)
	}
}
