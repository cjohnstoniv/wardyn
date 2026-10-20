// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
)

// CLI sign-in is off unless WARDYN_CLI_SIGN_IN is set, paste-back is off unless WARDYN_CLI_PASTE_SIGN_IN is
// set, and the file store is allowed unless WARDYN_CLI_FILE_STORE is off. Boot refuses a session TTL outside
// 1h..90 days and an idle lifetime longer than the TTL in effect. The proof origin is the SSO redirect URL's
// scheme, host and base path.
func TestCLISignInFlags(t *testing.T) {
	d := func(v time.Duration) *time.Duration { return &v }
	b := func(v bool) *bool { return &v }
	var unset cliSignInFlags
	if cfg := unset.config(""); cfg.Enabled || cfg.Paste || cfg.FileStoreOff {
		t.Fatalf("unregistered flags = %+v, want everything off and the file store allowed", cfg)
	}
	for _, c := range []struct {
		ttl, idle time.Duration
		refused   string
	}{
		{0, 0, ""},
		{api.MaxCLISessionTTL, 0, ""},
		{time.Hour, time.Hour, ""},
		{30 * time.Minute, 0, "WARDYN_CLI_SESSION_TTL"},
		{api.MaxCLISessionTTL + time.Hour, 0, "WARDYN_CLI_SESSION_TTL"},
		{0, api.DefaultCLISessionTTL + time.Hour, "WARDYN_CLI_SESSION_IDLE"},
		{2 * time.Hour, 3 * time.Hour, "WARDYN_CLI_SESSION_IDLE"},
		{0, -time.Second, "WARDYN_CLI_SESSION_IDLE"},
	} {
		err := cliSignInFlags{ttl: d(c.ttl), idle: d(c.idle)}.validate()
		if (err == nil) != (c.refused == "") || (err != nil && !strings.Contains(err.Error(), c.refused)) {
			t.Errorf("ttl %s idle %s: validate = %v, want refusal naming %q", c.ttl, c.idle, err, c.refused)
		}
	}
	cfg := cliSignInFlags{enabled: b(true), paste: b(true), fileStore: b(false), ttl: d(48 * time.Hour)}.config("https://wardyn.corp.example/wardyn/auth/callback")
	want := api.CLISignInConfig{Enabled: true, Paste: true, FileStoreOff: true, SessionTTL: 48 * time.Hour, Origin: "https://wardyn.corp.example/wardyn"}
	if cfg != want {
		t.Fatalf("config = %+v, want %+v", cfg, want)
	}
}

// Boot warns when WARDYN_CLI_SIGN_IN is on without SSO, the one case where the switch is silently off.
func TestCLISignInWarnsWithoutSSO(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name       string
		enabled    *bool
		sso, warns bool
	}{
		{"on without SSO", &on, false, true},
		{"on with SSO", &on, true, false},
		{"off without SSO", &off, false, false},
		{"unregistered", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			cliSignInFlags{enabled: tc.enabled}.warnWithoutSSO(tc.sso)
			out := buf.String()
			if warned := strings.Contains(out, "level=WARN") && strings.Contains(out, "WARDYN_CLI_SIGN_IN is set but SSO is not configured"); warned != tc.warns {
				t.Errorf("warned = %v, want %v; log: %q", warned, tc.warns, out)
			}
		})
	}
}
