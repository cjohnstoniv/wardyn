// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
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
