// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
)

// WARDYN_DOCKER_PROXY_SANDBOX_PEERS_ONLY unset is on: the registered driver's proxies serve only
// their own run's network.
func TestProxyPeersOnlyEnv_UnsetIsOn(t *testing.T) {
	t.Setenv(EnvProxySandboxPeersOnly, "")
	if cfg := buildConfig(substrate.Deps{}, ""); cfg.ProxyServesAnyPeer {
		t.Errorf("%s unset: ProxyServesAnyPeer = true, want false (peers only)", EnvProxySandboxPeersOnly)
	}
}

// Set off, in any spelling EnvBool reads as false, the proxy serves any peer as before; set on, it
// does not.
func TestProxyPeersOnlyEnv_ExplicitOffServesAnyPeer(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want bool
	}{{"false", true}, {"0", true}, {"off", true}, {"no", true}, {"true", false}, {"1", false}} {
		t.Setenv(EnvProxySandboxPeersOnly, tc.env)
		if got := buildConfig(substrate.Deps{}, "").ProxyServesAnyPeer; got != tc.want {
			t.Errorf("%s=%s: ProxyServesAnyPeer = %v, want %v", EnvProxySandboxPeersOnly, tc.env, got, tc.want)
		}
	}
}

// A value that is not a boolean exits 2 naming the variable and the value; it never falls back to a
// default. cliutil's exit is os.Exit, so the read runs in a re-exec'd child.
func TestProxyPeersOnlyEnv_MalformedRefused(t *testing.T) {
	if os.Getenv("PROXY_PEERS_ENV_CHILD") == "1" {
		ProxyServesAnyPeerFromEnv()
		return // not refused: the child exits 0 and the parent fails
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProxyPeersOnlyEnv_MalformedRefused$", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), "PROXY_PEERS_ENV_CHILD=1", EnvProxySandboxPeersOnly+"=treu")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 2 {
		t.Fatalf("%s=treu: want exit 2; err=%v\n%s", EnvProxySandboxPeersOnly, err, out)
	}
	if !strings.Contains(string(out), EnvProxySandboxPeersOnly) || !strings.Contains(string(out), "treu") {
		t.Fatalf("the refusal must name the variable and the value:\n%s", out)
	}
}
