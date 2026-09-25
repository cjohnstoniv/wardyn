// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The DOCKER half of the substrate pass-through. A sidecar container inherits
// nothing from wardynd's process, so a knob the proxy reads from its own
// environment is not "off" here — it is UNREACHABLE: the operator sets it,
// nothing refuses it, the docs say what it does, and the control silently never
// applies. The k8s driver has the same pin (TestCreateSandbox_ProxyPodCarriesTheOperatorKnobs);
// the list is shared (runner.ProxySidecarEnvKnobs) so a knob added later cannot
// land on one substrate only.
func TestProxyEnv_CarriesTheOperatorKnobs(t *testing.T) {
	t.Setenv("WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS", "on")
	t.Setenv("WARDYN_LLM_SCAN", "off")
	// The re-auth hold's budget: the ONE number that decides how long a sandbox's
	// AWS SSO credential exchange is parked while its owner signs in again. A
	// deployment whose SDK is less patient than the hold lowers it — and could
	// not, on either substrate, until it rode this list.
	t.Setenv("WARDYN_CREDENTIAL_REAUTH_TIMEOUT", "45s")

	env, err := proxyEnv(uuid.New(), runner.ProxyConfig{ControlPlaneURL: "http://wardynd:8080"}, runner.ProxyListenPort)
	if err != nil {
		t.Fatalf("proxyEnv: %v", err)
	}
	got := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			got[k] = v
		}
	}
	for name, want := range map[string]string{
		"WARDYN_GIT_PAT_BROKER_ENFORCE_BRANCH_NS": "on",
		"WARDYN_LLM_SCAN":                         "off",
		"WARDYN_CREDENTIAL_REAUTH_TIMEOUT":        "45s",
	} {
		if got[name] != want {
			t.Errorf("proxy container env %s = %q, want %q", name, got[name], want)
		}
	}
	if got["WARDYN_CONTROL_PLANE_URL"] == "" || got["WARDYN_RUN_ID"] == "" {
		t.Error("the pass-through clobbered the environment the sidecar always carries")
	}
}

// A knob wardynd does not have set must not arrive as an EMPTY value: every
// reader treats "" as "unset" today, but an empty value is a value, and pinning
// it ABSENT keeps a future reader that distinguishes the two honest.
func TestProxyEnv_CarriesNoUnsetKnob(t *testing.T) {
	for _, k := range runner.ProxySidecarEnvKnobNames() {
		t.Setenv(k, "") // t.Setenv restores it; Unsetenv is what actually clears it
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}
	env, err := proxyEnv(uuid.New(), runner.ProxyConfig{ControlPlaneURL: "http://wardynd:8080"}, runner.ProxyListenPort)
	if err != nil {
		t.Fatalf("proxyEnv: %v", err)
	}
	for _, kv := range env {
		for _, k := range runner.ProxySidecarEnvKnobNames() {
			if strings.HasPrefix(kv, k+"=") {
				t.Errorf("an unset knob reached the sidecar as %q", kv)
			}
		}
	}
}
