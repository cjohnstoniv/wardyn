// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
)

func trustedCAOf(t *testing.T, cfg []byte) string {
	t.Helper()
	var c struct {
		TrustedCAPEM string `json:"trusted_ca_pem"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		t.Fatalf("decode proxy config: %v", err)
	}
	return c.TrustedCAPEM
}

// A runner's own bundle reaches its proxy on create AND on revive (a revive rebuilds from the
// organisation's stored config, which carries no CA for a runner), and the sandbox's CA trust after
// whatever the organisation staged, without touching the caller's env map.
func TestRunnerTrustedCA_ProxyOnCreateAndReviveAndSandbox(t *testing.T) {
	const bundle = "runner-ca-pem"
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", InternalNetwork: "wardyn-internal", TrustedCAPEM: bundle})
	ctx := context.Background()
	spec := testSpec()
	spec.Env = map[string]string{"WARDYN_MITM_CA_PEM": "run-ca-pem\norg-ca-pem"}
	sb, err := d.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	name := proxyContainerName(spec.RunID)
	if got := trustedCAOf(t, f.stdinOf(name)); got != bundle {
		t.Errorf("created proxy trusted_ca_pem = %q, want the runner's bundle %q", got, bundle)
	}
	env := f.containers[sb.Ref].cfg.Env
	for _, want := range []string{"WARDYN_MITM_CA_PEM=run-ca-pem\norg-ca-pem\n" + bundle, "SSL_CERT_FILE=/tmp/wardyn/ca-bundle.pem", "AWS_CA_BUNDLE=/tmp/wardyn/ca-bundle.pem"} {
		if !slices.Contains(env, want) {
			t.Errorf("sandbox env lacks %q: %v", want, env)
		}
	}
	if spec.Env["WARDYN_MITM_CA_PEM"] != "run-ca-pem\norg-ca-pem" || len(spec.Env) != 1 {
		t.Errorf("the caller's env map changed: %v", spec.Env)
	}

	if err := d.StopProxy(ctx, sb.Ref); err != nil {
		t.Fatalf("StopProxy: %v", err)
	}
	stored := renderedTestConfig(t, "fresh")
	if trustedCAOf(t, stored) != "" {
		t.Fatal("the stored config already carries a CA; the revive leg proves nothing")
	}
	if err := d.ReplaceProxy(ctx, sb.Ref, stored); err != nil {
		t.Fatalf("ReplaceProxy: %v", err)
	}
	if got := trustedCAOf(t, f.stdinOf(name)); got != bundle {
		t.Errorf("revived proxy trusted_ca_pem = %q, want the runner's bundle %q", got, bundle)
	}
}

// withTrustedCA replaces an organisation value (never meant for a runner's proxy) and leaves the
// config alone with no bundle.
func TestWithTrustedCA(t *testing.T) {
	in := []byte(`{"run_token":"tok","trusted_ca_pem":"org-ca-pem"}`)
	out, err := withTrustedCA(in, "runner-ca-pem")
	if err != nil || trustedCAOf(t, out) != "runner-ca-pem" {
		t.Fatalf("withTrustedCA = %s, %v; want the runner's bundle in place of the org's", out, err)
	}
	if out, err := withTrustedCA(in, ""); err != nil || string(out) != string(in) {
		t.Fatalf("withTrustedCA(no bundle) = %s, %v; want the config untouched", out, err)
	}
}
