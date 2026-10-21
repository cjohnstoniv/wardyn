// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// TestBuildProxyConfig_SigninListenFollowsDesktopSignins: the sign-in
// listener is written only for a run that has desktop sign-ins.
func TestBuildProxyConfig_SigninListenFollowsDesktopSignins(t *testing.T) {
	for _, on := range []bool{false, true} {
		b, err := BuildProxyConfig(uuid.New(), ProxyConfig{DesktopSignins: on}, ProxyListenPort)
		if err != nil {
			t.Fatal(err)
		}
		var cfg proxy.Config
		if err := json.Unmarshal(b, &cfg); err != nil {
			t.Fatal(err)
		}
		if want := map[bool]string{false: "", true: ":3129"}[on]; cfg.SigninListen != want {
			t.Fatalf("DesktopSignins=%v: signin_listen = %q, want %q", on, cfg.SigninListen, want)
		}
	}
}
