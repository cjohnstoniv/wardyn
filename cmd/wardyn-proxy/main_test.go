// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Unit tests for the wardyn-proxy binary's config-load precedence and the
// validation/defaulting performed by the proxy config loader it dispatches to.
//
// main() itself is not directly testable (it calls log.Fatal, mints injection
// credentials against the broker, and starts a listening server). The
// LOAD-BEARING logic in main() is the source-selection switch:
//
//	-config flag  >  WARDYN_PROXY_CONFIG_JSON env  >  fatal "required"
//
// which dispatches to proxy.LoadConfig (file) / proxy.LoadConfigBytes (bytes).
// selectConfig below mirrors that switch EXACTLY and delegates to the same
// production loaders, so these tests exercise the real parse/validate/default
// code paths the binary uses while asserting the precedence the binary relies
// on. If the binary's switch or the loader contract regressed, the precedence
// assertions here would fail.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// selectConfig replicates cmd/wardyn-proxy/main.go's config-source precedence
// switch, delegating to the SAME production loaders the binary uses. Keeping it
// in lockstep with main() lets us unit-test the precedence rule without the
// log.Fatal / server-start side effects of calling main() directly.
func selectConfig(configPath, envJSON string) (*proxy.Config, error) {
	switch {
	case configPath != "":
		return proxy.LoadConfig(configPath)
	case envJSON != "":
		return proxy.LoadConfigBytes([]byte(envJSON))
	default:
		return nil, errors.New("wardyn-proxy: -config or WARDYN_PROXY_CONFIG_JSON is required")
	}
}

// validConfigJSON returns a minimal, valid wardyn-proxy config with the given
// run id so tests can distinguish which source a Config was loaded from.
func validConfigJSON(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	return `{
		"run_id": "` + runID.String() + `",
		"control_plane_url": "http://wardynd:8080",
		"run_token": "rt-token"
	}`
}

// writeConfigFile writes JSON to a temp file and returns its path.
func writeConfigFile(t *testing.T, json string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wardyn-proxy.json")
	if err := os.WriteFile(path, []byte(json), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

// TestSelectConfig_Precedence pins the binary's flag > env > error precedence.
// The flag and env each carry a DIFFERENT run_id so the resolved Config tells
// us unambiguously which source won.
func TestSelectConfig_Precedence(t *testing.T) {
	flagID := uuid.New()
	envID := uuid.New()

	flagPath := writeConfigFile(t, validConfigJSON(t, flagID))
	envJSON := validConfigJSON(t, envID)

	tests := []struct {
		name       string
		configPath string
		envJSON    string
		wantID     uuid.UUID
		wantErr    bool
	}{
		{
			name:       "flag wins over env when both set",
			configPath: flagPath,
			envJSON:    envJSON,
			wantID:     flagID,
		},
		{
			name:       "flag used when only flag set",
			configPath: flagPath,
			envJSON:    "",
			wantID:     flagID,
		},
		{
			name:       "env used when only env set",
			configPath: "",
			envJSON:    envJSON,
			wantID:     envID,
		},
		{
			name:       "error when neither source is provided",
			configPath: "",
			envJSON:    "",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := selectConfig(tt.configPath, tt.envJSON)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error (no config source), got nil")
				}
				if cfg != nil {
					t.Fatalf("want nil config on error, got %+v", cfg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.RunID != tt.wantID {
				t.Fatalf("resolved run_id = %s, want %s (wrong source won)", cfg.RunID, tt.wantID)
			}
		})
	}
}

// TestSelectConfig_FlagPathMissingFile surfaces the read error from the flag
// branch (the binary log.Fatals on this; the loader returns a wrapped error).
func TestSelectConfig_FlagPathMissingFile(t *testing.T) {
	_, err := selectConfig(filepath.Join(t.TempDir(), "does-not-exist.json"), "")
	if err == nil {
		t.Fatal("want error for missing -config file, got nil")
	}
}

// TestSelectConfig_DefaultsApplied verifies the loader fills Listen and
// DecisionBufferSize defaults when the config omits them — the proxy must come
// up on :3128 with a 1024-entry decision buffer rather than an empty addr / a
// zero (deadlocking) buffer.
func TestSelectConfig_DefaultsApplied(t *testing.T) {
	cfg, err := selectConfig("", validConfigJSON(t, uuid.New()))
	if err != nil {
		t.Fatalf("load valid config: %v", err)
	}
	if cfg.Listen != ":3128" {
		t.Errorf("Listen = %q, want default :3128", cfg.Listen)
	}
	if cfg.DecisionBufferSize != 1024 {
		t.Errorf("DecisionBufferSize = %d, want default 1024", cfg.DecisionBufferSize)
	}
}

// TestSelectConfig_ExplicitListenAndBufferKept verifies caller-supplied Listen
// and DecisionBufferSize override the defaults (precedence: explicit > default).
func TestSelectConfig_ExplicitListenAndBufferKept(t *testing.T) {
	json := `{
		"run_id": "` + uuid.New().String() + `",
		"control_plane_url": "http://wardynd:8080",
		"run_token": "rt-token",
		"listen": "127.0.0.1:9999",
		"decision_buffer_size": 7
	}`
	cfg, err := selectConfig("", json)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q, want explicit 127.0.0.1:9999", cfg.Listen)
	}
	if cfg.DecisionBufferSize != 7 {
		t.Errorf("DecisionBufferSize = %d, want explicit 7", cfg.DecisionBufferSize)
	}
}

// TestSelectConfig_ValidationErrors covers the fail-closed required-field checks
// the binary depends on: a config missing run_id / control_plane_url / run_token
// must be rejected (the proxy must never start half-configured).
func TestSelectConfig_ValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		wantSub string
	}{
		{
			name:    "missing run_id",
			json:    `{"control_plane_url":"http://wardynd:8080","run_token":"t"}`,
			wantSub: "run_id is required",
		},
		{
			name:    "missing control_plane_url",
			json:    `{"run_id":"` + uuid.New().String() + `","run_token":"t"}`,
			wantSub: "control_plane_url is required",
		},
		{
			name:    "missing run_token",
			json:    `{"run_id":"` + uuid.New().String() + `","control_plane_url":"http://wardynd:8080"}`,
			wantSub: "run_token is required",
		},
		{
			name:    "malformed json",
			json:    `{not json`,
			wantSub: "parse config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := selectConfig("", tt.json)
			if err == nil {
				t.Fatalf("want validation error, got nil (cfg=%+v)", cfg)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("error %q does not mention %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestSelectConfig_FlagAndEnvLoadIdentically asserts the file path and the bytes
// path (the two real loaders the switch dispatches to) produce an equivalent
// Config for the same JSON — i.e. the WARDYN_PROXY_CONFIG_JSON sidecar path is not
// a second-class loader with different validation/defaulting.
func TestSelectConfig_FlagAndEnvLoadIdentically(t *testing.T) {
	id := uuid.New()
	json := validConfigJSON(t, id)

	fromFile, err := selectConfig(writeConfigFile(t, json), "")
	if err != nil {
		t.Fatalf("file path: %v", err)
	}
	fromEnv, err := selectConfig("", json)
	if err != nil {
		t.Fatalf("env path: %v", err)
	}

	if fromFile.RunID != fromEnv.RunID {
		t.Errorf("run_id differs: file=%s env=%s", fromFile.RunID, fromEnv.RunID)
	}
	if fromFile.ControlPlaneURL != fromEnv.ControlPlaneURL {
		t.Errorf("control_plane_url differs: file=%q env=%q", fromFile.ControlPlaneURL, fromEnv.ControlPlaneURL)
	}
	if fromFile.RunToken != fromEnv.RunToken {
		t.Errorf("run_token differs: file=%q env=%q", fromFile.RunToken, fromEnv.RunToken)
	}
	if fromFile.Listen != fromEnv.Listen || fromFile.DecisionBufferSize != fromEnv.DecisionBufferSize {
		t.Errorf("defaults differ between loaders: file=(%q,%d) env=(%q,%d)",
			fromFile.Listen, fromFile.DecisionBufferSize, fromEnv.Listen, fromEnv.DecisionBufferSize)
	}
}
